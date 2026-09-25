package fetcher

import (
	"context"
	"errors"
	"io"
)

// FailoverFetcher tries Primary and, when Primary's result is one of the
// FailoverOn outcomes, retries the same request on Secondary. It backs
// mode: hybrid: a plain HTTP fetch first, and a browser only for pages that
// need one (a JS-execution check, or a consent wall the HTTP fetcher could
// not dismiss). Rendering a page that requires JavaScript is what a browser
// is for; it is not a way around a block, and CAPTCHA / "unusual traffic"
// pages are deliberately not in the default set: those go back to the
// worker pool as blocked, for proxy cooldown and retry elsewhere.
type FailoverFetcher struct {
	Primary, Secondary Fetcher

	// FailoverOn selects the Primary outcomes that trigger Secondary.
	// Nil means DefaultFailoverOn.
	FailoverOn func(Outcome) bool

	// OnFailover, if set, is called before each Secondary attempt (for
	// metrics).
	OnFailover func(Outcome)
}

// DefaultFailoverOn fails over on interstitials (JS checks) and
// undismissed consent walls.
func DefaultFailoverOn(o Outcome) bool {
	return o == OutcomeInterstitial || o == OutcomeConsent
}

// Fetch implements Fetcher.
func (f *FailoverFetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	resp, err := f.Primary.Fetch(ctx, req)
	if err == nil || ctx.Err() != nil {
		return resp, err
	}
	outcome := Classify(err)
	on := f.FailoverOn
	if on == nil {
		on = DefaultFailoverOn
	}
	if !on(outcome) {
		return nil, err
	}
	if f.OnFailover != nil {
		f.OnFailover(outcome)
	}
	return f.Secondary.Fetch(ctx, req)
}

// Close closes whichever of Primary and Secondary are io.Closers.
func (f *FailoverFetcher) Close() error {
	var errs []error
	for _, x := range []Fetcher{f.Primary, f.Secondary} {
		if c, ok := x.(io.Closer); ok {
			errs = append(errs, c.Close())
		}
	}
	return errors.Join(errs...)
}
