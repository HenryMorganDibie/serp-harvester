package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServeHTTP_ExposesCounters(t *testing.T) {
	c := &Counters{}
	c.IncSuccess()
	c.IncSuccess()
	c.IncFailure()
	c.IncDropped()
	c.IncRetried()
	c.IncRetried()
	c.IncRetried()
	c.IncAIOverviewPresent()
	c.IncCalibrationFlagged()
	c.IncProxyBanned()

	srv := httptest.NewServer(ServeHTTP(c, nil))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)

	checks := []string{
		"serp_harvester_success_total 2",
		"serp_harvester_failure_total 1",
		"serp_harvester_dropped_total 1",
		"serp_harvester_retried_total 3",
		"serp_harvester_ai_overview_total 1",
		"serp_harvester_parser_drift_total 1",
		"serp_harvester_proxy_banned_total 1",
	}
	for _, want := range checks {
		if !strings.Contains(text, want) {
			t.Errorf("expected metrics output to contain %q, got:\n%s", want, text)
		}
	}

	// Latency metrics should be absent when no recorder is supplied.
	if strings.Contains(text, "serp_harvester_latency_p50_ms") {
		t.Error("did not expect latency metrics when latencies is nil")
	}
}

func TestServeHTTP_ExposesLatencyWhenRecorderSet(t *testing.T) {
	c := &Counters{}
	lat := NewLatencyRecorder(100)
	for i := 1; i <= 10; i++ {
		lat.Record(time.Duration(i*10) * time.Millisecond)
	}

	srv := httptest.NewServer(ServeHTTP(c, lat))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)

	for _, want := range []string{"serp_harvester_latency_p50_ms", "serp_harvester_latency_p95_ms", "serp_harvester_latency_p99_ms"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected metrics output to contain %q, got:\n%s", want, text)
		}
	}
}

func TestServeHTTP_Healthz(t *testing.T) {
	srv := httptest.NewServer(ServeHTTP(&Counters{}, nil))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200 from /healthz, got %d", resp.StatusCode)
	}
}

func TestServeHTTP_BrowserMetricsOnlyWhenEnabled(t *testing.T) {
	scrape := func(c *Counters) string {
		srv := httptest.NewServer(ServeHTTP(c, nil))
		defer srv.Close()
		resp, err := srv.Client().Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return string(body)
	}

	if text := scrape(&Counters{}); strings.Contains(text, "serp_harvester_browser_") {
		t.Error("browser metrics should be absent outside mode: playwright")
	}

	b := &BrowserCounters{}
	b.IncLaunches()
	b.IncDisconnects()
	b.IncNavigationTimeouts()
	b.IncConsentHandled()
	b.IncBlocked("captcha")
	b.IncBlocked("captcha")
	b.IncBlocked("consent")
	b.IncBlocked("interstitial")
	b.AddSessionsOpen(3)
	b.AddSessionsInUse(2)
	b.AddSessionsInUse(-1)

	text := scrape(&Counters{Browser: b})
	for _, want := range []string{
		"serp_harvester_browser_launches_total 1",
		"serp_harvester_browser_disconnects_total 1",
		"serp_harvester_browser_navigation_timeouts_total 1",
		"serp_harvester_browser_consent_handled_total 1",
		`serp_harvester_browser_blocked_total{reason="captcha"} 2`,
		`serp_harvester_browser_blocked_total{reason="consent"} 1`,
		`serp_harvester_browser_blocked_total{reason="interstitial"} 1`,
		"serp_harvester_browser_sessions_open 3",
		"serp_harvester_browser_sessions_in_use 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected metrics output to contain %q, got:\n%s", want, text)
		}
	}
}

func TestBrowserCounters_NilSafe(t *testing.T) {
	var b *BrowserCounters
	b.IncLaunches()
	b.IncBlocked("captcha")
	b.AddSessionsOpen(1)
	if s := b.Snapshot(); s != (BrowserSnapshot{}) {
		t.Errorf("nil snapshot = %+v, want zero", s)
	}
}

func TestServeHTTP_AcquisitionMetrics(t *testing.T) {
	c := &Counters{}
	c.IncOutcome("success")
	c.IncOutcome("success")
	c.IncOutcome("captcha")
	c.IncProxyCooldown("blocked")
	c.IncRateDecrease()
	c.IncFailover()
	c.IncHTTPConsentHandled()

	srv := httptest.NewServer(ServeHTTP(c, nil))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		`serp_harvester_fetch_outcomes_total{outcome="success"} 2`,
		`serp_harvester_fetch_outcomes_total{outcome="captcha"} 1`,
		`serp_harvester_proxy_cooldowns_total{reason="blocked"} 1`,
		"serp_harvester_ratelimit_decreases_total 1",
		"serp_harvester_fetch_failovers_total 1",
		"serp_harvester_http_consent_handled_total 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "serp_harvester_proxies_available") {
		t.Error("proxy gauges should be absent when ProxyGauges is unset")
	}

	c.ProxyGauges = func() ProxyGauges { return ProxyGauges{Available: 3, Cooling: 2, Throttled: 1} }
	c.Browser = &BrowserCounters{}
	c.Browser.IncLaunchFailures()
	c.Browser.IncPageCrashes()
	resp2, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	for _, want := range []string{
		"serp_harvester_proxies_available 3",
		"serp_harvester_proxies_cooling 2",
		"serp_harvester_proxies_throttled 1",
		"serp_harvester_browser_launch_failures_total 1",
		"serp_harvester_browser_page_crashes_total 1",
	} {
		if !strings.Contains(string(body2), want) {
			t.Errorf("expected %q in:\n%s", want, body2)
		}
	}
}
