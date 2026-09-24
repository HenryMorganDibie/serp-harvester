package fetcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/playwright-community/playwright-go"

	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
)

// Browser-backed tests for PlaywrightFetcher. Every page comes from a local
// httptest server; nothing here contacts Google. They need the Playwright
// driver and Chromium, so they run only with SERP_HARVESTER_PLAYWRIGHT=1
// (CI installs both and sets it). SERP_HARVESTER_CHROMIUM_PATH optionally
// points at a Chromium binary other than the Playwright-installed one.

func newTestPlaywright(t *testing.T, cfg PlaywrightConfig) (*PlaywrightFetcher, *metrics.BrowserCounters) {
	t.Helper()
	if os.Getenv("SERP_HARVESTER_PLAYWRIGHT") != "1" {
		t.Skip("set SERP_HARVESTER_PLAYWRIGHT=1 (with Playwright's Chromium installed) to run browser tests")
	}
	cfg.Headless = true
	cfg.ExecutablePath = os.Getenv("SERP_HARVESTER_CHROMIUM_PATH")
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	m := &metrics.BrowserCounters{}
	cfg.Metrics = m
	f, err := NewPlaywrightFetcher(cfg)
	if err != nil {
		t.Fatalf("NewPlaywrightFetcher: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f, m
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/playwright/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func serveHTML(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

func probe(t *testing.T, body []byte) *goquery.Selection {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	return doc.Find("#probe")
}

func TestPlaywright_RendersJavaScriptForTheHTMLParser(t *testing.T) {
	page := fixture(t, "js_results.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveHTML(w, http.StatusOK, page)
	}))
	defer srv.Close()

	// Baseline: the raw document has no results until its script runs.
	raw, err := parser.New().Parse("q", page)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Organic) != 0 || raw.Calibration == nil {
		t.Fatalf("fixture should parse to zero results without rendering, got %d", len(raw.Organic))
	}

	f, _ := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search", WaitSelector: ".organic-result"})
	resp, err := f.Fetch(context.Background(), Request{Query: "golang worker pool"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.Latency <= 0 {
		t.Errorf("status=%d latency=%s", resp.StatusCode, resp.Latency)
	}

	result, err := parser.New().Parse("golang worker pool", resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Organic) != 3 || result.Calibration != nil {
		t.Fatalf("rendered page should parse to 3 results, got %d (calibration %+v)", len(result.Organic), result.Calibration)
	}
	if !strings.Contains(result.Organic[0].Title, "golang worker pool") {
		t.Errorf("query not passed through: title %q", result.Organic[0].Title)
	}
}

func TestPlaywright_PassesQueryLocaleDeviceAndUserAgent(t *testing.T) {
	page := fixture(t, "js_results.html")
	var mu sync.Mutex
	var gotQuery url.Values
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			mu.Lock()
			gotQuery, gotHeaders = r.URL.Query(), r.Header.Clone()
			mu.Unlock()
		}
		serveHTML(w, http.StatusOK, page)
	}))
	defer srv.Close()

	f, _ := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search"})
	resp, err := f.Fetch(context.Background(), Request{
		Query: "heat pump", Language: "en", Country: "US", Device: "mobile",
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) serp-harvester/0.1",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotQuery.Get("q") != "heat pump" || gotQuery.Get("hl") != "en" || gotQuery.Get("gl") != "US" {
		t.Errorf("query params = %v", gotQuery)
	}
	if al := gotHeaders.Get("Accept-Language"); !strings.HasPrefix(al, "en-US") {
		t.Errorf("Accept-Language = %q, want en-US first", al)
	}
	if ua := gotHeaders.Get("User-Agent"); ua != "Mozilla/5.0 (X11; Linux x86_64) serp-harvester/0.1" {
		t.Errorf("User-Agent = %q, want the caller's UA unchanged", ua)
	}

	p := probe(t, resp.Body)
	if v, _ := p.Attr("data-language"); v != "en-US" {
		t.Errorf("navigator.language = %q", v)
	}
	if v, _ := p.Attr("data-width"); v != "412" {
		t.Errorf("mobile viewport width = %q, want 412", v)
	}
	if v, _ := p.Attr("data-touch"); v != "true" {
		t.Errorf("mobile touch = %q, want true", v)
	}
}

