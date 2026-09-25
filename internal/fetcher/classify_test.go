package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/playwright-community/playwright-go"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("worker context: %w", err) }
	cases := []struct {
		name string
		err  error
		want Outcome
	}{
		{"nil is success", nil, OutcomeSuccess},
		{"429", &RateLimitError{StatusCode: 429}, OutcomeRateLimited},
		{"wrapped 429", wrap(&RateLimitError{StatusCode: 429}), OutcomeRateLimited},
		{"captcha", &BlockedError{Reason: "captcha"}, OutcomeCaptcha},
		{"consent", &BlockedError{Reason: "consent"}, OutcomeConsent},
		{"interstitial", &BlockedError{Reason: "interstitial"}, OutcomeInterstitial},
		{"503", &StatusError{StatusCode: 503}, OutcomeServerError},
		{"404", &StatusError{StatusCode: 404}, OutcomeClientError},
		{"canceled", wrap(context.Canceled), OutcomeCanceled},
		{"deadline", wrap(context.DeadlineExceeded), OutcomeTimeout},
		{"playwright timeout", wrap(playwright.ErrTimeout), OutcomeTimeout},
		{"net timeout", &url.Error{Op: "Get", URL: "x", Err: timeoutErr{}}, OutcomeTimeout},
		{"connection refused", &url.Error{Op: "Get", URL: "x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, OutcomeTransport},
		{"browser net error", &TransportError{Err: errors.New("net::ERR_PROXY_CONNECTION_FAILED")}, OutcomeTransport},
		{"unknown", errors.New("something else"), OutcomeOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.err); got != c.want {
				t.Errorf("Classify(%v) = %s, want %s", c.err, got, c.want)
			}
		})
	}
}

func TestOutcome_RetryableAndBlocked(t *testing.T) {
	for _, o := range []Outcome{OutcomeSuccess, OutcomeClientError, OutcomeCanceled} {
		if o.Retryable() {
			t.Errorf("%s should not be retryable", o)
		}
	}
	for _, o := range []Outcome{OutcomeRateLimited, OutcomeCaptcha, OutcomeConsent, OutcomeInterstitial, OutcomeTimeout, OutcomeTransport, OutcomeServerError, OutcomeOther} {
		if !o.Retryable() {
			t.Errorf("%s should be retryable", o)
		}
	}
	if !OutcomeCaptcha.Blocked() || !OutcomeInterstitial.Blocked() || OutcomeRateLimited.Blocked() || OutcomeConsent.Blocked() {
		t.Error("Blocked() should be true exactly for captcha and interstitial")
	}
	if Outcome(99).String() != "other" {
		t.Error("out-of-range outcome should label as other")
	}
}
