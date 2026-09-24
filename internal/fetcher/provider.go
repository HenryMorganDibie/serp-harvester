package fetcher

import (
	"context"
	"encoding/json"
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

// Fetch performs the GET request for req and returns the raw JSON body. If
// the response's ai_overview is a page_token stub rather than inline
// content, it transparently makes the documented follow-up request
// (engine=google_ai_overview) and merges the resolved content back in,
// so parser.JSONParser never needs to know a two-step fetch happened.
func (f *ProviderFetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()

	q := url.Values{}
	q.Set("engine", f.Engine)
	q.Set("q", req.Query)
	body, status, headers, err := f.doGet(ctx, req.ProxyURL, q)
	if err != nil {
		return nil, fmt.Errorf("provider fetcher: request failed: %w", err)
	}
	if status == http.StatusTooManyRequests {
		retryAfter, _ := parseRetryAfter(headers.Get("Retry-After"))
		return nil, &RateLimitError{StatusCode: status, RetryAfter: retryAfter, Body: string(body)}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("provider fetcher: unexpected status %d: %s", status, string(body))
	}

	body = f.resolveAIOverview(ctx, req.ProxyURL, body)

	return &Response{StatusCode: status, Body: body, Latency: time.Since(start)}, nil
}

// resolveAIOverview checks the response for an ai_overview that's only a
// page_token stub (no inline text_blocks yet) and, if found, fetches and
// merges in the full content via the documented follow-up request. Any
// failure along this path (expired token, network error, unexpected shape)
// is non-fatal: it returns the original body unchanged, so organic results
// and everything else still parse — the result just comes back with no
// AIOverview for that query, the same as if the SERP genuinely had none.
//
// This has been verified against a synthetic two-request fixture
// (provider_test.go), not a live provider key — see README "Third-party
// provider fetcher" for what a real-key smoke test should confirm before
// this is relied on for a client.
func (f *ProviderFetcher) resolveAIOverview(ctx context.Context, proxyURL string, body []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}

	rawAIOverview, ok := top["ai_overview"]
	if !ok {
		return body
	}

	var stub struct {
		PageToken  string            `json:"page_token"`
		TextBlocks []json.RawMessage `json:"text_blocks"`
	}
	if err := json.Unmarshal(rawAIOverview, &stub); err != nil {
		return body
	}
	if stub.PageToken == "" || len(stub.TextBlocks) > 0 {
		return body // no follow-up needed: no token, or content already inline
	}

	resolved, err := f.fetchAIOverviewPage(ctx, proxyURL, stub.PageToken)
	if err != nil {
		return body
	}

	top["ai_overview"] = resolved
	merged, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return merged
}

// fetchAIOverviewPage performs the documented follow-up request
// (engine=google_ai_overview&page_token=...) and returns a JSON value
// shaped like providerResponse.AIOverview (text_blocks/references at the
// top level). Per SerpApi's docs, page_token expires ~4 minutes after the
// original search, so this should be called promptly.
func (f *ProviderFetcher) fetchAIOverviewPage(ctx context.Context, proxyURL, pageToken string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("engine", "google_ai_overview")
	q.Set("page_token", pageToken)
	body, status, _, err := f.doGet(ctx, proxyURL, q)
	if err != nil {
		return nil, fmt.Errorf("ai_overview follow-up request failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("ai_overview follow-up status %d: %s", status, string(body))
	}

	// Documented as text_blocks/references at the top level; defensively
	// also check for a nested "ai_overview" object in case a real
	// provider's shape differs from what's documented, since this is
	// unverified against a live key.
	var withWrapper struct {
		AIOverview json.RawMessage `json:"ai_overview"`
	}
	if err := json.Unmarshal(body, &withWrapper); err == nil && len(withWrapper.AIOverview) > 0 {
		return withWrapper.AIOverview, nil
	}
	return body, nil
}

// doGet performs one GET against BaseURL with params plus engine/api_key
// conventions applied, honoring proxyURL if set. It returns the response
// headers alongside the body/status so callers can read signals like
// Retry-After without a second round trip.
func (f *ProviderFetcher) doGet(ctx context.Context, proxyURL string, params url.Values) ([]byte, int, http.Header, error) {
	client := &http.Client{Timeout: f.Timeout}
	if proxyURL != "" {
		pu, err := url.Parse(proxyURL)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(pu)}
	}

	if f.APIKey != "" {
		params.Set("api_key", f.APIKey)
	}
	u := f.BaseURL + "?" + params.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read body: %w", err)
	}

	return body, resp.StatusCode, resp.Header, nil
}
