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
