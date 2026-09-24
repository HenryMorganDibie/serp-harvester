package worker

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/model"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
	"github.com/HenryMorganDibie/serp-harvester/internal/proxy"
	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
	"github.com/HenryMorganDibie/serp-harvester/internal/ratelimit"
)

// countingSink is a store.Sink that just counts writes, so the test doesn't
// depend on the filesystem.
type countingSink struct {
	mu    sync.Mutex
	count int
}

func (s *countingSink) Write(*model.SerpResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	return nil
}

func (s *countingSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func TestPool_ProcessesAllJobsUnderConcurrency(t *testing.T) {
	const n = 2000

	f, err := fetcher.NewMockFromDir("../parser/testdata")
	if err != nil {
		t.Fatalf("build mock fetcher: %v", err)
	}

	sink := &countingSink{}
	counters := &metrics.Counters{}

	pool := &Pool{
		Concurrency: 32,
		MaxRetries:  1,
		Fetcher:     f,
		ProxyPool:   proxy.NewPool(nil, 3, time.Second),
		Limiter:     ratelimit.New(100000, 1000), // effectively unthrottled for this test
		Parser:      parser.New(),
		Sink:        sink,
		Metrics:     counters,
	}

	queries := make([]string, n)
	for i := range queries {
		queries[i] = fmt.Sprintf("query %d", i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs := queue.New(queries, pool.Concurrency*2)
	pool.Run(ctx, jobs)

	snap := counters.Snapshot()
	if snap.Success != n {
		t.Errorf("expected %d successes, got %d (failures=%d dropped=%d)", n, snap.Success, snap.Failure, snap.Dropped)
	}
	if sink.Count() != n {
		t.Errorf("expected sink to receive %d writes, got %d", n, sink.Count())
	}
	if snap.Dropped != 0 {
		t.Errorf("expected 0 dropped jobs, got %d", snap.Dropped)
	}
}

// jsonParserAdapter proves worker.Parser is satisfied by the JSON provider
// parser too, not just the HTML one — the pool never needs to know which.
func TestPool_AcceptsJSONParser(t *testing.T) {
	var _ Parser = (*parser.Parser)(nil)
	var _ Parser = (*parser.JSONParser)(nil)
}

// rateLimitOnceFetcher fails its first call with a RateLimitError carrying
// RetryAfter, then succeeds on every call after — enough to observe whether
// the pool actually waited out RetryAfter before retrying.
type rateLimitOnceFetcher struct {
	retryAfter time.Duration
	calls      int
	mu         sync.Mutex
	body       []byte
}

func (f *rateLimitOnceFetcher) Fetch(ctx context.Context, req fetcher.Request) (*fetcher.Response, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	if call == 1 {
		return nil, &fetcher.RateLimitError{StatusCode: 429, RetryAfter: f.retryAfter}
	}
	return &fetcher.Response{StatusCode: 200, Body: f.body}, nil
}

func TestPool_HonorsRateLimitRetryAfter(t *testing.T) {
	fixture, err := fetcherFixture(t)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	f := &rateLimitOnceFetcher{retryAfter: 300 * time.Millisecond, body: fixture}
	counters := &metrics.Counters{}

	pool := &Pool{
		Concurrency: 1,
		MaxRetries:  1,
		Fetcher:     f,
		ProxyPool:   proxy.NewPool(nil, 1000, time.Hour), // never bans within this test
		Limiter:     ratelimit.New(100000, 1000),
		Parser:      parser.New(),
		Sink:        &countingSink{},
		Metrics:     counters,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	pool.Run(ctx, queue.New([]string{"one query"}, 1))
	elapsed := time.Since(start)

	if f.calls != 2 {
		t.Fatalf("expected exactly 2 fetch attempts (1 rate-limited, 1 success), got %d", f.calls)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("expected the pool to wait out the ~300ms RetryAfter before retrying, only took %s", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("retry took suspiciously long (%s) — may have fallen back to a longer default backoff instead of RetryAfter", elapsed)
	}

	snap := counters.Snapshot()
	if snap.Success != 1 {
		t.Errorf("expected 1 success after the retry, got %d", snap.Success)
	}
}

func fetcherFixture(t *testing.T) ([]byte, error) {
	t.Helper()
	return []byte(`<html><body><div class="serp-organic-results"><div class="organic-result" data-position="1"><a class="organic-link" href="https://example.com/1"><span class="organic-title">t</span></a><div class="organic-snippet">s</div></div></div></body></html>`), nil
}
