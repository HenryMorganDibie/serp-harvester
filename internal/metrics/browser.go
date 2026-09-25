package metrics

import "sync/atomic"

// BrowserCounters holds counters specific to the headless-browser fetch
// path (fetcher.PlaywrightFetcher). It is attached to Counters.Browser only
// when mode: playwright is active, so the other modes' /metrics output is
// unchanged. Every method is nil-safe: a fetcher built without metrics can
// call them unconditionally.
type BrowserCounters struct {
	// Launches counts Chromium processes started, including relaunches
	// after a crash or disconnect. More than 1 in a long run means the
	// browser has been recycled.
	Launches uint64
	// Disconnects counts unexpected browser disconnects (crash, OOM kill),
	// not the intentional close at shutdown.
	Disconnects uint64
	// NavigationTimeouts counts fetches whose page load exceeded
	// request_timeout.
	NavigationTimeouts uint64
	// ConsentHandled counts cookie-consent interstitials dismissed by
	// submitting the page's own "reject all" form.
	ConsentHandled uint64
	// BlockedCaptcha, BlockedConsent and BlockedInterstitial count pages
	// classified as a block the fetcher does not get past: a CAPTCHA /
	// "unusual traffic" page, a consent page with no usable reject form,
	// or a JS-check interstitial that persisted after rendering.
	BlockedCaptcha      uint64
	BlockedConsent      uint64
	BlockedInterstitial uint64

	// LaunchFailures counts failed Chromium launches (each one starts a
	// launch backoff). PageCrashes counts renderer crashes of a page.
	LaunchFailures uint64
	PageCrashes    uint64

	// SessionsOpen and SessionsInUse are gauges: browser sessions (one
	// context + page each) currently open, and currently serving a fetch.
	SessionsOpen  int64
	SessionsInUse int64
}

// IncLaunches records one browser launch.
func (b *BrowserCounters) IncLaunches() {
	if b != nil {
		atomic.AddUint64(&b.Launches, 1)
	}
}

// IncDisconnects records one unexpected browser disconnect.
func (b *BrowserCounters) IncDisconnects() {
	if b != nil {
		atomic.AddUint64(&b.Disconnects, 1)
	}
}

// IncLaunchFailures records one failed browser launch.
func (b *BrowserCounters) IncLaunchFailures() {
	if b != nil {
		atomic.AddUint64(&b.LaunchFailures, 1)
	}
}

// IncPageCrashes records one page (renderer) crash.
func (b *BrowserCounters) IncPageCrashes() {
	if b != nil {
		atomic.AddUint64(&b.PageCrashes, 1)
	}
}

// IncNavigationTimeouts records one navigation that hit its timeout.
func (b *BrowserCounters) IncNavigationTimeouts() {
	if b != nil {
		atomic.AddUint64(&b.NavigationTimeouts, 1)
	}
}

// IncConsentHandled records one consent interstitial dismissed.
func (b *BrowserCounters) IncConsentHandled() {
	if b != nil {
		atomic.AddUint64(&b.ConsentHandled, 1)
	}
}

// IncBlocked records one page classified as a block, by reason: "captcha",
// "consent", or "interstitial". Unknown reasons count as interstitial.
func (b *BrowserCounters) IncBlocked(reason string) {
	if b == nil {
		return
	}
	switch reason {
	case "captcha":
		atomic.AddUint64(&b.BlockedCaptcha, 1)
	case "consent":
		atomic.AddUint64(&b.BlockedConsent, 1)
	default:
		atomic.AddUint64(&b.BlockedInterstitial, 1)
	}
}

// AddSessionsOpen adjusts the open-sessions gauge by delta.
func (b *BrowserCounters) AddSessionsOpen(delta int64) {
	if b != nil {
		atomic.AddInt64(&b.SessionsOpen, delta)
	}
}

// AddSessionsInUse adjusts the in-use-sessions gauge by delta.
func (b *BrowserCounters) AddSessionsInUse(delta int64) {
	if b != nil {
		atomic.AddInt64(&b.SessionsInUse, delta)
	}
}

// BrowserSnapshot is a point-in-time read of BrowserCounters.
type BrowserSnapshot struct {
	Launches, Disconnects, NavigationTimeouts, ConsentHandled uint64
	BlockedCaptcha, BlockedConsent, BlockedInterstitial       uint64
	LaunchFailures, PageCrashes                               uint64
	SessionsOpen, SessionsInUse                               int64
}

// Snapshot reads the current values. A nil receiver yields a zero snapshot.
func (b *BrowserCounters) Snapshot() BrowserSnapshot {
	if b == nil {
		return BrowserSnapshot{}
	}
	return BrowserSnapshot{
		Launches:            atomic.LoadUint64(&b.Launches),
		Disconnects:         atomic.LoadUint64(&b.Disconnects),
		NavigationTimeouts:  atomic.LoadUint64(&b.NavigationTimeouts),
		ConsentHandled:      atomic.LoadUint64(&b.ConsentHandled),
		BlockedCaptcha:      atomic.LoadUint64(&b.BlockedCaptcha),
		BlockedConsent:      atomic.LoadUint64(&b.BlockedConsent),
		BlockedInterstitial: atomic.LoadUint64(&b.BlockedInterstitial),
		LaunchFailures:      atomic.LoadUint64(&b.LaunchFailures),
		PageCrashes:         atomic.LoadUint64(&b.PageCrashes),
		SessionsOpen:        atomic.LoadInt64(&b.SessionsOpen),
		SessionsInUse:       atomic.LoadInt64(&b.SessionsInUse),
	}
}
