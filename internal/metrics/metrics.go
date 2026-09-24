// Package metrics tracks throughput counters for the harvest run. In
// production these would be exported as Prometheus counters/histograms;
// here they are printed periodically so the demo shows the same signals
// (success rate, req/sec, dropped jobs) an on-call engineer would watch.
package metrics

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Counters holds run-wide atomic counters.
type Counters struct {
	Success uint64
	Failure uint64
	Dropped uint64
	Retried uint64
}

// IncSuccess records one successful fetch+parse.
func (c *Counters) IncSuccess() { atomic.AddUint64(&c.Success, 1) }

// IncFailure records one failed fetch attempt.
func (c *Counters) IncFailure() { atomic.AddUint64(&c.Failure, 1) }

// IncDropped records one job that exhausted all retries.
func (c *Counters) IncDropped() { atomic.AddUint64(&c.Dropped, 1) }

// IncRetried records one retry attempt.
func (c *Counters) IncRetried() { atomic.AddUint64(&c.Retried, 1) }

// Snapshot is a point-in-time read of all counters.
type Snapshot struct {
	Success, Failure, Dropped, Retried uint64
}

// Snapshot reads the current counter values.
func (c *Counters) Snapshot() Snapshot {
	return Snapshot{
		Success: atomic.LoadUint64(&c.Success),
		Failure: atomic.LoadUint64(&c.Failure),
		Dropped: atomic.LoadUint64(&c.Dropped),
		Retried: atomic.LoadUint64(&c.Retried),
	}
}

// StartReporter prints a throughput line every interval until ctx is done,
// returning a channel that is closed once the reporter has stopped (so
// callers can flush a final line after cancellation).
func (c *Counters) StartReporter(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		start := time.Now()
		for {
			select {
			case <-ctx.Done():
				c.report(time.Since(start))
				return
			case <-ticker.C:
				c.report(time.Since(start))
			}
		}
	}()
	return done
}

func (c *Counters) report(elapsed time.Duration) {
	s := c.Snapshot()
	total := s.Success + s.Dropped
	rps := float64(0)
	if elapsed.Seconds() > 0 {
		rps = float64(s.Success) / elapsed.Seconds()
	}
	dailyProjection := rps * 86400
	fmt.Printf(
		"[metrics] elapsed=%s success=%d failure=%d retried=%d dropped=%d completed=%d req/s=%.2f projected/day=%.0f\n",
		elapsed.Round(time.Second), s.Success, s.Failure, s.Retried, s.Dropped, total, rps, dailyProjection,
	)
}
