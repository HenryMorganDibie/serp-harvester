// Command loadtest measures the pipeline's own orchestration overhead:
// queue -> worker pool -> proxy pool -> rate limiter -> mock fetch -> parse
// -> discard sink, at high concurrency, with the per-proxy rate limit
// effectively disabled.
//
// This measures how fast this codebase's plumbing can move jobs through
// itself, NOT real network throughput against Google or any other live
// target — the mock fetcher never touches the network. Treat the number
// this prints as an upper bound on orchestration overhead, evidence that
// the pipeline doesn't bottleneck on its own machinery before a proxy
// budget and target site would, not as a live scraping throughput claim.
package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/model"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
	"github.com/HenryMorganDibie/serp-harvester/internal/proxy"
	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
	"github.com/HenryMorganDibie/serp-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/serp-harvester/internal/worker"
)

// discardSink implements store.Sink without doing any I/O, so the
// measurement reflects pipeline overhead, not disk/stdout speed.
type discardSink struct{}

func (discardSink) Write(*model.SerpResult) error { return nil }

func main() {
	n := flag.Int("n", 50000, "number of synthetic queries to process")
	concurrency := flag.Int("concurrency", 200, "worker pool concurrency")
	fixtureDir := flag.String("fixtures", "internal/parser/testdata", "mock fixture directory")
	flag.Parse()

	f, err := fetcher.NewMockFromDir(*fixtureDir)
	if err != nil {
		panic(err)
	}

	counters := &metrics.Counters{}
	pool := &worker.Pool{
		Concurrency: *concurrency,
		MaxRetries:  1,
		Fetcher:     f,
		ProxyPool:   proxy.NewPool(nil, 1000000, time.Millisecond), // effectively never bans
		Limiter:     ratelimit.New(1_000_000, 1_000_000),           // effectively unthrottled
		Parser:      parser.New(),
		Sink:        discardSink{},
		Metrics:     counters,
	}

	queries := make([]string, *n)
	for i := range queries {
		queries[i] = fmt.Sprintf("synthetic query %d", i)
	}

	fmt.Printf("load test: n=%d concurrency=%d fixtures=%s (mock fetcher, no network)\n", *n, *concurrency, *fixtureDir)

	ctx := context.Background()
	jobs := queue.New(queries, *concurrency*2)

	start := time.Now()
	pool.Run(ctx, jobs)
	elapsed := time.Since(start)

	snap := counters.Snapshot()
	rps := float64(snap.Success) / elapsed.Seconds()

	fmt.Printf("done in %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("success=%d failure=%d dropped=%d retried=%d\n", snap.Success, snap.Failure, snap.Dropped, snap.Retried)
	fmt.Printf("throughput: %.0f req/s (orchestration overhead only, not live network) => %.0f/day if sustained\n", rps, rps*86400)
}
