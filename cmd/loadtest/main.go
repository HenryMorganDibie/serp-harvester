// Command loadtest measures the pipeline's own orchestration overhead:
// queue -> worker pool -> proxy pool -> rate limiter -> mock fetch -> parse
// -> discard sink, at high concurrency, with the per-proxy rate limit
// effectively disabled.
//
// This measures how fast this codebase's plumbing can move jobs through
// itself, NOT real network throughput against Google or any other live
// target — the mock fetcher never touches the network. Treat every number
// this prints as an upper bound on orchestration overhead, evidence the
// pipeline doesn't bottleneck on its own machinery before a proxy budget or
// target site would, never as a live scraping throughput claim.
//
// Three modes:
//
//	-n           fixed-count benchmark at one concurrency level (default)
//	-sweep       fixed-count benchmark run at several concurrency levels in
//	             sequence, printing one comparison table
//	-duration    runs continuously for a wall-clock duration instead of a
//	             fixed count — a soak test. Periodically reports throughput,
//	             latency percentiles, memory, and goroutine count so a
//	             sustained run can be checked for leaks or degradation, not
//	             just "it didn't crash."
package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/HenryMorganDibie/web-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/web-harvester/internal/metrics"
	"github.com/HenryMorganDibie/web-harvester/internal/model"
	"github.com/HenryMorganDibie/web-harvester/internal/parser"
	"github.com/HenryMorganDibie/web-harvester/internal/proxy"
	"github.com/HenryMorganDibie/web-harvester/internal/queue"
	"github.com/HenryMorganDibie/web-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/web-harvester/internal/worker"
)

// discardSink implements store.Sink without doing any I/O, so the
// measurement reflects pipeline overhead, not disk/stdout speed.
type discardSink struct{}

func (discardSink) Write(*model.SerpResult) error { return nil }

func main() {
	n := flag.Int("n", 50000, "number of synthetic queries to process (fixed-count mode)")
	concurrency := flag.Int("concurrency", 200, "worker pool concurrency")
	fixtureDir := flag.String("fixtures", "internal/parser/testdata", "mock fixture directory")
	sweep := flag.String("sweep", "", "comma-separated concurrency levels to benchmark in sequence, e.g. 1,4,8,16,50,200,800")
	duration := flag.String("duration", "", "run continuously for this duration instead of a fixed count, e.g. 90m (soak test mode)")
	sampleInterval := flag.String("sample-interval", "1m", "progress reporting interval in duration mode")
	flag.Parse()

	f, err := fetcher.NewMockFromDir(*fixtureDir)
	if err != nil {
		panic(err)
	}

	switch {
	case *duration != "":
		d, err := time.ParseDuration(*duration)
		if err != nil {
			panic(fmt.Sprintf("invalid -duration: %v", err))
		}
		interval, err := time.ParseDuration(*sampleInterval)
		if err != nil {
			panic(fmt.Sprintf("invalid -sample-interval: %v", err))
		}
		runSoak(f, *concurrency, d, interval)

	case *sweep != "":
		levels, err := parseIntList(*sweep)
		if err != nil {
			panic(fmt.Sprintf("invalid -sweep: %v", err))
		}
		runSweep(f, levels, *n)

	default:
		result := runFixed(f, *concurrency, *n)
		printResult(result)
	}
}

