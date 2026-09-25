package fetcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/playwright-community/playwright-go"
)

// This file holds what every live fetcher shares: recognising non-results
// pages, the error types fetchers return, and Classify, which turns any
// fetch error into an Outcome the worker pool acts on (retry or not, which
// proxy penalty, whether to slow down). None of it interacts with a block;
// it only names it.

// BlockedError reports a page the fetcher recognised as a block it does not
// get past. Reason is "captcha", "consent" or "interstitial".
type BlockedError struct {
	Reason     string
	URL        string
	StatusCode int
	// Body is the block page itself (capped at maxBlockedBody), for
	// diagnostics: checking what the target actually served when a
	// classification looks wrong. Not part of Error().
	Body []byte
}

// maxBlockedBody caps BlockedError.Body.
const maxBlockedBody = 256 << 10

func (e *BlockedError) Error() string {
	return fmt.Sprintf("fetcher: blocked by %s page (status %d, url %s)", e.Reason, e.StatusCode, e.URL)
}

// StatusError reports an HTTP status that is neither success nor a rate
// limit (see RateLimitError).
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("fetcher: unexpected status %d", e.StatusCode)
}

// TransportError reports a failure below HTTP: DNS, connect, TLS, proxy or
// browser network errors (net::ERR_*).
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return "fetcher: transport error: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Outcome classifies how a fetch ended.
type Outcome int

const (
	OutcomeSuccess Outcome = iota
	// OutcomeRateLimited: HTTP 429 without a block page.
	OutcomeRateLimited
	// OutcomeCaptcha: a CAPTCHA or "unusual traffic" page.
	OutcomeCaptcha
	// OutcomeConsent: a consent wall the fetcher could not dismiss.
	OutcomeConsent
	// OutcomeInterstitial: a JS-check or similar interstitial.
	OutcomeInterstitial
	OutcomeTimeout
	OutcomeTransport
	// OutcomeServerError: HTTP 5xx.
	OutcomeServerError
	// OutcomeClientError: HTTP 4xx other than 429. Retrying won't help.
	OutcomeClientError
	OutcomeCanceled
	OutcomeOther
)

var outcomeNames = [...]string{
	OutcomeSuccess:      "success",
	OutcomeRateLimited:  "rate_limited",
	OutcomeCaptcha:      "captcha",
	OutcomeConsent:      "consent",
	OutcomeInterstitial: "interstitial",
	OutcomeTimeout:      "timeout",
	OutcomeTransport:    "transport",
	OutcomeServerError:  "http_5xx",
	OutcomeClientError:  "http_4xx",
	OutcomeCanceled:     "canceled",
	OutcomeOther:        "other",
}

// String returns the outcome's metric label.
func (o Outcome) String() string {
	if o < 0 || int(o) >= len(outcomeNames) {
		return "other"
	}
	return outcomeNames[o]
}

// Retryable reports whether another attempt (typically on another proxy or
// after a wait) can reasonably succeed. Client errors and cancellation are
// final.
func (o Outcome) Retryable() bool {
	switch o {
	case OutcomeSuccess, OutcomeClientError, OutcomeCanceled:
		return false
	}
	return true
}

// Blocked reports whether the target answered with a block page (as
// opposed to failing or being slow).
func (o Outcome) Blocked() bool {
	return o == OutcomeCaptcha || o == OutcomeInterstitial
}

// Classify maps a Fetch error to an Outcome. A nil error is
// OutcomeSuccess.
func Classify(err error) Outcome {
	if err == nil {
		return OutcomeSuccess
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return OutcomeRateLimited
	}
	var blocked *BlockedError
	if errors.As(err, &blocked) {
		switch blocked.Reason {
		case "captcha":
			return OutcomeCaptcha
		case "consent":
			return OutcomeConsent
		default:
			return OutcomeInterstitial
		}
	}
	var status *StatusError
	if errors.As(err, &status) {
		if status.StatusCode >= 500 {
			return OutcomeServerError
		}
		return OutcomeClientError
	}
	if errors.Is(err, context.Canceled) {
		return OutcomeCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, playwright.ErrTimeout) {
		return OutcomeTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return OutcomeTimeout
	}
	var transport *TransportError
	var urlErr *url.Error
	if errors.As(err, &transport) || errors.As(err, &urlErr) || errors.As(err, &netErr) {
		return OutcomeTransport
	}
	return OutcomeOther
}

type pageKind int

const (
	pageNormal pageKind = iota
	pageConsent
	pageCaptcha
	pageInterstitial
)

// classifyPage recognises the non-results pages Google is known to serve,
// from the final URL and the (rendered or raw) DOM. It only classifies; it
// never interacts with a CAPTCHA.
func classifyPage(pageURL, html string) pageKind {
	u, _ := url.Parse(pageURL)
	if u != nil {
		switch {
		case strings.Contains(u.Path, "/sorry/"):
			return pageCaptcha
		case strings.Contains(u.Path, "/httpservice/retry/enablejs"):
			return pageInterstitial
		}
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(html)))
	if err != nil {
		return pageNormal
	}
	if doc.Find(`#captcha-form, form[action*="/sorry/"], .g-recaptcha, #recaptcha, iframe[src*="recaptcha"]`).Length() > 0 {
		return pageCaptcha
	}
	if (u != nil && strings.HasPrefix(u.Hostname(), "consent.")) ||
		doc.Find(`form[action*="consent"], input[name="set_eom"]`).Length() > 0 {
		return pageConsent
	}
	return pageNormal
}

var (
	noscriptBlock = regexp.MustCompile(`(?is)<noscript[^>]*>(.*?)</noscript>`)
	metaRefresh   = regexp.MustCompile(`(?i)http-equiv\s*=\s*["']?refresh`)
)

// requiresJS reports whether a raw (unrendered) page is only a JS-check
// shell: its <noscript> fallback meta-refreshes to
// /httpservice/retry/enablejs. Only meaningful for fetchers that don't
// execute scripts; a browser runs the page's script instead.
func requiresJS(rawHTML string) bool {
	for _, m := range noscriptBlock.FindAllStringSubmatch(rawHTML, -1) {
		if metaRefresh.MatchString(m[1]) && strings.Contains(m[1], "/httpservice/retry/enablejs") {
			return true
		}
	}
	return false
}

// blockedErrorFor returns the BlockedError for a non-normal page kind, or
// nil for pageNormal. body is the page as served (or rendered).
func blockedErrorFor(kind pageKind, pageURL string, status int, body string) *BlockedError {
	var reason string
	switch kind {
	case pageCaptcha:
		reason = "captcha"
	case pageConsent:
		reason = "consent"
	case pageInterstitial:
		reason = "interstitial"
	default:
		return nil
	}
	return &BlockedError{Reason: reason, URL: pageURL, StatusCode: status, Body: []byte(truncate(body, maxBlockedBody))}
}

// rejectConsentForm is the consent page's "reject all" form: Google's
// consent forms carry a hidden set_eom input, true for "reject all".
const rejectConsentForm = `form:has(input[name="set_eom"][value="true"])`
