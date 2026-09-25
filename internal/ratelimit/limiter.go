// Package ratelimit provides per-key (typically per-proxy or per-egress-IP)
// token-bucket rate limiting so a worker pool never exceeds a safe request
// rate against a single upstream identity, regardless of how many workers
// are running concurrently.
//
// A limiter built with NewAdaptive also adapts each key's rate (AIMD):
// Penalize, called when the target rate-limits or blocks that key, halves
// its rate down to a floor; Reward, called on success, raises it back
// additively toward the configured rate. A key that is being pushed back
// therefore slows down on its own, instead of every worker continuing at
// the full configured rate until the proxy is cooled.
package ratelimit

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// Limiter holds one token bucket per key.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	rps     float64
	burst   int

	// Adaptive settings; adaptive is false for New.
	adaptive bool
	minRPS   float64
	step     float64 // additive increase per Reward
}

// New creates a Limiter where every key gets its own bucket refilling at rps
// requests/second with the given burst capacity. Penalize and Reward are
// no-ops on it.
func New(rps float64, burst int) *Limiter {
	return &Limiter{
		buckets: make(map[string]*rate.Limiter),
		rps:     rps,
		burst:   burst,
	}
}

// NewAdaptive is New plus AIMD adaptation per key between minRPS and rps.
// minRPS <= 0 defaults to rps/10. Each Reward raises a penalized key's rate
// by 5% of rps, so a key halved once recovers after ~10 successes.
func NewAdaptive(rps float64, burst int, minRPS float64) *Limiter {
	l := New(rps, burst)
	if minRPS <= 0 || minRPS > rps {
		minRPS = rps / 10
	}
	l.adaptive = true
	l.minRPS = minRPS
	l.step = rps * 0.05
	return l
}

// Wait blocks until a token is available for key, or ctx is done.
func (l *Limiter) Wait(ctx context.Context, key string) error {
	return l.bucketFor(key).Wait(ctx)
}

// Penalize halves key's rate (not below the floor). It reports whether the
// rate actually decreased; always false for a non-adaptive limiter.
func (l *Limiter) Penalize(key string) bool {
	if !l.adaptive {
		return false
	}
	b := l.bucketFor(key)
	cur := float64(b.Limit())
	next := cur / 2
	if next < l.minRPS {
		next = l.minRPS
	}
	if next >= cur {
		return false
	}
	b.SetLimit(rate.Limit(next))
	return true
}

// Reward raises key's rate additively toward the configured rate. No-op for
// a non-adaptive limiter or a key already at full rate.
func (l *Limiter) Reward(key string) {
	if !l.adaptive {
		return
	}
	b := l.bucketFor(key)
	cur := float64(b.Limit())
	if cur >= l.rps {
		return
	}
	next := cur + l.step
	if next > l.rps {
		next = l.rps
	}
	b.SetLimit(rate.Limit(next))
}

// Rate returns key's current rate in requests/second.
func (l *Limiter) Rate(key string) float64 {
	return float64(l.bucketFor(key).Limit())
}

// Throttled returns how many keys currently run below the configured rate.
func (l *Limiter) Throttled() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, b := range l.buckets {
		if float64(b.Limit()) < l.rps {
			n++
		}
	}
	return n
}

func (l *Limiter) bucketFor(key string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = rate.NewLimiter(rate.Limit(l.rps), l.burst)
		l.buckets[key] = b
	}
	return b
}