func parseIntList(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("%q: %w", p, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// runResult is one fixed-count or soak run's outcome.
type runResult struct {
	Concurrency int
	N           uint64
	Elapsed     time.Duration
	Metrics     metrics.Snapshot
	Latency     metrics.LatencyStats
	RPS         float64
}

func buildPool(f fetcher.Fetcher, concurrency int, counters *metrics.Counters, latencies *metrics.LatencyRecorder) *worker.Pool {
	return &worker.Pool{
		Concurrency: concurrency,
		MaxRetries:  1,
		Fetcher:     f,
		ProxyPool:   proxy.NewPool(nil, 1000000, time.Millisecond), // effectively never bans
		Limiter:     ratelimit.New(1_000_000, 1_000_000),           // effectively unthrottled
		Parser:      parser.New(),
		Sink:        discardSink{},
		Metrics:     counters,
		Latencies:   latencies,
	}
}

// runFixed processes exactly n synthetic queries at the given concurrency.
func runFixed(f fetcher.Fetcher, concurrency, n int) runResult {
	counters := &metrics.Counters{}
	latencies := metrics.NewLatencyRecorder(200000)
	pool := buildPool(f, concurrency, counters, latencies)

	queries := make([]string, n)
	for i := range queries {
		queries[i] = fmt.Sprintf("synthetic query %d", i)
	}

	ctx := context.Background()
	jobs := queue.New(queries, concurrency*2)

	start := time.Now()
	pool.Run(ctx, jobs)
	elapsed := time.Since(start)

	snap := counters.Snapshot()
	rps := float64(snap.Success) / elapsed.Seconds()

	return runResult{
		Concurrency: concurrency,
		N:           snap.Success,
		Elapsed:     elapsed,
		Metrics:     snap,
		Latency:     latencies.Stats(),
		RPS:         rps,
	}
}

func printResult(r runResult) {
	fmt.Printf("load test: n=%d concurrency=%d (mock fetcher, no network)\n", r.N, r.Concurrency)
	fmt.Printf("done in %s\n", r.Elapsed.Round(time.Millisecond))
	fmt.Printf("success=%d failure=%d dropped=%d retried=%d\n", r.Metrics.Success, r.Metrics.Failure, r.Metrics.Dropped, r.Metrics.Retried)
	fmt.Printf("latency: min=%s p50=%s p95=%s p99=%s max=%s (n=%d sampled)\n",
		r.Latency.Min.Round(time.Millisecond), r.Latency.P50.Round(time.Millisecond),
		r.Latency.P95.Round(time.Millisecond), r.Latency.P99.Round(time.Millisecond),
		r.Latency.Max.Round(time.Millisecond), r.Latency.Count)
	fmt.Printf("throughput: %.0f req/s (orchestration overhead only, not live network) => %.0f/day if sustained\n", r.RPS, r.RPS*86400)
}

// runSweep runs a fixed-count benchmark at each concurrency level in
// sequence and prints one comparison table — a reproducible way to answer
// "how does throughput scale with concurrency" with real, not invented,
// numbers.
func runSweep(f fetcher.Fetcher, levels []int, n int) {
	fmt.Printf("concurrency sweep: n=%d per level (mock fetcher, no network)\n\n", n)
	fmt.Println("| Concurrency | Success | Failed | Dropped | Wall time | Throughput (req/s) | p50 | p95 | p99 |")
	fmt.Println("|-------------|---------|--------|---------|-----------|---------------------|-----|-----|-----|")

	for _, c := range levels {
		r := runFixed(f, c, n)
		fmt.Printf(
			"| %d | %d | %d | %d | %s | %.0f | %s | %s | %s |\n",
			r.Concurrency, r.Metrics.Success, r.Metrics.Failure, r.Metrics.Dropped,
			r.Elapsed.Round(time.Millisecond), r.RPS,
			r.Latency.P50.Round(time.Millisecond), r.Latency.P95.Round(time.Millisecond), r.Latency.P99.Round(time.Millisecond),
		)
	}
}

// runSoak runs continuously for d, feeding synthetically-generated queries
// with no fixed count, reporting progress every interval: throughput,
// latency percentiles, memory, and goroutine count. This is a stability
// check (no crash, no unbounded memory/goroutine growth, consistent
// throughput over the run), not a live-network benchmark.
func runSoak(f fetcher.Fetcher, concurrency int, d, interval time.Duration) {
	counters := &metrics.Counters{}
	latencies := metrics.NewLatencyRecorder(200000)
	pool := buildPool(f, concurrency, counters, latencies)

	fmt.Printf("soak test starting: duration=%s concurrency=%d sample_interval=%s (mock fetcher, no network)\n", d, concurrency, interval)
	fmt.Println("this measures orchestration stability over time (memory, goroutines, throughput consistency), not live network throughput.")

	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	jobs := infiniteJobs(ctx, concurrency*2)

	startedAt := time.Now()
	stopSampling := make(chan struct{})
	go sampleLoop(ctx, startedAt, interval, counters, latencies, stopSampling)

	pool.Run(ctx, jobs)
	close(stopSampling)

	elapsed := time.Since(startedAt)
	snap := counters.Snapshot()
	rps := float64(snap.Success) / elapsed.Seconds()

	fmt.Println("\n=== soak test complete ===")
	fmt.Printf("actual elapsed: %s (requested: %s)\n", elapsed.Round(time.Second), d)
	fmt.Printf("total success=%d failure=%d dropped=%d retried=%d\n", snap.Success, snap.Failure, snap.Dropped, snap.Retried)
	fmt.Printf("ai_overview=%d calibration_flagged=%d proxy_banned=%d\n", snap.AIOverviewPresent, snap.CalibrationFlagged, snap.ProxyBanned)
	ls := latencies.Stats()
	fmt.Printf("latency (last %d samples): min=%s p50=%s p95=%s p99=%s max=%s\n",
		ls.Count, ls.Min.Round(time.Millisecond), ls.P50.Round(time.Millisecond),
		ls.P95.Round(time.Millisecond), ls.P99.Round(time.Millisecond), ls.Max.Round(time.Millisecond))
	fmt.Printf("average throughput: %.0f req/s => %.0f/day if sustained\n", rps, rps*86400)

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fmt.Printf("final memory: alloc=%dMB sys=%dMB goroutines=%d\n", mem.Alloc/1024/1024, mem.Sys/1024/1024, runtime.NumGoroutine())
	fmt.Println("compare this against the first sample line above: stable memory/goroutines across the run is the actual soak-test evidence, not just that it finished.")
}

func sampleLoop(ctx context.Context, startedAt time.Time, interval time.Duration, counters *metrics.Counters, latencies *metrics.LatencyRecorder, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastSuccess uint64
	lastSampleAt := startedAt

	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			snap := counters.Snapshot()
			intervalElapsed := now.Sub(lastSampleAt).Seconds()
			intervalRPS := float64(0)
			if intervalElapsed > 0 {
				intervalRPS = float64(snap.Success-lastSuccess) / intervalElapsed
			}
			lastSuccess = snap.Success
			lastSampleAt = now

			ls := latencies.Stats()
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)

			fmt.Printf(
				"[soak %s] success=%d failure=%d dropped=%d interval_rps=%.0f p50=%s p95=%s p99=%s alloc=%dMB goroutines=%d\n",
				now.Sub(startedAt).Round(time.Second), snap.Success, snap.Failure, snap.Dropped, intervalRPS,
				ls.P50.Round(time.Millisecond), ls.P95.Round(time.Millisecond), ls.P99.Round(time.Millisecond),
				mem.Alloc/1024/1024, runtime.NumGoroutine(),
			)
		}
	}
}

// infiniteJobs feeds synthetically-generated queries until ctx is done, for
// duration-mode (soak) runs where a fixed query list isn't the point.
func infiniteJobs(ctx context.Context, buffer int) <-chan queue.Job {
	ch := make(chan queue.Job, buffer)
	go func() {
		defer close(ch)
		var i uint64
		for {
			select {
			case <-ctx.Done():
				return
			case ch <- queue.Job{Query: fmt.Sprintf("soak query %d", i)}:
				i++
			}
		}
	}()
	return ch
}
