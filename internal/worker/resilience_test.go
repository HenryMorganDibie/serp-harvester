package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/HenryMorganDibie/web-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/web-harvester/internal/metrics"
	"github.com/HenryMorganDibie/web-harvester/internal/parser"
	"github.com/HenryMorganDibie/web-harvester/internal/proxy"
	"github.com/HenryMorganDibie/web-harvester/internal/queue"
	"github.com/HenryMorganDibie/web-harvester/internal/ratelimit"
)

// perProxyFetcher answers according to which proxy the request used, and
// records every call, so tests can observe failover and cooldown choices.
type perProxyFetcher struct {
	mu     sync.Mutex
	errFor map[string]func(call int) error // proxy URL -> error for its nth call (1-based); nil = success
	calls  map[string]int
	body   []byte
}

func newPerProxyFetcher(t *testing.T, errFor map[string]func(int) error) *perProxyFetcher {
	body, _ := fetcherFixture(t)
	return &perProxyFetcher{errFor: errFor, calls: map[string]int{}, body: body}
}

func (f *perProxyFetcher) Fetch(ctx context.Context, req fetcher.Request) (*fetcher.Response, error) {
	f.mu.Lock()
	f.calls[req.ProxyURL]++
	n := f.calls[req.ProxyURL]
	fn := f.errFor[req.ProxyURL]
	f.mu.Unlock()
	if fn != nil {
		if err := fn(n); err != nil {
			return nil, err
		}
	}
	return &fetcher.Response{StatusCode: 200, Body: f.body}, nil
}

func (f *perProxyFetcher) callsTo(proxyURL string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[proxyURL]
}

func always(err error) func(int) error { return func(int) error { return err } }

func runJobs(t *testing.T, pool *Pool, n int, timeout time.Duration) time.Duration {
	t.Helper()
	queries := make([]string, n)
	for i := range queries {
		queries[i] = "q"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	pool.Run(ctx, queue.New(queries, n))
	return time.Since(start)
}

func outcomeCount(c *metrics.Counters, label string) uint64 {
	for _, v := range c.Outcomes() {
		if v.Label == label {
			return v.Value
		}
	}
	return 0
}

func cooldownCount(c *metrics.Counters, label string) uint64 {
	for _, v := range c.Cooldowns() {
		if v.Label == label {
			return v.Value
		}
	}
	return 0
}

func newResiliencePool(f fetcher.Fetcher, proxies []string, limiter *ratelimit.Limiter) (*Pool, *metrics.Counters, *proxy.Pool) {
	counters := &metrics.Counters{}
	pp := proxy.NewPool(proxies, 3, time.Hour)
	return &Pool{
		Concurrency: 1,
		MaxRetries:  2,
		Fetcher:     f,
		ProxyPool:   pp,
		Limiter:     limiter,
		Parser:      parser.New(),
		Sink:        &countingSink{},
		Metrics:     counters,
	}, counters, pp
}

func TestPool_RateLimitFailsOverToAnotherProxyWithoutWaiting(t *testing.T) {
	const p1, p2 = "http://p1:1", "http://p2:1"
	f := newPerProxyFetcher(t, map[string]func(int) error{
		p1: always(&fetcher.RateLimitError{StatusCode: 429, RetryAfter: 30 * time.Second}),
	})
	limiter := ratelimit.NewAdaptive(1000, 10, 0)
	pool, counters, pp := newResiliencePool(f, []string{p1, p2}, limiter)

	elapsed := runJobs(t, pool, 1, 5*time.Second)

	if s := counters.Snapshot(); s.Success != 1 || s.Dropped != 0 {
		t.Fatalf("success=%d dropped=%d", s.Success, s.Dropped)
	}
	if elapsed > time.Second {
		t.Errorf("took %s: a rate-limited proxy should fail over to the other one, not wait out Retry-After", elapsed)
	}
	if f.callsTo(p1) != 1 || f.callsTo(p2) != 1 {
		t.Errorf("calls p1=%d p2=%d, want 1 each", f.callsTo(p1), f.callsTo(p2))
	}
	if a, c := pp.Counts(); a != 1 || c != 1 {
		t.Errorf("p1 should be cooling for its Retry-After: available=%d cooling=%d", a, c)
	}
	if limiter.Rate(p1) >= 1000 || counters.RateDecreases != 1 {
		t.Errorf("p1's rate should have been halved: rate=%v decreases=%d", limiter.Rate(p1), counters.RateDecreases)
	}
	if outcomeCount(counters, "rate_limited") != 1 || outcomeCount(counters, "success") != 1 || cooldownCount(counters, "rate_limited") != 1 {
		t.Errorf("outcomes=%v cooldowns=%v", counters.Outcomes(), counters.Cooldowns())
	}
}

func TestPool_CaptchaCoolsProxyAndJobsRouteAroundIt(t *testing.T) {
	const p1, p2 = "http://p1:1", "http://p2:1"
	f := newPerProxyFetcher(t, map[string]func(int) error{
		p1: always(&fetcher.BlockedError{Reason: "captcha", StatusCode: 429}),
	})
	pool, counters, _ := newResiliencePool(f, []string{p1, p2}, ratelimit.New(1000, 10))

	runJobs(t, pool, 6, 5*time.Second)

	if s := counters.Snapshot(); s.Success != 6 || s.Dropped != 0 || s.ProxyBanned != 1 {
		t.Fatalf("success=%d dropped=%d proxy_banned=%d; want 6, 0, 1", s.Success, s.Dropped, s.ProxyBanned)
	}
	if f.callsTo(p1) != 1 {
		t.Errorf("the CAPTCHA'd proxy was used %d times; it should cool after the first block", f.callsTo(p1))
	}
	if cooldownCount(counters, "blocked") != 1 || outcomeCount(counters, "captcha") != 1 {
		t.Errorf("outcomes=%v cooldowns=%v", counters.Outcomes(), counters.Cooldowns())
	}
}

func TestPool_ClientErrorIsNotRetried(t *testing.T) {
	f := newPerProxyFetcher(t, map[string]func(int) error{"": always(&fetcher.StatusError{StatusCode: 404})})
	pool, counters, pp := newResiliencePool(f, nil, ratelimit.New(1000, 10))
	pool.MaxRetries = 5

	runJobs(t, pool, 1, 5*time.Second)

	if f.callsTo("") != 1 {
		t.Errorf("404 was attempted %d times; retrying cannot fix it", f.callsTo(""))
	}
	if s := counters.Snapshot(); s.Dropped != 1 || s.Retried != 0 {
		t.Errorf("dropped=%d retried=%d", s.Dropped, s.Retried)
	}
	if a, _ := pp.Counts(); a != 1 {
		t.Error("a client error says nothing about the proxy and must not cool it")
	}
}

func TestPool_ConsentWallDoesNotPenalizeProxy(t *testing.T) {
	f := newPerProxyFetcher(t, map[string]func(int) error{
		"": func(n int) error {
			if n == 1 {
				return &fetcher.BlockedError{Reason: "consent"}
			}
			return nil
		},
	})
	pool, counters, _ := newResiliencePool(f, nil, ratelimit.New(1000, 10))
	pp2 := proxy.NewPool(nil, 1, time.Hour) // would ban on the first failure
	pool.ProxyPool = pp2

	runJobs(t, pool, 1, 5*time.Second)

	if counters.Snapshot().Success != 1 {
		t.Fatal("expected the retry to succeed")
	}
	if a, _ := pp2.Counts(); a != 1 || counters.Snapshot().ProxyBanned != 0 {
		t.Error("a consent wall must not cool the proxy")
	}
}

func TestPool_WaitsForCooldownWhenEveryProxyIsCooling(t *testing.T) {
	f := newPerProxyFetcher(t, map[string]func(int) error{
		"": func(n int) error {
			if n == 1 {
				return &fetcher.BlockedError{Reason: "interstitial"}
			}
			return nil
		},
	})
	pool, counters, _ := newResiliencePool(f, nil, ratelimit.New(1000, 10))
	pool.ProxyPool = proxy.NewPool(nil, 3, 250*time.Millisecond)

	elapsed := runJobs(t, pool, 1, 5*time.Second)

	if counters.Snapshot().Success != 1 {
		t.Fatalf("expected success after the cooldown, got %+v", counters.Snapshot())
	}
	if elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("took %s; should wait out the 250ms cooldown of the only proxy, then retry", elapsed)
	}
}

