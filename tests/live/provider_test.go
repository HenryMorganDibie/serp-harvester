package live

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
)

// TestProvider_RealKeySmokeTest is the real-key verification called for in
// README "No API key yet?": confirms this codebase's JSON field mapping
// (internal/parser/json_provider.go) actually matches a real provider's
// response, not just the publicly documented schema it was built from.
// Skips (not fails) when no key is configured, so this never blocks CI or
// anyone without a key.
//
// Run explicitly:
//
//	HARVESTER_LIVE=true SERPAPI_KEY=... go test ./tests/live/... -run Provider -v
//
// Set PROVIDER_BASE_URL to test a different SerpApi-compatible vendor.
func TestProvider_RealKeySmokeTest(t *testing.T) {
	requireLive(t)

	apiKey := os.Getenv("SERPAPI_KEY")
	if apiKey == "" {
		t.Skip("set SERPAPI_KEY (and HARVESTER_LIVE=true) to run this test against a real provider")
	}
	baseURL := os.Getenv("PROVIDER_BASE_URL")
	if baseURL == "" {
		baseURL = "https://serpapi.com/search"
	}

	f := fetcher.NewProviderFetcher(baseURL, apiKey, "google", 15*time.Second)
	resp, err := f.Fetch(context.Background(), fetcher.Request{Query: "golang worker pool pattern"})
	if err != nil {
		t.Fatalf("provider fetch failed: %v", err)
	}
	t.Logf("status=%d bytes=%d", resp.StatusCode, len(resp.Body))

	p := parser.NewJSON()
	result, err := p.Parse("golang worker pool pattern", resp.Body)
	if err != nil {
		t.Fatalf("json parser failed on real provider response: %v", err)
	}

	if len(result.Organic) == 0 {
		t.Errorf(
			"expected at least one organic result from a real provider response, got 0 (calibration=%+v) — "+
				"check whether the provider's real field names match json_provider.go's providerResponse struct",
			result.Calibration,
		)
	} else {
		t.Logf("got %d organic results from a real provider response", len(result.Organic))
	}

	if result.AIOverview != nil {
		t.Logf("AI Overview present: %d chars of text, %d sources", len(result.AIOverview.Text), len(result.AIOverview.Sources))
	} else {
		t.Logf("no AI Overview for this query — not every query triggers one, not itself a failure")
	}
}
