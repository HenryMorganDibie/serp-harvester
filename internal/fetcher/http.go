package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// HTTPFetcher issues a plain HTTP GET against a search endpoint. It is
// intentionally minimal: standard headers, a cookie jar so a consent
// decision persists across requests, no fingerprint spoofing, no headless
// rendering, no CAPTCHA handling. At any real volume Google will challenge
// or block requests that look like this — that is expected, and is exactly
// the gap a production engagement would close with a compliant,
// client-approved fetch layer built to the client's legal/ToS posture
// rather than assumed.
//
// What it does do is report what came back precisely: a 429 is a
// RateLimitError (with Retry-After), a CAPTCHA / "unusual traffic" page or
// a JS-check interstitial is a BlockedError, other non-2xx statuses are a
// StatusError, and network failures are classified by Classify. A consent
// page is dismissed by submitting that page's own "reject all" form, the
// same choice PlaywrightFetcher makes by clicking it. The worker pool uses
// those distinctions for retries, proxy cooldowns and throttling.
type HTTPFetcher struct {
	Endpoint string // e.g. "https://www.google.com/search"
	Timeout  time.Duration

	// OnConsentHandled, if set, is called after a consent page was
	// dismissed (for metrics).
	OnConsentHandled func()

	// Generic switches block detection from Google's pages to those of
	// bot-protection vendors on arbitrary sites (classifyGenericPage). Set
	// by the web crawler.
	Generic bool
	// DetectAppShells, with Generic, reports a client-side app shell
	// (looksLikeJSShell) as an interstitial so a FailoverFetcher renders
	// it in the browser. Leave it off when there is no browser to fall
	// back to: the shell is then returned as the page it is.
	DetectAppShells bool

	jar *cookiejar.Jar

	// transports holds one *http.Transport per proxy URL ("" = direct), so
	// connections are pooled per egress identity instead of rebuilt for
	// every request.
	transports sync.Map
}

// NewHTTPFetcher builds a fetcher against endpoint with the given per-request
// timeout. It seeds a cookie jar with a consent decision for endpoint's host
// so requests land on a results page instead of the "before you continue"
// consent interstitial — this affects only that regulatory consent screen,
// not any bot-detection mechanism.
func NewHTTPFetcher(endpoint string, timeout time.Duration) (*HTTPFetcher, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("fetcher: create cookie jar: %w", err)
	}

	if u, err := url.Parse(endpoint); err == nil {
		// Host-only cookie (no Domain attribute) so it applies solely to
		// this endpoint's host, not every google.com subdomain.
		jar.SetCookies(u, []*http.Cookie{{Name: "CONSENT", Value: "YES+"}})
	}

	return &HTTPFetcher{Endpoint: endpoint, Timeout: timeout, jar: jar}, nil
}

// Fetch performs the GET request for req.
func (f *HTTPFetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	transport, err := f.transportFor(req.ProxyURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: f.Timeout, Jar: f.jar, Transport: transport}

	u := req.URL
	if u == "" {
		q := url.Values{}
		q.Set("q", req.Query)
		if req.Country != "" {
			q.Set("gl", req.Country)
		}
		if req.Language != "" {
			q.Set("hl", req.Language)
		}
		u = f.Endpoint + "?" + q.Encode()
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("fetcher: build request: %w", err)
	}
	setHeaders(httpReq, req.UserAgent)

	start := time.Now()
	status, finalURL, header, body, err := do(client, httpReq)
	if err != nil {
		return nil, err
	}

	contentType := header.Get("Content-Type")
	kind := pageNormal
	if isHTML(contentType) {
		kind = f.classify(finalURL.String(), status, string(body))
	}
	if kind == pageConsent {
		consentReq, ok, err := rejectConsentRequest(ctx, finalURL, body, req.UserAgent)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, blockedErrorFor(pageConsent, finalURL.String(), status, string(body))
		}
		if status, finalURL, header, body, err = do(client, consentReq); err != nil {
			return nil, err
		}
		if f.OnConsentHandled != nil {
			f.OnConsentHandled()
		}
		contentType = header.Get("Content-Type")
		kind = f.classify(finalURL.String(), status, string(body))
	}
	if kind == pageNormal && isHTML(contentType) && f.needsJS(string(body)) {
		// The page only works with JavaScript; hybrid mode renders it in
		// the browser instead.
		kind = pageInterstitial
	}
	if blocked := blockedErrorFor(kind, finalURL.String(), status, string(body)); blocked != nil {
		return nil, blocked
	}

	switch {
	case status == http.StatusTooManyRequests:
		retryAfter, _ := parseRetryAfter(header.Get("Retry-After"))
		return nil, &RateLimitError{StatusCode: status, RetryAfter: retryAfter, Body: truncate(string(body), 512)}
	case status >= 400:
		return nil, &StatusError{StatusCode: status}
	}

	return &Response{
		StatusCode:  status,
		Body:        body,
		Latency:     time.Since(start),
		ContentType: contentType,
		FinalURL:    finalURL.String(),
	}, nil
}

