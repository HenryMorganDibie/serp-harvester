package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
)

// Live capture and calibration against Google. Both are gated behind
// HARVESTER_LIVE=true, send a small, capped number of requests at a fixed
// low pace, and never try to get past a block: they record what Google
// returns.
//
//	HARVESTER_LIVE=true SERP_CAPTURE_DIR=./captures go test ./tests/live -run Capture -v
//	HARVESTER_LIVE=true SERP_CAPTURE_DIR=./captures go test ./tests/live -run Calibration -v -timeout 60m
//
// SERP_CAPTURE_DIR is resolved relative to this package directory
// (tests/live) when relative, as go test runs there.
//
// Environment (all optional): SERP_CAPTURE_QUERIES (comma-separated),
// SERP_CAPTURE_MODES (capture only; default "http,playwright"), SERP_LIVE_MODE (http | playwright, calibration only; default playwright),
// SERP_LIVE_PROXY (one proxy URL), SERP_CALIBRATION_REQUESTS (default 20,
// max 100), SERP_CALIBRATION_INTERVAL (default 15s, min 3s),
// SERP_HARVESTER_CHROMIUM_PATH.

const liveEndpoint = "https://www.google.com/search"

var defaultQueries = []string{
	"golang worker pool pattern",
	"how does a heat pump work",
	"best noise cancelling headphones",
}

