package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ProviderFetcher calls a third-party SERP data API (SerpApi-compatible
// shape: GET {BaseURL}?engine=...&q=...&api_key=...) instead of requesting
// Google directly. The provider has already solved consent walls, CAPTCHAs,
// and proxy rotation as part of their product — this is the fetch path a
// real deployment should use for the Google-facing leg, while everything
// upstream (queue, worker pool, rate limiting, retries, proxy pool) and
// downstream (parsing via parser.JSONParser, calibration, metrics, sink)
// stays exactly what the rest of this repo already builds and tests.
//
// It does not implement HTML scraping, CAPTCHA handling, or anti-bot
// evasion — it's an HTTP client for a paid API, the same shape as a Stripe
// or Twilio client.
type ProviderFetcher struct {
	BaseURL string // e.g. "https://serpapi.com/search"
	APIKey  string
	Engine  string // e.g. "google"
	Timeout time.Duration
}

// NewProviderFetcher builds a fetcher against a SerpApi-compatible endpoint.
// engine defaults to "google" if empty.
func NewProviderFetcher(baseURL, apiKey, engine string, timeout time.Duration) *ProviderFetcher {
	if engine == "" {
		engine = "google"
	}
	return &ProviderFetcher{BaseURL: baseURL, APIKey: apiKey, Engine: engine, Timeout: timeout}
}

// Fetch performs the GET request for req and returns the raw JSON body.
func (f *ProviderFetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	client := &http.Client{Timeout: f.Timeout}
	if req.ProxyURL != "" {
		proxyURL, err := url.Parse(req.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("provider fetcher: invalid proxy url: %w", err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}

	q := url.Values{}
	q.Set("engine", f.Engine)
	q.Set("q", req.Query)
	if f.APIKey != "" {
		q.Set("api_key", f.APIKey)
	}
	u := f.BaseURL + "?" + q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("provider fetcher: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := client.Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("provider fetcher: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, fmt.Errorf("provider fetcher: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider fetcher: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return &Response{StatusCode: resp.StatusCode, Body: body, Latency: latency}, nil
}
