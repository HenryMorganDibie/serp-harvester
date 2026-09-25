package fetcher

import (
	"context"
	"errors"
	"testing"
)

type scriptedFetcher struct {
	err    error
	calls  int
	closed bool
}

func (f *scriptedFetcher) Fetch(context.Context, Request) (*Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &Response{StatusCode: 200, Body: []byte("ok")}, nil
}

func (f *scriptedFetcher) Close() error { f.closed = true; return nil }

func TestFailoverFetcher(t *testing.T) {
	cases := []struct {
		name          string
		primaryErr    error
		wantSecondary bool
	}{
		{"success stays on primary", nil, false},
		{"JS-check interstitial fails over", &BlockedError{Reason: "interstitial"}, true},
		{"undismissed consent fails over", &BlockedError{Reason: "consent"}, true},
		{"captcha is not retried in the browser", &BlockedError{Reason: "captcha"}, false},
		{"rate limit is not retried in the browser", &RateLimitError{StatusCode: 429}, false},
		{"server error is not failed over", &StatusError{StatusCode: 503}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			primary := &scriptedFetcher{err: c.primaryErr}
			secondary := &scriptedFetcher{}
			var failovers []Outcome
			f := &FailoverFetcher{Primary: primary, Secondary: secondary, OnFailover: func(o Outcome) { failovers = append(failovers, o) }}

			_, err := f.Fetch(context.Background(), Request{Query: "q"})
			if got := secondary.calls == 1; got != c.wantSecondary {
				t.Fatalf("secondary called=%v, want %v", got, c.wantSecondary)
			}
			if c.wantSecondary {
				if err != nil || len(failovers) != 1 {
					t.Errorf("err=%v failovers=%v", err, failovers)
				}
			} else if c.primaryErr != nil && !errors.Is(err, c.primaryErr) {
				t.Errorf("primary error should be returned unchanged, got %v", err)
			}
		})
	}
}

func TestFailoverFetcher_CustomPolicyAndClose(t *testing.T) {
	primary := &scriptedFetcher{err: &StatusError{StatusCode: 503}}
	secondary := &scriptedFetcher{}
	f := &FailoverFetcher{Primary: primary, Secondary: secondary, FailoverOn: func(o Outcome) bool { return o == OutcomeServerError }}
	if _, err := f.Fetch(context.Background(), Request{}); err != nil || secondary.calls != 1 {
		t.Fatalf("custom FailoverOn not honored: err=%v calls=%d", err, secondary.calls)
	}
	if err := f.Close(); err != nil || !primary.closed || !secondary.closed {
		t.Errorf("Close should close both fetchers: err=%v", err)
	}
}

func TestFailoverFetcher_NoFailoverAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	secondary := &scriptedFetcher{}
	f := &FailoverFetcher{Primary: &scriptedFetcher{err: &BlockedError{Reason: "interstitial"}}, Secondary: secondary}
	f.Fetch(ctx, Request{})
	if secondary.calls != 0 {
		t.Error("a cancelled fetch must not start the fallback")
	}
}