func liveFetcher(t *testing.T, mode string) fetcher.Fetcher {
	t.Helper()
	switch mode {
	case "http":
		f, err := fetcher.NewHTTPFetcher(liveEndpoint, 20*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return f
	case "playwright":
		f, err := fetcher.NewPlaywrightFetcher(fetcher.PlaywrightConfig{
			Endpoint:       liveEndpoint,
			Timeout:        45 * time.Second,
			PoolSize:       1,
			Headless:       true,
			ExecutablePath: os.Getenv("SERP_HARVESTER_CHROMIUM_PATH"),
			WaitSelector:   "#search",
		})
		if err != nil {
			t.Fatalf("start Playwright (run `make playwright-install`): %v", err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	t.Fatalf("unknown mode %q (want http or playwright)", mode)
	return nil
}

func envQueries() []string {
	if v := os.Getenv("SERP_CAPTURE_QUERIES"); v != "" {
		return strings.Split(v, ",")
	}
	return defaultQueries
}

func captureDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("SERP_CAPTURE_DIR")
	if dir == "" {
		t.Skip("set SERP_CAPTURE_DIR to a directory for captured pages and reports")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// pageOf returns the page Google served for a fetch: the body on success,
// the block page on a BlockedError.
func pageOf(resp *fetcher.Response, err error) []byte {
	if resp != nil {
		return resp.Body
	}
	var blocked *fetcher.BlockedError
	if errors.As(err, &blocked) {
		return blocked.Body
	}
	return nil
}

// TestGoogleCapture_SavesPages fetches a few queries through both the HTTP
// fetcher and Chromium and saves every page Google returns (results or
// block pages) with its classification, as the raw material for
// retargeting the parser at live markup.
func TestGoogleCapture_SavesPages(t *testing.T) {
	requireLive(t)
	dir := captureDir(t)
	proxyURL := os.Getenv("SERP_LIVE_PROXY")

	modes := []string{"http", "playwright"}
	if v := os.Getenv("SERP_CAPTURE_MODES"); v != "" {
		modes = strings.Split(v, ",")
	}
	for _, mode := range modes {
		f := liveFetcher(t, mode)
		for i, q := range envQueries() {
			if i > 0 {
				time.Sleep(10 * time.Second) // stay slow; this is a capture, not a load test
			}
			resp, err := f.Fetch(context.Background(), fetcher.Request{Query: q, Language: "en", Country: "US", ProxyURL: proxyURL})
			outcome := fetcher.Classify(err)
			page := pageOf(resp, err)
			base := filepath.Join(dir, fmt.Sprintf("%s-%02d-%s", mode, i, outcome))
			if len(page) > 0 {
				if werr := os.WriteFile(base+".html", page, 0o644); werr != nil {
					t.Fatal(werr)
				}
			}
			meta := map[string]any{"mode": mode, "query": q, "outcome": outcome.String(), "captured_at": time.Now().UTC()}
			if err != nil {
				meta["error"] = err.Error()
			}
			if resp != nil {
				result, _ := parser.New().Parse(q, resp.Body)
				meta["status"] = resp.StatusCode
				meta["parsed_organic"] = len(result.Organic)
				meta["calibration"] = result.Calibration
			}
			b, _ := json.MarshalIndent(meta, "", "  ")
			os.WriteFile(base+".json", b, 0o644)
			t.Logf("%s %q -> %s (%d bytes saved to %s.html)", mode, q, outcome, len(page), base)
			if outcome == fetcher.OutcomeTransport || outcome == fetcher.OutcomeOther {
				t.Fatalf("network failure, not a capture of what Google serves: %v", err)
			}
		}
	}
}

// calibrationSample is one request of a calibration run.
type calibrationSample struct {
	Index      int           `json:"index"`
	Elapsed    time.Duration `json:"elapsed_ns"`
	Outcome    string        `json:"outcome"`
	RetryAfter time.Duration `json:"retry_after_ns,omitempty"`
	Latency    time.Duration `json:"latency_ns,omitempty"`
}

// calibrationSummary is what a run measured, plus suggested settings.
type calibrationSummary struct {
	Mode           string         `json:"mode"`
	Requests       int            `json:"requests"`
	Interval       time.Duration  `json:"interval_ns"`
	Outcomes       map[string]int `json:"outcomes"`
	FirstPushback  int            `json:"first_pushback_index"` // -1 if none
	PushbackAfter  time.Duration  `json:"pushback_after_ns"`
	MaxRetryAfter  time.Duration  `json:"max_retry_after_ns"`
	RecoveredAfter time.Duration  `json:"recovered_after_ns"` // first pushback -> next success; 0 if never
	Suggestions    []string       `json:"suggestions"`
}

func isPushback(outcome string) bool {
	switch outcome {
	case "rate_limited", "captcha", "interstitial":
		return true
	}
	return false
}

// summarize turns calibration samples into measurements and suggested
// config values. The suggestions only extrapolate from what was observed.
func summarize(mode string, interval time.Duration, samples []calibrationSample) calibrationSummary {
	s := calibrationSummary{Mode: mode, Requests: len(samples), Interval: interval, Outcomes: map[string]int{}, FirstPushback: -1}
	for _, x := range samples {
		s.Outcomes[x.Outcome]++
		if x.RetryAfter > s.MaxRetryAfter {
			s.MaxRetryAfter = x.RetryAfter
		}
		if s.FirstPushback < 0 && isPushback(x.Outcome) {
			s.FirstPushback = x.Index
			s.PushbackAfter = x.Elapsed
		}
		if s.FirstPushback >= 0 && s.RecoveredAfter == 0 && x.Outcome == "success" && x.Index > s.FirstPushback {
			s.RecoveredAfter = x.Elapsed - s.PushbackAfter
		}
	}
	rps := 1 / interval.Seconds()
	switch {
	case s.Outcomes["success"] == 0 && s.FirstPushback == 0:
		// Blocked before any rate could matter: the egress itself is
		// refused, so there is no rate to recommend.
		s.Suggestions = append(s.Suggestions, fmt.Sprintf(
			"blocked from the first request and never served results in %d requests: the block is not rate-driven, so no rate_per_proxy_rps is supported from this egress; use different egress",
			s.Requests))
	case s.FirstPushback < 0:
		s.Suggestions = append(s.Suggestions, fmt.Sprintf(
			"no pushback in %d requests at %.3f req/s: rate_per_proxy_rps up to %.3f is supported by this run; higher rates are untested",
			s.Requests, rps, rps))
	default:
		s.Suggestions = append(s.Suggestions, fmt.Sprintf(
			"first pushback at request %d (%s in) at %.3f req/s: keep rate_per_proxy_rps below %.3f for this egress",
			s.FirstPushback, s.PushbackAfter.Round(time.Second), rps, rps/2))
	}
	if s.FirstPushback >= 0 {
		if s.RecoveredAfter > 0 {
			s.Suggestions = append(s.Suggestions, fmt.Sprintf(
				"success resumed %s after the first pushback: proxy_ban_cooldown of about %s matches that",
				s.RecoveredAfter.Round(time.Second), s.RecoveredAfter.Round(time.Second)))
		} else {
			s.Suggestions = append(s.Suggestions,
				"no recovery observed before the run ended: cooldowns must be longer than this run; extend it or keep proxy_ban_cooldown_max high")
		}
	}
	if s.MaxRetryAfter > 0 {
		s.Suggestions = append(s.Suggestions, fmt.Sprintf(
			"largest Retry-After was %s (the pool cools a proxy for exactly Retry-After on a 429)", s.MaxRetryAfter.Round(time.Second)))
	}
	return s
}

func envInt(name string, def, lo, hi int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return def
	}
	return max(lo, min(hi, v))
}

// TestGoogleCalibration_MeasuresPushback sends a capped number of queries
// through one fetcher and egress at a fixed interval, recording when Google
// first pushes back (429, CAPTCHA, JS check), any Retry-After, and whether
// and when success resumes. It keeps going after a block (at the same slow
// pace) precisely to measure recovery time, which is what
// proxy_ban_cooldown should be based on. The report is written to
// SERP_CAPTURE_DIR.
func TestGoogleCalibration_MeasuresPushback(t *testing.T) {
	requireLive(t)
	dir := captureDir(t)
	mode := os.Getenv("SERP_LIVE_MODE")
	if mode == "" {
		mode = "playwright"
	}
	n := envInt("SERP_CALIBRATION_REQUESTS", 20, 1, 100)
	interval := 15 * time.Second
	if d, err := time.ParseDuration(os.Getenv("SERP_CALIBRATION_INTERVAL")); err == nil {
		interval = max(d, 3*time.Second)
	}
	f := liveFetcher(t, mode)
	queries := envQueries()
	proxyURL := os.Getenv("SERP_LIVE_PROXY")

	start := time.Now()
	var samples []calibrationSample
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(time.Until(start.Add(time.Duration(i) * interval)))
		}
		q := queries[i%len(queries)]
		resp, err := f.Fetch(context.Background(), fetcher.Request{Query: q, Language: "en", Country: "US", ProxyURL: proxyURL})
		x := calibrationSample{Index: i, Elapsed: time.Since(start), Outcome: fetcher.Classify(err).String()}
		var rl *fetcher.RateLimitError
		if errors.As(err, &rl) {
			x.RetryAfter = rl.RetryAfter
		}
		if resp != nil {
			x.Latency = resp.Latency
		}
		samples = append(samples, x)
		t.Logf("#%02d +%s %q -> %s", i, x.Elapsed.Round(time.Second), q, x.Outcome)
		if x.Outcome == "transport" || x.Outcome == "other" {
			t.Fatalf("network failure, not a measurement: %v", err)
		}
	}

	summary := summarize(mode, interval, samples)
	report := map[string]any{"summary": summary, "samples": samples, "proxy": proxyURL != ""}
	b, _ := json.MarshalIndent(report, "", "  ")
	path := filepath.Join(dir, fmt.Sprintf("calibration-%s-%s.json", mode, start.UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("outcomes: %v", summary.Outcomes)
	for _, s := range summary.Suggestions {
		t.Logf("suggestion: %s", s)
	}
	t.Logf("report: %s", path)
}
