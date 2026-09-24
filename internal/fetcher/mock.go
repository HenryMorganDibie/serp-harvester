package fetcher

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Mock is a deterministic, offline fetcher that cycles through fixture HTML
// files. It powers `--mode mock` and the test suite so the full pipeline
// (queue -> worker -> proxy -> rate limit -> fetch -> parse -> sink) can be
// exercised and demoed with zero network calls and zero risk of tripping
// Google's abuse detection.
type Mock struct {
	fixtures [][]byte
	counter  uint64

	// FailEvery, if > 0, makes every Nth call return an error to exercise
	// the worker pool's retry/backoff path.
	FailEvery uint64
}

// NewMockFromDir loads every *.html file in dir as a fixture.
func NewMockFromDir(dir string) (*Mock, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("mock fetcher: read dir: %w", err)
	}
	var fixtures [][]byte
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".html" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("mock fetcher: read fixture %s: %w", e.Name(), err)
		}
		fixtures = append(fixtures, b)
	}
	if len(fixtures) == 0 {
		return nil, fmt.Errorf("mock fetcher: no .html fixtures found in %s", dir)
	}
	return &Mock{fixtures: fixtures}, nil
}

// Fetch returns the next fixture in rotation, simulating realistic latency.
func (m *Mock) Fetch(ctx context.Context, req Request) (*Response, error) {
	n := atomic.AddUint64(&m.counter, 1)
	start := time.Now()

	// Simulate network latency so the metrics/backoff paths behave
	// realistically in the demo.
	select {
	case <-time.After(time.Duration(20+rand.Intn(60)) * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	latency := time.Since(start)

	if m.FailEvery > 0 && n%m.FailEvery == 0 {
		return nil, fmt.Errorf("mock fetcher: simulated failure for %q", req.Query)
	}

	body := m.fixtures[int(n-1)%len(m.fixtures)]
	return &Response{
		StatusCode: 200,
		Body:       body,
		Latency:    latency,
	}, nil
}
