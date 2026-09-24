// Package worker runs a pool of concurrent goroutines that pull jobs off a
// queue, fetch, parse, and sink the result — with per-proxy rate limiting,
// retry with backoff, and proxy health tracking wired in on every request.
package worker

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/model"
	"github.com/HenryMorganDibie/serp-harvester/internal/proxy"
	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
	"github.com/HenryMorganDibie/serp-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/serp-harvester/internal/store"
)

// Parser extracts a SerpResult from a fetcher's raw response body. Both the
// HTML parser (internal/parser.Parser) and the JSON parser for structured
// third-party provider responses (internal/parser.JSONParser) satisfy this,
// so the worker pool never needs to know which fetch path is active.
type Parser interface {
	Parse(query string, body []byte) (*model.SerpResult, error)
}

// userAgents is a small, honest rotation pool. This is standard practice for
// identifying different concurrent clients — not fingerprint spoofing.
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) serp-harvester/0.1",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) serp-harvester/0.1",
	"Mozilla/5.0 (X11; Linux x86_64) serp-harvester/0.1",
}

// Pool wires together every stage of the pipeline.
type Pool struct {
	Concurrency int
	MaxRetries  int

	Fetcher   fetcher.Fetcher
	ProxyPool *proxy.Pool
	Limiter   *ratelimit.Limiter
	Parser    Parser
	Sink      store.Sink
	Metrics   *metrics.Counters

	// Latencies, if set, records fetch latency for every successful fetch —
	// used for percentile reporting (see cmd/loadtest). Nil is safe: no
	// latency tracking occurs.
	Latencies *metrics.LatencyRecorder
}

// Run starts Concurrency workers consuming jobs, and blocks until jobs is
// closed and every in-flight job has been processed.
func (p *Pool) Run(ctx context.Context, jobs <-chan queue.Job) {
	done := make(chan struct{})
	for i := 0; i < p.Concurrency; i++ {
		go func() {
			p.runWorker(ctx, jobs)
			done <- struct{}{}
		}()
	}
	for i := 0; i < p.Concurrency; i++ {
		<-done
	}
}

func (p *Pool) runWorker(ctx context.Context, jobs <-chan queue.Job) {
	for {
		select {
		case job, ok := <-jobs:
			if !ok {
				return
			}
			p.process(ctx, job)
		case <-ctx.Done():
			return
		}
	}
}

func (p *Pool) process(ctx context.Context, job queue.Job) {
	var lastErr error
	var rateLimitDelay time.Duration // set when the target tells us how long to wait, honored instead of generic backoff

	for attempt := 0; attempt <= p.MaxRetries; attempt++ {
		if attempt > 0 {
			p.Metrics.IncRetried()
			if rateLimitDelay > 0 {
				select {
				case <-time.After(rateLimitDelay):
				case <-ctx.Done():
					p.Metrics.IncDropped()
					log.Printf("worker: job %q dropped waiting out rate limit: %v", job.Query, ctx.Err())
					return
				}
				rateLimitDelay = 0
			} else {
				backoff(attempt)
			}
		}

		px, err := p.ProxyPool.Next()
		if err != nil {
			lastErr = err
			continue
		}

		if err := p.Limiter.Wait(ctx, px.Key()); err != nil {
			// Context cancelled (shutdown/timeout) while waiting for a rate
			// limit slot. Counted as dropped rather than silently
			// unaccounted for, so success+dropped stays a complete count of
			// every job actually attempted — found via soak testing, where
			// jobs in flight at the exact cutoff need to land somewhere.
			p.Metrics.IncDropped()
			log.Printf("worker: job %q dropped: %v", job.Query, err)
			return
		}

		req := fetcher.Request{
			Query:     job.Query,
			UserAgent: userAgents[rand.Intn(len(userAgents))],
			ProxyURL:  px.URL,
		}

		resp, err := p.Fetcher.Fetch(ctx, req)
		if newlyBanned := p.ProxyPool.ReportResult(px, err); newlyBanned {
			p.Metrics.IncProxyBanned()
		}
		if err != nil {
			lastErr = err
			p.Metrics.IncFailure()
			var rlErr *fetcher.RateLimitError
			if errors.As(err, &rlErr) && rlErr.RetryAfter > 0 {
				rateLimitDelay = rlErr.RetryAfter
			}
			continue
		}
		if p.Latencies != nil {
			p.Latencies.Record(resp.Latency)
		}

		result, err := p.Parser.Parse(job.Query, resp.Body)
		if err != nil {
			lastErr = err
			p.Metrics.IncFailure()
			continue
		}
		result.LatencyMS = resp.Latency.Milliseconds()
		result.ProxyUsed = px.Key()
		result.FetchedAt = time.Now().UTC()

		if err := p.Sink.Write(result); err != nil {
			log.Printf("worker: sink write failed for %q: %v", job.Query, err)
		}
		p.Metrics.IncSuccess()
		if result.AIOverview != nil {
			p.Metrics.IncAIOverviewPresent()
		}
		if result.Calibration != nil {
			p.Metrics.IncCalibrationFlagged()
		}
		return
	}

	p.Metrics.IncDropped()
	log.Printf("worker: job %q dropped after %d attempts: %v", job.Query, p.MaxRetries+1, lastErr)
}

// backoff sleeps for an exponential delay with jitter, capped at 2s. This is
// deliberately short since mock-mode runs should stay fast; a live
// production deployment would use longer, configurable backoff tied to the
// client's agreed request budget.
func backoff(attempt int) {
	base := time.Duration(1<<uint(attempt)) * 50 * time.Millisecond
	if base > 2*time.Second {
		base = 2 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(base) / 2))
	time.Sleep(base + jitter)
}
