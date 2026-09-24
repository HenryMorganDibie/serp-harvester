package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
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

// recordingSink keeps every result, for assertions on parsed content.
type recordingSink struct {
	mu      sync.Mutex
	results []*model.SerpResult
}

func (s *recordingSink) Write(r *model.SerpResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
	return nil
}

// TestPool_PlaywrightFetcherEndToEnd runs the unchanged worker pool with the
// Playwright fetcher: jobs flow through the existing proxy pool, rate
// limiter and HTML parser, against a local JS-rendered fixture. Gated like
// the fetcher's browser tests (SERP_HARVESTER_PLAYWRIGHT=1).
func TestPool_PlaywrightFetcherEndToEnd(t *testing.T) {
	if os.Getenv("SERP_HARVESTER_PLAYWRIGHT") != "1" {
		t.Skip("set SERP_HARVESTER_PLAYWRIGHT=1 (with Playwright's Chromium installed) to run browser tests")
	}
	page, err := os.ReadFile("../fetcher/testdata/playwright/js_results.html")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	}))
	defer srv.Close()

	browser := &metrics.BrowserCounters{}
	f, err := fetcher.NewPlaywrightFetcher(fetcher.PlaywrightConfig{
		Endpoint:       srv.URL + "/search",
		Timeout:        15 * time.Second,
		PoolSize:       2,
		Headless:       true,
		ExecutablePath: os.Getenv("SERP_HARVESTER_CHROMIUM_PATH"),
		Metrics:        browser,
	})
	if err != nil {
		t.Fatalf("NewPlaywrightFetcher: %v", err)
	}
	defer f.Close()

	sink := &recordingSink{}
	counters := &metrics.Counters{Browser: browser}
	pool := &Pool{
		Concurrency: 4, // more workers than browser sessions: the pool bounds them
		MaxRetries:  1,
		Fetcher:     f,
		ProxyPool:   proxy.NewPool(nil, 3, time.Second),
		Limiter:     ratelimit.New(1000, 10),
		Parser:      parser.New(),
		Sink:        sink,
		Metrics:     counters,
	}

	queries := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	jobs := make(chan queue.Job, len(queries))
	for _, q := range queries {
		jobs <- queue.Job{Query: q, RunID: "run-pw", Locale: "US-en", Device: "mobile"}
	}
	close(jobs)
	pool.Run(ctx, jobs)

	s := counters.Snapshot()
	if s.Success != uint64(len(queries)) || s.Dropped != 0 {
		t.Fatalf("success=%d dropped=%d failure=%d, want all %d to succeed", s.Success, s.Dropped, s.Failure, len(queries))
	}
	if s.CalibrationFlagged != 0 {
		t.Errorf("calibration flagged %d results; rendered pages should parse cleanly", s.CalibrationFlagged)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, r := range sink.results {
		if len(r.Organic) != 3 || r.ProxyUsed != "direct" || r.RunID != "run-pw" || r.Device != "mobile" || r.Locale != "US-en" {
			t.Errorf("unexpected result: organic=%d proxy=%q run=%q device=%q locale=%q",
				len(r.Organic), r.ProxyUsed, r.RunID, r.Device, r.Locale)
		}
	}
	if b := browser.Snapshot(); b.Launches != 1 || b.SessionsOpen > 2 {
		t.Errorf("browser launches=%d sessions_open=%d, want 1 launch and <= 2 sessions", b.Launches, b.SessionsOpen)
	}
}