func TestPlaywright_DismissesConsentWithRejectAll(t *testing.T) {
	results := fixture(t, "js_results.html")
	consent := fixture(t, "consent.html")
	var saves atomic.Int32
	var choice atomic.Value

	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("fixture_consent"); err != nil || c.Value == "" {
			http.Redirect(w, r, "/consent?continue="+url.QueryEscape(r.URL.String()), http.StatusFound)
			return
		}
		serveHTML(w, http.StatusOK, results)
	})
	mux.HandleFunc("/consent", func(w http.ResponseWriter, r *http.Request) {
		page := bytes.ReplaceAll(consent, []byte("{{CONTINUE}}"), []byte(r.URL.Query().Get("continue")))
		serveHTML(w, http.StatusOK, page)
	})
	mux.HandleFunc("/save", func(w http.ResponseWriter, r *http.Request) {
		saves.Add(1)
		choice.Store(r.FormValue("set_eom"))
		http.SetCookie(w, &http.Cookie{Name: "fixture_consent", Value: "decided", Path: "/"})
		http.Redirect(w, r, r.FormValue("continue"), http.StatusSeeOther)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search"})
	resp, err := f.Fetch(context.Background(), Request{Query: "q1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, _ := choice.Load().(string); got != "true" {
		t.Errorf("consent choice = %q, want set_eom=true (reject all)", got)
	}
	if result, _ := parser.New().Parse("q1", resp.Body); len(result.Organic) != 3 {
		t.Errorf("expected results after consent, got %d", len(result.Organic))
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status after consent = %d", resp.StatusCode)
	}

	// The session is reused, so the decision persists like a cookie jar.
	if _, err := f.Fetch(context.Background(), Request{Query: "q2"}); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if saves.Load() != 1 || m.Snapshot().ConsentHandled != 1 {
		t.Errorf("consent saves=%d handled=%d, want 1 each", saves.Load(), m.Snapshot().ConsentHandled)
	}
}

func TestPlaywright_ConsentWithoutRejectFormIsBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveHTML(w, http.StatusOK, []byte(`<html><body><form action="/consent/save"><button>Accept all</button></form></body></html>`))
	}))
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search"})
	_, err := f.Fetch(context.Background(), Request{Query: "q"})
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "consent" {
		t.Fatalf("err = %v, want a consent BlockedError", err)
	}
	if m.Snapshot().BlockedConsent != 1 {
		t.Errorf("blocked consent metric = %d", m.Snapshot().BlockedConsent)
	}
}

func TestPlaywright_CaptchaIsReportedNotSolved(t *testing.T) {
	page := fixture(t, "captcha.html")
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		serveHTML(w, http.StatusTooManyRequests, page)
	}))
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search"})
	_, err := f.Fetch(context.Background(), Request{Query: "q"})
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "captcha" || blocked.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want a captcha BlockedError with status 429", err)
	}
	if posts.Load() != 0 {
		t.Errorf("fetcher submitted the CAPTCHA form %d times; it must never interact with it", posts.Load())
	}
	if m.Snapshot().BlockedCaptcha != 1 {
		t.Errorf("blocked captcha metric = %d", m.Snapshot().BlockedCaptcha)
	}
}

func TestPlaywright_RateLimitHonorsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		serveHTML(w, http.StatusTooManyRequests, []byte("<html><body>slow down</body></html>"))
	}))
	defer srv.Close()

	f, _ := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search"})
	_, err := f.Fetch(context.Background(), Request{Query: "q"})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Fatalf("err = %v, want RateLimitError with RetryAfter 7s", err)
	}
}

func TestPlaywright_ServerErrorStatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveHTML(w, http.StatusServiceUnavailable, []byte("<html><body>down</body></html>"))
	}))
	defer srv.Close()

	f, _ := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search"})
	if _, err := f.Fetch(context.Background(), Request{Query: "q"}); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want an unexpected-status error", err)
	}
}

