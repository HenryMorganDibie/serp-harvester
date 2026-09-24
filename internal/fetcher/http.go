package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HTTPFetcher issues a plain HTTP GET against a search endpoint. It is
// intentionally minimal: standard headers, no fingerprint spoofing, no
// headless rendering, no CAPTCHA handling. At any real volume Google will
// challenge or block requests that look like this — that is expected, and
// is exactly the gap a production engagement would close with a compliant,
// client-approved fetch layer (rendering, residential egress, CAPTCHA
// handling) built to the client's legal/ToS posture rather than assumed.
type HTTPFetcher struct {
	Endpoint string // e.g. "https://www.google.com/search"
	Timeout  time.Duration
}

// NewHTTPFetcher builds a fetcher against endpoint with the given per-request
// timeout.
func NewHTTPFetcher(endpoint string, timeout time.Duration) *HTTPFetcher {
	return &HTTPFetcher{Endpoint: endpoint, Timeout: timeout}
}

// Fetch performs the GET request for req.
func (f *HTTPFetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	client := &http.Client{Timeout: f.Timeout}
	if req.ProxyURL != "" {
		proxyURL, err := url.Parse(req.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("fetcher: invalid proxy url: %w", err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}

	q := url.Values{}
	q.Set("q", req.Query)
	u := f.Endpoint + "?" + q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("fetcher: build request: %w", err)
	}
	ua := req.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (compatible; serp-harvester-sample/0.1)"
	}
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")

	start := time.Now()
	resp, err := client.Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("fetcher: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB cap
	if err != nil {
		return nil, fmt.Errorf("fetcher: read body: %w", err)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Body:       body,
		Latency:    latency,
	}, nil
}
