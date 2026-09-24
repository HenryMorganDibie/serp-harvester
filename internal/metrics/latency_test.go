package metrics

import (
	"testing"
	"time"
)

func TestLatencyRecorder_Percentiles(t *testing.T) {
	r := NewLatencyRecorder(1000)
	for i := 1; i <= 100; i++ {
		r.Record(time.Duration(i) * time.Millisecond)
	}

	stats := r.Stats()
	if stats.Count != 100 {
		t.Errorf("expected 100 samples, got %d", stats.Count)
	}
	if stats.Min != 1*time.Millisecond {
		t.Errorf("expected min 1ms, got %v", stats.Min)
	}
	if stats.Max != 100*time.Millisecond {
		t.Errorf("expected max 100ms, got %v", stats.Max)
	}
	// p50 of 1..100ms should land near 50ms.
	if stats.P50 < 45*time.Millisecond || stats.P50 > 55*time.Millisecond {
		t.Errorf("expected p50 near 50ms, got %v", stats.P50)
	}
	if stats.P99 < 95*time.Millisecond {
		t.Errorf("expected p99 near the top of the range, got %v", stats.P99)
	}
}

func TestLatencyRecorder_EvictsOldestWhenFull(t *testing.T) {
	r := NewLatencyRecorder(10)
	for i := 1; i <= 20; i++ {
		r.Record(time.Duration(i) * time.Millisecond)
	}

	stats := r.Stats()
	if stats.Count != 10 {
		t.Fatalf("expected reservoir capped at 10, got %d", stats.Count)
	}
	if stats.TotalRecorded != 20 {
		t.Errorf("expected TotalRecorded to track all 20 observations, got %d", stats.TotalRecorded)
	}
	// The reservoir should hold the most recent 10 (11ms..20ms), so min
	// should be 11ms, not 1ms.
	if stats.Min != 11*time.Millisecond {
		t.Errorf("expected min of most recent window to be 11ms, got %v", stats.Min)
	}
}

func TestLatencyRecorder_EmptyIsSafe(t *testing.T) {
	r := NewLatencyRecorder(10)
	stats := r.Stats()
	if stats.Count != 0 || stats.TotalRecorded != 0 {
		t.Errorf("expected zero-value stats for an empty recorder, got %+v", stats)
	}
}
