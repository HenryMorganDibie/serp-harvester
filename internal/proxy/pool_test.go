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

// Regression: a proxy that keeps failing after its first cooldown expires
// must be banned again. Previously the ban only triggered when the failure
// count equalled the threshold exactly, so it never re-banned.
func TestPool_RebansAfterCooldownExpires(t *testing.T) {
	pl := NewPool([]string{"http://p1:1"}, 2, 20*time.Millisecond)
	p, _ := pl.Next()
	pl.ReportResult(p, errors.New("fail"))
	if !pl.ReportResult(p, errors.New("fail")) {
		t.Fatal("expected first ban at the threshold")
	}
	time.Sleep(60 * time.Millisecond) // first cooldown (20ms) over

	p, err := pl.Next()
	if err != nil {
		t.Fatalf("proxy should be usable after cooldown: %v", err)
	}
	pl.ReportResult(p, errors.New("fail"))
	if !pl.ReportResult(p, errors.New("fail")) {
		t.Fatal("expected a second ban after failing the threshold again")
	}
	if _, err := pl.Next(); !errors.Is(err, ErrAllBanned) {
		t.Errorf("proxy should be cooling again, Next err = %v", err)
	}
}

func TestPool_CooldownsEscalateAndCap(t *testing.T) {
	pl := NewPool([]string{"http://p1:1"}, 1, 10*time.Second)
	pl.MaxCooldown = 35 * time.Second
	p, _ := pl.Next()

	var got []time.Duration
	for i := 0; i < 4; i++ {
		before := time.Now()
		pl.ReportOutcome(p, Blocked, 0)
		got = append(got, p.Stats().BannedUntil.Sub(before).Round(time.Second))
		// Expire the cooldown without waiting, keeping the streak.
		p.mu.Lock()
		p.bannedUntil = time.Time{}
		p.mu.Unlock()
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 35 * time.Second, 35 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cooldown %d = %s, want %s (all: %v)", i+1, got[i], want[i], got)
		}
	}

	pl.ReportOutcome(p, Success, 0)
	if s := p.Stats(); s.BanStreak != 0 {
		t.Errorf("success should reset the escalation, streak = %d", s.BanStreak)
	}
	before := time.Now()
	pl.ReportOutcome(p, Blocked, 0)
	if d := p.Stats().BannedUntil.Sub(before).Round(time.Second); d != 10*time.Second {
		t.Errorf("after a success the next cooldown should be the base 10s, got %s", d)
	}
}

func TestPool_BlockedCoolsImmediately(t *testing.T) {
	pl := NewPool([]string{"http://p1:1", "http://p2:1"}, 5, time.Hour)
	p, _ := pl.Next()
	if !pl.ReportOutcome(p, Blocked, 0) {
		t.Fatal("a block page should cool the proxy on the first occurrence")
	}
	for i := 0; i < 4; i++ {
		q, err := pl.Next()
		if err != nil || q == p {
			t.Fatalf("Next should skip the blocked proxy, got %v, %v", q, err)
		}
	}
}

func TestPool_RateLimitedHonorsRetryAfterWithoutEscalating(t *testing.T) {
	pl := NewPool([]string{"http://p1:1"}, 5, time.Hour)
	p, _ := pl.Next()
	before := time.Now()
	if !pl.ReportOutcome(p, RateLimited, 7*time.Second) {
		t.Fatal("rate limit should start a cooldown")
	}
	if d := p.Stats().BannedUntil.Sub(before).Round(time.Second); d != 7*time.Second {
		t.Errorf("cooldown = %s, want exactly Retry-After (7s)", d)
	}
	if p.Stats().BanStreak != 0 {
		t.Error("a Retry-After cooldown should not escalate future cooldowns")
	}
	if pl.ReportOutcome(p, RateLimited, time.Second) {
		t.Error("reporting while already cooling is not a new cooldown event")
	}
	if d := p.Stats().BannedUntil.Sub(before).Round(time.Second); d != 7*time.Second {
		t.Errorf("a shorter Retry-After must not shorten the cooldown, got %s", d)
	}
}

func TestPool_NeutralOutcomeChangesNothing(t *testing.T) {
	pl := NewPool([]string{"http://p1:1"}, 1, time.Hour)
	p, _ := pl.Next()
	for i := 0; i < 5; i++ {
		if pl.ReportOutcome(p, Neutral, 0) {
			t.Fatal("neutral outcome must not cool the proxy")
		}
	}
	if s := p.Stats(); s.TotalFailure != 0 || s.Health != -1 || s.Banned {
		t.Errorf("neutral outcomes changed stats: %+v", s)
	}
}

func TestPool_NextAvailableAtAndCounts(t *testing.T) {
	pl := NewPool([]string{"http://p1:1", "http://p2:1"}, 1, time.Hour)
	if !pl.NextAvailableAt().IsZero() {
		t.Error("with a healthy proxy, NextAvailableAt should be zero")
	}
	p1, _ := pl.Next()
	p2, _ := pl.Next()
	pl.ReportOutcome(p1, RateLimited, 30*time.Second)
	pl.ReportOutcome(p2, RateLimited, 10*time.Second)

	if a, c := pl.Counts(); a != 0 || c != 2 {
		t.Errorf("Counts = %d available, %d cooling; want 0, 2", a, c)
	}
	at := pl.NextAvailableAt()
	if d := time.Until(at).Round(time.Second); d != 10*time.Second {
		t.Errorf("NextAvailableAt in %s, want the soonest cooldown (10s)", d)
	}
}

func TestPool_HealthTracksRecentOutcomes(t *testing.T) {
	pl := NewPool([]string{"http://p1:1"}, 1000, time.Hour)
	p, _ := pl.Next()
	for i := 0; i < 50; i++ {
		pl.ReportOutcome(p, Success, 0)
	}
	for i := 0; i < 5; i++ {
		pl.ReportOutcome(p, Failure, 0)
	}
	s := p.Stats()
	if s.SuccessRate() < 0.9 {
		t.Fatalf("lifetime rate should still look good: %.2f", s.SuccessRate())
	}
	if s.Health > 0.4 {
		t.Errorf("health should drop quickly after 5 straight failures despite history, got %.2f", s.Health)
	}
}
