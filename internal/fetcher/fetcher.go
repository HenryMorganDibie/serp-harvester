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
	Query string
	// URL, if set, is fetched as-is instead of a search for Query at the
	// fetcher's endpoint (Query, Country and Language are then not added
	// to it). The web crawler uses this.
	URL       string
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
	// WaitSelector, if set, overrides PlaywrightConfig.WaitSelector for
	// this fetch. Ignored by fetchers that don't render.
	WaitSelector string
}

// Response is the raw result of a fetch.
type Response struct {
	StatusCode int
	Body       []byte
	Latency    time.Duration
	// ContentType is the response's Content-Type header, when known.
	ContentType string
	// FinalURL is the URL after redirects, when known.
	FinalURL string
}

// Fetcher retrieves raw SERP HTML for a query, or a page by URL.
type Fetcher interface {
	Fetch(ctx context.Context, req Request) (*Response, error)
}
