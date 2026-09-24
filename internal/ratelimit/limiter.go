// Package ratelimit provides per-key (typically per-proxy or per-egress-IP)
// token-bucket rate limiting so a worker pool never exceeds a safe request
// rate against a single upstream identity, regardless of how many workers
// are running concurrently.
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
}

// New creates a Limiter where every key gets its own bucket refilling at rps
// requests/second with the given burst capacity.
func New(rps float64, burst int) *Limiter {
	return &Limiter{
		buckets: make(map[string]*rate.Limiter),
		rps:     rps,
		burst:   burst,
	}
}

// Wait blocks until a token is available for key, or ctx is done.
func (l *Limiter) Wait(ctx context.Context, key string) error {
	return l.bucketFor(key).Wait(ctx)
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