func TestPlaywright_NavigationTimeoutThenRecovers(t *testing.T) {
	page := fixture(t, "js_results.html")
	var slow atomic.Bool
	slow.Store(true)
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		serveHTML(w, http.StatusOK, page)
	}))
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search", Timeout: 700 * time.Millisecond})
	start := time.Now()
	_, err := f.Fetch(context.Background(), Request{Query: "q"})
	if !errors.Is(err, playwright.ErrTimeout) {
		t.Fatalf("err = %v, want a navigation timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %s, want close to 700ms", elapsed)
	}
	if m.Snapshot().NavigationTimeouts != 1 {
		t.Errorf("navigation timeouts metric = %d", m.Snapshot().NavigationTimeouts)
	}

	slow.Store(false)
	if _, err := f.Fetch(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatalf("Fetch after timeout: %v", err)
	}
}

func TestPlaywright_ContextCancellationAbortsNavigation(t *testing.T) {
	page := fixture(t, "js_results.html")
	var hang atomic.Bool
	hang.Store(true)
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		serveHTML(w, http.StatusOK, page)
	}))
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search", Timeout: 30 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.Fetch(ctx, Request{Query: "q"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %s; it should not wait for the 30s navigation timeout", elapsed)
	}
	if s := m.Snapshot(); s.SessionsInUse != 0 || s.SessionsOpen != 0 {
		t.Errorf("cancelled session not released: %+v", s)
	}

	hang.Store(false)
	if _, err := f.Fetch(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatalf("Fetch after cancellation: %v", err)
	}
}

func TestPlaywright_AuthenticatedProxy(t *testing.T) {
	page := fixture(t, "js_results.html")
	var sawAuth atomic.Bool
	var sawHost atomic.Value
	// A minimal forward proxy: it demands Basic credentials, then serves
	// the fixture for the (fictional) target host itself.
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "no TLS in this fixture", http.StatusMethodNotAllowed)
			return
		}
		auth := r.Header.Get("Proxy-Authorization")
		if auth == "" {
			w.Header().Set("Proxy-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if !isBasicAuthFor(auth, "puser", "ppass") {
			http.Error(w, "bad proxy credentials", http.StatusForbidden)
			return
		}
		sawAuth.Store(true)
		sawHost.Store(r.URL.Host)
		serveHTML(w, http.StatusOK, page)
	}))
	defer proxySrv.Close()

	f, _ := newTestPlaywright(t, PlaywrightConfig{Endpoint: "http://serp.fixture.test/search"})
	resp, err := f.Fetch(context.Background(), Request{
		Query:    "via proxy",
		ProxyURL: "http://puser:ppass@" + proxySrv.Listener.Addr().String(),
	})
	if err != nil {
		t.Fatalf("Fetch through proxy: %v", err)
	}
	if !sawAuth.Load() {
		t.Error("proxy never received valid Proxy-Authorization")
	}
	if h, _ := sawHost.Load().(string); h != "serp.fixture.test" {
		t.Errorf("proxy saw host %q, want serp.fixture.test", h)
	}
	if result, _ := parser.New().Parse("via proxy", resp.Body); len(result.Organic) != 3 {
		t.Errorf("expected 3 results via proxy, got %d", len(result.Organic))
	}
}

func TestPlaywright_InvalidProxyURLFails(t *testing.T) {
	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: "http://serp.fixture.test/search"})
	if _, err := f.Fetch(context.Background(), Request{Query: "q", ProxyURL: "::bad"}); err == nil {
		t.Fatal("expected an error for an invalid proxy url")
	}
	if s := m.Snapshot(); s.SessionsOpen != 0 || s.SessionsInUse != 0 {
		t.Errorf("failed session setup leaked: %+v", s)
	}
}