func TestPool_MaxProxyWaitBoundsTheWait(t *testing.T) {
	f := newPerProxyFetcher(t, map[string]func(int) error{"": always(&fetcher.BlockedError{Reason: "captcha"})})
	pool, counters, _ := newResiliencePool(f, nil, ratelimit.New(1000, 10))
	pool.MaxProxyWait = 100 * time.Millisecond // the proxy cools for an hour

	elapsed := runJobs(t, pool, 1, 10*time.Second)

	if s := counters.Snapshot(); s.Dropped != 1 || f.callsTo("") != 1 {
		t.Errorf("dropped=%d calls=%d; want the job dropped after one attempt", s.Dropped, f.callsTo(""))
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %s; MaxProxyWait should stop the wait for an hour-long cooldown", elapsed)
	}
}

func TestPool_ShutdownDuringBackoffDropsPromptly(t *testing.T) {
	f := newPerProxyFetcher(t, map[string]func(int) error{"": always(&fetcher.StatusError{StatusCode: 503})})
	pool, counters, _ := newResiliencePool(f, nil, ratelimit.New(1000, 10))
	pool.ProxyPool = proxy.NewPool(nil, 1000, time.Hour)
	pool.MaxRetries = 20 // backoff would reach 2s per attempt

	elapsed := runJobs(t, pool, 1, 400*time.Millisecond)

	if elapsed > 1500*time.Millisecond {
		t.Errorf("Run took %s after a 400ms deadline; backoff should stop on cancellation", elapsed)
	}
	if s := counters.Snapshot(); s.Dropped != 1 {
		t.Errorf("dropped=%d, want the job accounted for as dropped", s.Dropped)
	}
	if outcomeCount(counters, "http_5xx") == 0 {
		t.Error("server errors should be counted by outcome")
	}
}
