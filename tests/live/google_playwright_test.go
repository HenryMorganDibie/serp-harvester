package live

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/HenryMorganDibie/web-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/web-harvester/internal/metrics"
	"github.com/HenryMorganDibie/web-harvester/internal/parser"
)

// TestGooglePlaywright_RecordsOutcome sends one query to Google through
// headless Chromium and records what comes back. Like
// TestGoogleDirect_CurrentlyBlocked, it measures rather than asserts
// success: a CAPTCHA or other block is a valid, logged outcome, because the
// Playwright fetcher does not try to get past one. It fails only on
// infrastructure errors (driver or browser missing, network down).
//
// Run explicitly, after `make playwright-install`:
//
//	HARVESTER_LIVE=true go test ./tests/live/... -run Playwright -v
func TestGooglePlaywright_RecordsOutcome(t *testing.T) {
	requireLive(t)

	m := &metrics.BrowserCounters{}
	f, err := fetcher.NewPlaywrightFetcher(fetcher.PlaywrightConfig{
		Endpoint:       "https://www.google.com/search",
		Timeout:        30 * time.Second,
		PoolSize:       1,
		Headless:       true,
		ExecutablePath: os.Getenv("SERP_HARVESTER_CHROMIUM_PATH"),
		WaitSelector:   "#search",
		Metrics:        m,
	})
	if err != nil {
		t.Fatalf("start Playwright (run `make playwright-install`): %v", err)
	}
	defer f.Close()

	const query = "golang worker pool pattern"
	resp, err := f.Fetch(context.Background(), fetcher.Request{Query: query, Language: "en", Country: "US"})
	t.Logf("browser metrics: %+v", m.Snapshot())

	var blocked *fetcher.BlockedError
	var limited *fetcher.RateLimitError
	switch {
	case errors.As(err, &blocked):
		t.Logf("outcome: blocked by a %s page (status %d, url %s); not bypassed, by design", blocked.Reason, blocked.StatusCode, blocked.URL)
		return
	case errors.As(err, &limited):
		t.Logf("outcome: rate limited (status %d, retry-after %s)", limited.StatusCode, limited.RetryAfter)
		return
	case err != nil:
		t.Fatalf("fetch failed for a reason other than a recognised block: %v", err)
	}

	result, err := parser.New().Parse(query, resp.Body)
	if err != nil {
		t.Fatalf("parser error: %v", err)
	}
	t.Logf("outcome: rendered page, status %d, %d bytes, %d organic results, calibration=%+v",
		resp.StatusCode, len(resp.Body), len(result.Organic), result.Calibration)
	if result.Calibration != nil {
		t.Log("the page rendered but the parser's fixture-targeted selectors did not match live markup; see README \"Honest limitations\" item 1")
	}
}
