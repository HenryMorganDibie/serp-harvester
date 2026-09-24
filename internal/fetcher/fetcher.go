// Package fetcher defines how raw SERP HTML is retrieved. Two
// implementations are provided: Mock (deterministic, offline, used by tests
// and `--mode mock`) and HTTP (a plain, undisguised HTTP client used by
// `--mode live`). Neither implementation attempts browser fingerprint
// spoofing, CAPTCHA solving, or any other anti-detection technique — see
// the README for why that line is drawn deliberately.
package fetcher

import (
	"context"
	"time"
)

// Request describes one fetch.
type Request struct {
	Query     string
	UserAgent string
	ProxyURL  string // empty = direct connection
}

// Response is the raw result of a fetch.
type Response struct {
	StatusCode int
	Body       []byte
	Latency    time.Duration
}

// Fetcher retrieves raw SERP HTML for a query.
type Fetcher interface {
	Fetch(ctx context.Context, req Request) (*Response, error)
}
