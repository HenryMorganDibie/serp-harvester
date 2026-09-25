// Package integration runs the whole acquisition pipeline against real
// Redis, PostgreSQL and Chromium, with the fictional search site and
// scripted proxies from ./fixture. Nothing contacts a real search engine.
//
// It runs only with INTEGRATION=1 plus REDIS_ADDR, POSTGRES_TEST_DSN and a
// Playwright install (SERP_HARVESTER_PLAYWRIGHT=1). `make integration`
// provides all of them in Docker (deploy/docker-compose.test.yml); they can
// equally be local services.
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
	"github.com/HenryMorganDibie/serp-harvester/internal/proxy"
	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
	"github.com/HenryMorganDibie/serp-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/serp-harvester/internal/store"
	"github.com/HenryMorganDibie/serp-harvester/internal/worker"
	"github.com/HenryMorganDibie/serp-harvester/tests/integration/fixture"
)

func requireIntegration(t *testing.T) (redisAddr, pgDSN string) {
	t.Helper()
	if os.Getenv("INTEGRATION") != "1" {
		t.Skip("set INTEGRATION=1 with REDIS_ADDR, POSTGRES_TEST_DSN and SERP_HARVESTER_PLAYWRIGHT=1 (or run `make integration`)")
	}
	redisAddr, pgDSN = os.Getenv("REDIS_ADDR"), os.Getenv("POSTGRES_TEST_DSN")
	if redisAddr == "" || pgDSN == "" || os.Getenv("SERP_HARVESTER_PLAYWRIGHT") != "1" {
		t.Fatal("INTEGRATION=1 needs REDIS_ADDR, POSTGRES_TEST_DSN and SERP_HARVESTER_PLAYWRIGHT=1")
	}
	return redisAddr, pgDSN
}

