package proxy

import (
	"errors"
	"testing"
	"time"
)

func TestPool_RoundRobin(t *testing.T) {
	pl := NewPool([]string{"http://a", "http://b", "http://c"}, 3, time.Minute)

	var seen []string
	for i := 0; i < 6; i++ {
		p, err := pl.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		seen = append(seen, p.URL)
	}

	want := []string{"http://a", "http://b", "http://c", "http://a", "http://b", "http://c"}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("position %d: got %q, want %q (full sequence: %v)", i, seen[i], want[i], seen)
		}
	}
}

func TestPool_BansAfterThreshold(t *testing.T) {
	pl := NewPool([]string{"http://a", "http://b"}, 2, time.Hour)

	pA, _ := pl.Next() // a
	_, _ = pl.Next()   // b

	banned := pl.ReportResult(pA, errors.New("fail 1"))
	if banned {
		t.Error("expected no ban after 1st failure (threshold=2)")
	}
	banned = pl.ReportResult(pA, errors.New("fail 2"))
	if !banned {
		t.Error("expected a ban event on the 2nd consecutive failure")
	}

	// a should now be skipped in rotation, leaving only b.
	for i := 0; i < 4; i++ {
		p, err := pl.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if p.URL == "http://a" {
			t.Error("banned proxy a was still handed out")
		}
	}
}

func TestPool_AllBannedReturnsError(t *testing.T) {
	pl := NewPool([]string{"http://a"}, 1, time.Hour)
	p, _ := pl.Next()
	pl.ReportResult(p, errors.New("fail"))

	if _, err := pl.Next(); !errors.Is(err, ErrAllBanned) {
		t.Errorf("expected ErrAllBanned, got %v", err)
	}
}

func TestPool_SuccessRateTracking(t *testing.T) {
	pl := NewPool([]string{"http://a"}, 100, time.Hour) // high threshold: never bans in this test
	p, _ := pl.Next()

	pl.ReportResult(p, nil)
	pl.ReportResult(p, nil)
	pl.ReportResult(p, errors.New("fail"))

	stats := p.Stats()
	if stats.TotalSuccess != 2 || stats.TotalFailure != 1 {
		t.Fatalf("expected 2 success / 1 failure, got %+v", stats)
	}
	if got := stats.SuccessRate(); got < 0.66 || got > 0.67 {
		t.Errorf("expected success rate ~0.667, got %v", got)
	}
}

func TestPool_ConsecutiveFailsResetsOnSuccess(t *testing.T) {
	pl := NewPool([]string{"http://a"}, 3, time.Hour)
	p, _ := pl.Next()

	pl.ReportResult(p, errors.New("fail"))
	pl.ReportResult(p, errors.New("fail"))
	pl.ReportResult(p, nil) // reset
	pl.ReportResult(p, errors.New("fail"))

	if banned := pl.ReportResult(p, errors.New("fail")); banned {
		t.Error("expected no ban: only 2 consecutive failures since the reset, threshold is 3")
	}
}

func TestPool_Reload_PreservesStatsForRetainedURLs(t *testing.T) {
	pl := NewPool([]string{"http://a", "http://b"}, 100, time.Hour)
	pa, _ := pl.Next()
	pl.ReportResult(pa, nil)
	pl.ReportResult(pa, nil)

	pl.Reload([]string{"http://a", "http://c"}) // b dropped, c added, a retained

	if pl.Size() != 2 {
		t.Fatalf("expected 2 proxies after reload, got %d", pl.Size())
	}

	var foundA, foundC bool
	for _, s := range pl.AllStats() {
		switch s.URL {
		case "http://a":
			foundA = true
			if s.TotalSuccess != 2 {
				t.Errorf("expected proxy a to retain its 2 successes after reload, got %d", s.TotalSuccess)
			}
		case "http://c":
			foundC = true
			if s.TotalSuccess != 0 {
				t.Errorf("expected new proxy c to start with a clean history, got %d successes", s.TotalSuccess)
			}
		case "http://b":
			t.Error("proxy b should have been dropped by reload")
		}
	}
	if !foundA || !foundC {
		t.Errorf("expected both a and c present after reload, foundA=%v foundC=%v", foundA, foundC)
	}
}

func TestPool_Reload_EmptyMeansDirect(t *testing.T) {
	pl := NewPool([]string{"http://a"}, 3, time.Hour)
	pl.Reload(nil)

	if pl.Size() != 1 {
		t.Fatalf("expected 1 proxy (direct) after empty reload, got %d", pl.Size())
	}
	p, err := pl.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if p.Key() != "direct" {
		t.Errorf("expected direct identity after empty reload, got key %q", p.Key())
	}
}

func TestPool_RandomStrategy_OnlyPicksHealthy(t *testing.T) {
	pl := NewPool([]string{"http://a", "http://b"}, 1, time.Hour)
	pl.Strategy = Random

	pa, _ := pl.Next()
	// Find and ban whichever proxy Next() gave first, to make the assertion
	// strategy-agnostic.
	pl.ReportResult(pa, errors.New("fail"))

	for i := 0; i < 10; i++ {
		p, err := pl.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if p.URL == pa.URL {
			t.Errorf("banned proxy %q was still selected under Random strategy", pa.URL)
		}
	}
}

func TestPool_WeightedSuccessRateStrategy_PrefersHealthier(t *testing.T) {
	pl := NewPool([]string{"http://good", "http://bad"}, 1000, time.Hour) // high threshold: never bans
	pl.Strategy = WeightedSuccessRate

	var good, bad *Proxy
	// Build up history directly via Next()/ReportResult so both proxies get used.
	for i := 0; i < 20; i++ {
		p, _ := pl.Next()
		if p.URL == "http://good" {
			good = p
			pl.ReportResult(p, nil) // always succeeds
		} else {
			bad = p
			pl.ReportResult(p, errors.New("fail")) // always fails
		}
	}
	if good == nil || bad == nil {
		t.Fatal("expected round-robin-seeded Next() calls to have hit both proxies at least once")
	}

	counts := map[string]int{}
	for i := 0; i < 500; i++ {
		p, err := pl.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		counts[p.URL]++
		// Keep reinforcing the same pattern so weights stay separated.
		if p.URL == "http://good" {
			pl.ReportResult(p, nil)
		} else {
			pl.ReportResult(p, errors.New("fail"))
		}
	}

	if counts["http://good"] <= counts["http://bad"] {
		t.Errorf("expected the consistently-succeeding proxy to be picked more often, got %v", counts)
	}
}
