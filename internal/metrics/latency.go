package metrics

import (
	"sort"
	"sync"
	"time"
)

// LatencyRecorder keeps a bounded reservoir of observed latencies and
// computes approximate percentiles from it. It's a fixed-size ring buffer,
// not a full history: at high sustained throughput (the whole point of a
// soak test) keeping every observation would grow memory without bound, so
// this trades exact percentiles for a bounded-memory approximation over the
// most recent Capacity observations. Good enough to spot real latency
// distribution shape and regressions; not a substitute for a real
// timeseries store in production.
type LatencyRecorder struct {
	mu       sync.Mutex
	samples  []time.Duration
	idx      int
	capacity int
	total    uint64 // total observations ever recorded, including evicted ones
}

// NewLatencyRecorder builds a recorder holding up to capacity samples.
func NewLatencyRecorder(capacity int) *LatencyRecorder {
	if capacity <= 0 {
		capacity = 1
	}
	return &LatencyRecorder{samples: make([]time.Duration, 0, capacity), capacity: capacity}
}

// Record adds one observation.
func (r *LatencyRecorder) Record(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	if len(r.samples) < r.capacity {
		r.samples = append(r.samples, d)
		return
	}
	r.samples[r.idx] = d
	r.idx = (r.idx + 1) % r.capacity
}

// LatencyStats is a snapshot of the recorder's current distribution.
type LatencyStats struct {
	Count                   int    // observations currently held in the reservoir
	TotalRecorded           uint64 // observations ever recorded, including evicted ones
	Min, P50, P95, P99, Max time.Duration
}

// Stats computes the current percentile snapshot. O(n log n) in the
// reservoir size, so call it for reporting, not on every request.
func (r *LatencyRecorder) Stats() LatencyStats {
	r.mu.Lock()
	sorted := append([]time.Duration(nil), r.samples...)
	total := r.total
	r.mu.Unlock()

	n := len(sorted)
	if n == 0 {
		return LatencyStats{TotalRecorded: total}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	pick := func(p float64) time.Duration {
		idx := int(p * float64(n))
		if idx >= n {
			idx = n - 1
		}
		return sorted[idx]
	}

	return LatencyStats{
		Count:         n,
		TotalRecorded: total,
		Min:           sorted[0],
		P50:           pick(0.50),
		P95:           pick(0.95),
		P99:           pick(0.99),
		Max:           sorted[n-1],
	}
}