func TestPlaywright_PoolIsBoundedAndBrowserIsShared(t *testing.T) {
	page := fixture(t, "js_results.html")
	var inflight, maxInflight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			n := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				m := maxInflight.Load()
				if n <= m || maxInflight.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
		}
		serveHTML(w, http.StatusOK, page)
	}))
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search", PoolSize: 2})
	devices := []string{"desktop", "mobile", "tablet"} // more profiles than slots forces eviction
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.Fetch(context.Background(), Request{Query: fmt.Sprintf("q%d", i), Device: devices[i%len(devices)]})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	}

	if got := maxInflight.Load(); got > 2 {
		t.Errorf("max concurrent page loads = %d, want <= PoolSize 2", got)
	}
	s := m.Snapshot()
	if s.Launches != 1 {
		t.Errorf("browser launches = %d, want 1 shared browser", s.Launches)
	}
	if s.SessionsOpen > 2 || s.SessionsOpen < 1 || s.SessionsInUse != 0 {
		t.Errorf("sessions open=%d in_use=%d, want 1..2 open and none in use", s.SessionsOpen, s.SessionsInUse)
	}
}

func TestPlaywright_RelaunchesAfterBrowserDisconnect(t *testing.T) {
	page := fixture(t, "js_results.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveHTML(w, http.StatusOK, page)
	}))
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search"})
	if _, err := f.Fetch(context.Background(), Request{Query: "before"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Simulate a crash: the browser goes away underneath the fetcher.
	f.mu.Lock()
	b := f.browser
	f.mu.Unlock()
	b.Close()

	if _, err := f.Fetch(context.Background(), Request{Query: "after"}); err != nil {
		t.Fatalf("Fetch after browser loss: %v", err)
	}
	s := m.Snapshot()
	if s.Launches != 2 || s.Disconnects != 1 {
		t.Errorf("launches=%d disconnects=%d, want 2 and 1", s.Launches, s.Disconnects)
	}
	if s.SessionsOpen != 1 {
		t.Errorf("sessions open = %d, want only the new browser's session", s.SessionsOpen)
	}
}

func TestPlaywright_CloseShutsEverythingDown(t *testing.T) {
	page := fixture(t, "js_results.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveHTML(w, http.StatusOK, page)
	}))
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search"})
	if _, err := f.Fetch(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	f.mu.Lock()
	b := f.browser
	f.mu.Unlock()

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if b.IsConnected() {
		t.Error("browser still connected after Close")
	}
	if s := m.Snapshot(); s.SessionsOpen != 0 || s.Disconnects != 0 {
		t.Errorf("after Close: %+v, want no open sessions and no unexpected disconnects", s)
	}
	if _, err := f.Fetch(context.Background(), Request{Query: "q"}); !errors.Is(err, ErrFetcherClosed) {
		t.Errorf("Fetch after Close = %v, want ErrFetcherClosed", err)
	}
}

func TestPlaywright_CloseUnblocksWaitersAndDrainsInFlight(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		serveHTML(w, http.StatusOK, []byte("<html><body>late</body></html>"))
	}))
	defer srv.Close()

	f, m := newTestPlaywright(t, PlaywrightConfig{Endpoint: srv.URL + "/search", PoolSize: 1, Timeout: 20 * time.Second})

	inFlight := make(chan error, 1)
	go func() {
		_, err := f.Fetch(context.Background(), Request{Query: "holds the only slot"})
		inFlight <- err
	}()
	waitFor(t, func() bool { return m.Snapshot().SessionsInUse == 1 })

	waiter := make(chan error, 1)
	go func() {
		_, err := f.Fetch(context.Background(), Request{Query: "waits for a slot"})
		waiter <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the waiter block on the pool

	closed := make(chan error, 1)
	go func() { closed <- f.Close() }()

	select {
	case err := <-waiter:
		if !errors.Is(err, ErrFetcherClosed) {
			t.Errorf("waiting Fetch = %v, want ErrFetcherClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock a Fetch waiting for a slot")
	}

	// Close waits for the in-flight fetch; let it finish.
	unblock()
	if err := <-inFlight; err != nil {
		t.Errorf("in-flight Fetch during Close: %v", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the in-flight fetch finished")
	}
	if s := m.Snapshot(); s.SessionsOpen != 0 || s.SessionsInUse != 0 {
		t.Errorf("after Close: %+v", s)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