// TestPipeline_HybridFailoverOverRealServices pushes jobs through a real
// Redis stream and runs them in hybrid mode across three proxies: one
// that is always CAPTCHA'd, one rate-limited on its first request, and a
// good one. Every page needs JavaScript, so each successful fetch is a
// plain-HTTP attempt that fails over to Chromium. Results must all land in
// PostgreSQL, the CAPTCHA'd proxy must be cooled after its first block,
// and every block and failover must show up in the metrics.
func TestPipeline_HybridFailoverOverRealServices(t *testing.T) {
	redisAddr, pgDSN := requireIntegration(t)

	proxies := map[fixture.Behavior]*fixture.Proxy{}
	var proxyURLs []string
	for _, b := range []fixture.Behavior{fixture.Captcha, fixture.RateLimitOnce, fixture.Good} {
		p := fixture.NewProxy(b)
		srv := httptest.NewServer(p)
		defer srv.Close()
		proxies[b] = p
		proxyURLs = append(proxyURLs, srv.URL)
	}
	endpoint := "http://" + fixture.Host + "/search"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Real Redis stream, unique per run.
	runID := fmt.Sprintf("it-%d", time.Now().UnixNano())
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	src := &queue.RedisStreamSource{Client: rdb, Stream: "serp-harvester:it:" + runID, Group: "it", Consumer: "it-1"}
	defer rdb.Del(context.Background(), src.Stream)
	if err := src.EnsureGroup(ctx); err != nil {
		t.Fatalf("redis: %v", err)
	}
	const jobs = 6
	for i := 0; i < jobs; i++ {
		if err := src.PushJob(ctx, queue.Job{Query: fmt.Sprintf("query %d", i), RunID: runID, Locale: "US-en"}); err != nil {
			t.Fatalf("push: %v", err)
		}
	}

	// Real PostgreSQL sink.
	sink, err := store.NewPostgresSink(ctx, pgDSN)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	defer sink.Close()

	counters := &metrics.Counters{Browser: &metrics.BrowserCounters{}}
	httpFetcher, err := fetcher.NewHTTPFetcher(endpoint, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := fetcher.NewPlaywrightFetcher(fetcher.PlaywrightConfig{
		Endpoint:       endpoint,
		Timeout:        20 * time.Second,
		PoolSize:       2,
		Headless:       true,
		ExecutablePath: os.Getenv("SERP_HARVESTER_CHROMIUM_PATH"),
		WaitSelector:   ".organic-result",
		Metrics:        counters.Browser,
	})
	if err != nil {
		t.Fatalf("playwright: %v", err)
	}
	f := &fetcher.FailoverFetcher{Primary: httpFetcher, Secondary: browser, OnFailover: func(fetcher.Outcome) { counters.IncFailover() }}
	defer f.Close()

	proxyPool := proxy.NewPool(proxyURLs, 3, time.Hour)
	limiter := ratelimit.NewAdaptive(100, 5, 1)
	pool := &worker.Pool{
		Concurrency: 2,
		MaxRetries:  4,
		Fetcher:     f,
		ProxyPool:   proxyPool,
		Limiter:     limiter,
		Parser:      parser.New(),
		Sink:        sink,
		Metrics:     counters,
	}

	// The stream never closes, so stop once every row is in PostgreSQL.
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { pool.Run(runCtx, src.Jobs(runCtx)); close(done) }()
	for {
		n, err := sink.CountByRunID(ctx, runID)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n >= jobs {
			break
		}
		select {
		case <-ctx.Done():
			stop()
			t.Fatalf("only %d/%d rows after timeout; outcomes=%v", n, jobs, counters.Outcomes())
		case <-time.After(200 * time.Millisecond):
		}
	}
	stop()
	<-done

	s := counters.Snapshot()
	t.Logf("outcomes=%v cooldowns=%v failovers=%d browser=%+v", counters.Outcomes(), counters.Cooldowns(), counters.Failovers, counters.Browser.Snapshot())
	if s.Success != jobs || s.CalibrationFlagged != 0 {
		t.Errorf("success=%d calibration_flagged=%d, want %d rendered, fully parsed results", s.Success, s.CalibrationFlagged, jobs)
	}
	if got := proxies[fixture.Captcha].Requests.Load(); got != 1 {
		t.Errorf("CAPTCHA'd proxy got %d requests; it should be cooled after its first block", got)
	}
	if got := proxies[fixture.RateLimitOnce].Requests.Load(); got < 1 {
		t.Errorf("rate-limited proxy got %d requests", got)
	}
	if counters.Failovers < jobs {
		t.Errorf("failovers = %d; every page needs JS, so each success should come via the browser", counters.Failovers)
	}
	outcomes := map[string]uint64{}
	for _, v := range counters.Outcomes() {
		outcomes[v.Label] = v.Value
	}
	if outcomes["captcha"] < 1 || outcomes["rate_limited"] < 1 || outcomes["success"] != jobs {
		t.Errorf("outcomes = %v", outcomes)
	}
	if limiter.Rate(proxyURLs[0]) >= 100 || limiter.Rate(proxyURLs[1]) >= 100 {
		t.Errorf("blocked and rate-limited proxies should be throttled: rates %.1f, %.1f", limiter.Rate(proxyURLs[0]), limiter.Rate(proxyURLs[1]))
	}
	if cooling := counters.Cooldowns(); len(cooling) == 0 {
		t.Error("expected proxy cooldowns to be recorded")
	}

	// Rows really are in PostgreSQL, parsed.
	db, err := sql.Open("pgx", pgDSN) // driver registered by internal/store
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var organic int
	row := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(jsonb_array_length(organic)),0) FROM serp_results WHERE run_id = $1`, runID)
	if err := row.Scan(&organic); err != nil {
		t.Fatal(err)
	}
	if organic != jobs*3 {
		t.Errorf("stored organic results = %d, want %d", organic, jobs*3)
	}
	var proxiesUsed string
	db.QueryRowContext(ctx, `SELECT string_agg(DISTINCT proxy_used, ',') FROM serp_results WHERE run_id = $1`, runID).Scan(&proxiesUsed)
	if strings.Contains(proxiesUsed, proxyURLs[0]) {
		t.Errorf("no result should have come through the CAPTCHA'd proxy, got %s", proxiesUsed)
	}
}
