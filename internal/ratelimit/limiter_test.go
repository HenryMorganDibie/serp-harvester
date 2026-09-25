package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestLimiter_WaitEnforcesRatePerKey(t *testing.T) {
	l := New(20, 1) // one token every 50ms per key
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := l.Wait(ctx, "a"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Errorf("4 waits at 20 rps took %s; expected ~150ms", elapsed)
	}
	// A different key has its own bucket.
	start = time.Now()
	l.Wait(ctx, "b")
	if time.Since(start) > 20*time.Millisecond {
		t.Error("an unused key should not wait")
	}
}

func TestLimiter_WaitHonorsContext(t *testing.T) {
	l := New(0.1, 1)
	l.Wait(context.Background(), "a") // spend the only token
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, "a"); err == nil {
		t.Error("expected Wait to fail when the context ends first")
	}
}

func TestLimiter_NonAdaptiveIgnoresFeedback(t *testing.T) {
	l := New(4, 1)
	if l.Penalize("a") {
		t.Error("New limiter must not adapt")
	}
	l.Reward("a")
	if l.Rate("a") != 4 || l.Throttled() != 0 {
		t.Errorf("rate changed on a non-adaptive limiter: %v", l.Rate("a"))
	}
}

func TestLimiter_AdaptiveAIMD(t *testing.T) {
	l := NewAdaptive(4, 1, 0.5)

	if !l.Penalize("p1") || l.Rate("p1") != 2 {
		t.Fatalf("first penalty should halve 4 -> 2, got %v", l.Rate("p1"))
	}
	l.Penalize("p1")
	l.Penalize("p1")
	if l.Rate("p1") != 0.5 {
		t.Fatalf("rate should floor at 0.5, got %v", l.Rate("p1"))
	}
	if l.Penalize("p1") {
		t.Error("penalizing at the floor should report no decrease")
	}
	if l.Rate("p2") != 4 || l.Throttled() != 1 {
		t.Errorf("other keys must be unaffected: p2=%v throttled=%d", l.Rate("p2"), l.Throttled())
	}

	// Additive recovery: +5% of 4 rps (0.2) per success, capped at 4.
	for i := 0; i < 10; i++ {
		l.Reward("p1")
	}
	if got := l.Rate("p1"); got < 2.49 || got > 2.51 {
		t.Errorf("after 10 rewards rate = %v, want 2.5", got)
	}
	for i := 0; i < 100; i++ {
		l.Reward("p1")
	}
	if l.Rate("p1") != 4 || l.Throttled() != 0 {
		t.Errorf("rate should recover to exactly the configured 4, got %v", l.Rate("p1"))
	}
}

func TestLimiter_AdaptiveDefaultFloor(t *testing.T) {
	l := NewAdaptive(10, 1, 0)
	for i := 0; i < 20; i++ {
		l.Penalize("k")
	}
	if l.Rate("k") != 1 {
		t.Errorf("default floor should be rps/10 = 1, got %v", l.Rate("k"))
	}
}

func TestLimiter_PenaltySlowsWaits(t *testing.T) {
	l := NewAdaptive(40, 1, 5)
	ctx := context.Background()
	l.Wait(ctx, "k")
	l.Penalize("k") // 40 -> 20 rps: 50ms between requests
	start := time.Now()
	l.Wait(ctx, "k")
	l.Wait(ctx, "k")
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Errorf("penalized key should pace at ~20 rps, two waits took %s", elapsed)
	}
}
