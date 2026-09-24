package live

import (
	"context"
	"testing"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
)

// TestGoogleDirect_CurrentlyBlocked documents, reproducibly, that a plain
// HTTP fetch against Google does not currently return usable organic
// results — see README "Live mode" for what was found (a consent
// interstitial on some attempts, a JS-execution check on others). This test
// passes when the pipeline correctly detects and flags that outcome
// (calibration), and deliberately calls out — via t.Log, not a failure — the
// case where Google unexpectedly returns real results, since that would
// mean the documented finding is stale and README needs revisiting.
//
// Run explicitly: HARVESTER_LIVE=true go test ./tests/live/... -run GoogleDirect -v
func TestGoogleDirect_CurrentlyBlocked(t *testing.T) {
	requireLive(t)

	f, err := fetcher.NewHTTPFetcher("https://www.google.com/search", 10*time.Second)
	if err != nil {
		t.Fatalf("build fetcher: %v", err)
	}

	resp, err := f.Fetch(context.Background(), fetcher.Request{Query: "golang worker pool pattern"})
	if err != nil {
		t.Fatalf("unexpected network error reaching Google: %v", err)
	}
	t.Logf("status=%d bytes=%d", resp.StatusCode, len(resp.Body))

	p := parser.New()
	result, err := p.Parse("golang worker pool pattern", resp.Body)
	if err != nil {
		t.Fatalf("parser error: %v", err)
	}

	if len(result.Organic) > 0 {
		t.Logf(
			"NOTE: this run got %d real organic results back. If reproducible, README \"Live mode\" is stale — "+
				"direct-to-Google may no longer be blocked the way it was when documented. Investigate before relying on this.",
			len(result.Organic),
		)
		return
	}

	if result.Calibration == nil {
		t.Error("expected either real organic results or a calibration flag; got neither — an unexpected empty-but-unflagged state worth investigating")
		return
	}
	t.Logf("confirmed current state: direct-to-Google fetch did not return usable organic results (calibration=%+v)", result.Calibration)
}