func (f *HTTPFetcher) classify(pageURL string, status int, html string) pageKind {
	if f.Generic {
		return classifyGenericPage(pageURL, status, html)
	}
	return classifyPage(pageURL, html)
}

func (f *HTTPFetcher) needsJS(rawHTML string) bool {
	if f.Generic {
		return f.DetectAppShells && looksLikeJSShell(rawHTML)
	}
	return requiresJS(rawHTML)
}

// transportFor returns the pooled transport for proxyURL.
func (f *HTTPFetcher) transportFor(proxyURL string) (*http.Transport, error) {
	if t, ok := f.transports.Load(proxyURL); ok {
		return t.(*http.Transport), nil
	}
	t := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("fetcher: invalid proxy url: %w", err)
		}
		t.Proxy = http.ProxyURL(parsed)
	}
	actual, _ := f.transports.LoadOrStore(proxyURL, t)
	return actual.(*http.Transport), nil
}

func setHeaders(r *http.Request, userAgent string) {
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (compatible; web-harvester/0.1)"
	}
	r.Header.Set("User-Agent", userAgent)
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
}

// do sends r and reads up to 5MB of the body, returning the status, the
// URL after redirects and the headers.
func do(client *http.Client, r *http.Request) (int, *url.URL, http.Header, []byte, error) {
	resp, err := client.Do(r)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("fetcher: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return 0, nil, nil, nil, &TransportError{Err: fmt.Errorf("read body: %w", err)}
	}
	return resp.StatusCode, resp.Request.URL, resp.Header, body, nil
}

// rejectConsentRequest builds the submission of a consent page's "reject
// all" form: its inputs posted to its action, as a browser would on a
// click. ok is false if the page has no such form.
func rejectConsentRequest(ctx context.Context, pageURL *url.URL, body []byte, userAgent string) (*http.Request, bool, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, false, nil
	}
	form := doc.Find(rejectConsentForm).First()
	if form.Length() == 0 {
		return nil, false, nil
	}

	action, _ := form.Attr("action")
	target, err := pageURL.Parse(action)
	if err != nil {
		return nil, false, nil
	}
	values := url.Values{}
	form.Find("input[name]").Each(func(_ int, in *goquery.Selection) {
		name, _ := in.Attr("name")
		value, _ := in.Attr("value")
		values.Add(name, value)
	})

	method := http.MethodPost
	if m, ok := form.Attr("method"); ok && strings.EqualFold(m, http.MethodGet) {
		method = http.MethodGet
	}
	var r *http.Request
	if method == http.MethodGet {
		target.RawQuery = values.Encode()
		r, err = http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	} else {
		r, err = http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(values.Encode()))
		if err == nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return nil, false, fmt.Errorf("fetcher: build consent request: %w", err)
	}
	setHeaders(r, userAgent)
	return r, true, nil
}
