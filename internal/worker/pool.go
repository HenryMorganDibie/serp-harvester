// Package worker runs a pool of concurrent goroutines that pull jobs off a
// queue, fetch, parse, and sink the result — with per-proxy rate limiting,
// retry with backoff, and proxy health tracking wired in on every request.
package worker

import (
	"context"
	"log"
	"math/rand"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
	"github.com/HenryMorganDibie/serp-harvester/internal/proxy"
	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
	"github.com/HenryMorganDibie/serp-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/serp-harvester/internal/store"
)

// userAgents is a small, honest rotation pool. This is standard practice for
// identifying different concurrent clients — not fingerprint spoofing.
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) serp-harvester-sample/0.1",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) serp-harvester-sample/0.1",
	"Mozilla/5.0 (X11; Linux x86_64) serp-harvester-sample/0.1",
}

// Pool wires together every stage of the pipeline.
type Pool struct {
	Concurrency int
	MaxRetries  int

	Fetcher   fetcher.Fetcher
	ProxyPool *proxy.Pool
	Limiter   *ratelimit.Limiter
	Parser    *parser.Parser
	Sink      store.Sink
	Metrics   *metrics.Counters
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

	for attempt := 0; attempt <= p.MaxRetries; attempt++ {
		if attempt > 0 {
			p.Metrics.IncRetried()
			backoff(attempt)
		}

		px, err := p.ProxyPool.Next()
		if err != nil {
			lastErr = err
			continue
		}

		if err := p.Limiter.Wait(ctx, px.Key()); err != nil {
			lastErr = err
			return // context cancelled
		}

		req := fetcher.Request{
			Query:     job.Query,
			UserAgent: userAgents[rand.Intn(len(userAgents))],
			ProxyURL:  px.URL,
		}

		resp, err := p.Fetcher.Fetch(ctx, req)
		p.ProxyPool.ReportResult(px, err)
		if err != nil {
			lastErr = err
			p.Metrics.IncFailure()
			continue
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
		return
	}

	p.Metrics.IncDropped()
	log.Printf("worker: job %q dropped after %d attempts: %v", job.Query, p.MaxRetries+1, lastErr)
}

// backoff sleeps for an exponential delay with jitter, capped at 2s. This is
// deliberately short since the sample's mock mode should stay fast; a live
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
