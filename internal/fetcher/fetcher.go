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

	// Country and Language are optional locale hints (e.g. "US", "en").
	// ProviderFetcher passes them through as gl/hl params when set;
	// HTTPFetcher passes Language through as hl. Empty means "provider/
	// target default."
	Country  string
	Language string
	// Device is an optional device hint (e.g. "desktop", "mobile", "tablet").
	Device string
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
