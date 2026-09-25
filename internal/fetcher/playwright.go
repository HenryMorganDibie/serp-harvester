package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/playwright-community/playwright-go"

	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
)

// PlaywrightFetcher renders the search page in headless Chromium (driven by
// Playwright) and returns the rendered DOM as HTML, so client-side-rendered
// content reaches the same HTML parser the other HTML fetch paths use.
//
// It is an acquisition backend, nothing more. It does not solve CAPTCHAs,
// spoof or randomize browser fingerprints, patch automation markers, or try
// to evade bot detection. The User-Agent is whatever the caller sends (the
// worker pool's honest serp-harvester strings). When the target serves a
// CAPTCHA, an "unusual traffic" page, or a JS-check interstitial that
// rendering doesn't resolve, Fetch reports it as a *BlockedError, so the
// failure counts against the proxy's health like any other failed request.
// The one interstitial it does act on is the cookie-consent page, which it
// dismisses by submitting that page's own "reject all" form: a regulatory
// choice any visitor makes, not a bot-detection bypass.
//
// Proxy selection and rate limiting stay in worker.Pool: each Fetch uses
// req.ProxyURL as given, exactly as HTTPFetcher does. The browser process is
// launched once and shared; each fetch borrows a session (one browser
// context plus one page) from a bounded pool keyed by proxy, device,
// locale and User-Agent, since those are fixed per context. Sessions are
// reused, so cookies such as a consent decision persist the way
// HTTPFetcher's cookie jar does, and are recycled after MaxSessionUses
// fetches or any failed fetch.
type PlaywrightFetcher struct {
	cfg PlaywrightConfig
	pw  *playwright.Playwright

	// slots bounds concurrent fetches (and therefore busy sessions) to
	// PoolSize. Idle sessions count toward the same cap via open.
	slots chan struct{}

	mu      sync.Mutex
	browser playwright.Browser
	idle    []*browserSession
	open    int

	// Launch backoff: after a failed (re)launch, fetches fail fast with
	// ErrBrowserUnavailable until nextLaunch instead of each retrying a
	// launch that just failed. Guarded by mu.
	launchFailures int
	nextLaunch     time.Time
	lastLaunchErr  error

	// closed is atomic, not guarded by mu, because the browser's
	// disconnect handler reads it from the driver's event goroutine,
	// possibly while mu is held around a launch.
	closed    atomic.Bool
	done      chan struct{} // closed by Close, to wake fetches waiting for a slot
	closeOnce sync.Once
}

// PlaywrightConfig configures NewPlaywrightFetcher.
type PlaywrightConfig struct {
	Endpoint string        // e.g. "https://www.google.com/search"
	Timeout  time.Duration // per-fetch budget: navigation, consent step and WaitSelector together

	// PoolSize caps open browser sessions (context + page). Fetches beyond
	// it wait for a free session or for their context to end. Default 4.
	PoolSize int
	// MaxSessionUses recycles a session after this many fetches. Default 50.
	MaxSessionUses int
	// Headless runs Chromium without a window. The zero value is false, so
	// callers building this directly should set it; config defaults it on.
	Headless bool
	// ExecutablePath points at a specific Chromium binary instead of the
	// one Playwright installed. Optional. Prefer Playwright's headless
	// shell (the default when Headless is set): a full Chrome/Chromium
	// build makes its own background requests to Google services (network
	// time, account checks) that bypass the per-context proxy and the rate
	// limiter.
	ExecutablePath string
	// WaitSelector, if set, is awaited after the page loads (within the
	// remaining Timeout) so results rendered after the load event are
	// present. A page that never shows it is still returned, and the
	// parser's drift detection flags the result.
	WaitSelector string

	// Metrics receives browser counters. Optional.
	Metrics *metrics.BrowserCounters
}

// ErrFetcherClosed is returned by Fetch after Close.
var ErrFetcherClosed = errors.New("playwright fetcher: closed")

// ErrBrowserUnavailable is returned while Chromium is in launch backoff
// after a failed launch or relaunch.
var ErrBrowserUnavailable = errors.New("playwright fetcher: browser unavailable (launch backoff)")

// maxLaunchBackoff caps the wait between relaunch attempts.
const maxLaunchBackoff = 30 * time.Second

type browserSession struct {
	key     string
	browser playwright.Browser
	context playwright.BrowserContext
	page    playwright.Page
	uses    int
	crashed atomic.Bool // set by the page's crash handler
}

// maxBodyBytes matches HTTPFetcher's body cap.
const maxBodyBytes = 5 << 20

// NewPlaywrightFetcher starts the Playwright driver and launches Chromium,
// failing fast if either is missing. It never downloads browsers at
// runtime: install them beforehand with scripts/install-playwright.sh (see
// also deploy/playwright.Dockerfile).
// Call Close to shut the browser down.
func NewPlaywrightFetcher(cfg PlaywrightConfig) (*PlaywrightFetcher, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("playwright fetcher: endpoint is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 4
	}
	if cfg.MaxSessionUses <= 0 {
		cfg.MaxSessionUses = 50
	}

	// Driver output goes to stderr: stdout may be the JSON-Lines sink.
	pw, err := playwright.Run(&playwright.RunOptions{SkipInstallBrowsers: true, Stdout: os.Stderr})
	if err != nil {
		return nil, fmt.Errorf("playwright fetcher: start driver: %w", err)
	}

	f := &PlaywrightFetcher{cfg: cfg, pw: pw, slots: make(chan struct{}, cfg.PoolSize), done: make(chan struct{})}
	f.mu.Lock()
	_, err = f.browserLocked()
	f.mu.Unlock()
	if err != nil {
		pw.Stop()
		return nil, err
	}
	return f, nil
}

// Fetch renders req's search page and returns its HTML.
func (f *PlaywrightFetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	s, err := f.acquire(ctx, req)
	if err != nil {
		return nil, err
	}
	f.cfg.Metrics.AddSessionsInUse(1)

	type outcome struct {
		resp *Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := f.render(s, req)
		done <- outcome{resp, err}
	}()

	select {
	case o := <-done:
		f.release(s, o.err == nil)
		return o.resp, o.err
	case <-ctx.Done():
		// Closing the context aborts the in-flight navigation; wait for
		// render to return so the session is never touched concurrently.
		s.context.Close()
		<-done
		f.release(s, false)
		return nil, ctx.Err()
	}
}

// render performs one navigation on s, including the consent step and the
// optional WaitSelector, within cfg.Timeout.
func (f *PlaywrightFetcher) render(s *browserSession, req Request) (*Response, error) {
	start := time.Now()
	deadline := start.Add(f.cfg.Timeout)
	remainingMS := func() float64 {
		ms := float64(time.Until(deadline).Milliseconds())
		if ms < 1 {
			ms = 1 // Playwright treats 0 as "no timeout"
		}
		return ms
	}

	target, err := searchURL(f.cfg.Endpoint, req)
	if err != nil {
		return nil, err
	}
	resp, err := s.page.Goto(target, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateLoad,
		Timeout:   playwright.Float(remainingMS()),
	})
	if err != nil {
		return nil, f.navigationError(err)
	}
	status := 0
	if resp != nil {
		status = resp.Status()
	}

	html, err := s.page.Content()
	if err != nil {
		return nil, fmt.Errorf("playwright fetcher: read page content: %w", err)
	}
	kind := classifyPage(s.page.URL(), html)

	if kind == pageConsent {
		next, ok, err := submitRejectConsent(s.page, remainingMS())
		if err != nil {
			return nil, f.navigationError(err)
		}
		if !ok {
			f.cfg.Metrics.IncBlocked("consent")
			return nil, blockedErrorFor(pageConsent, s.page.URL(), status, html)
		}
		f.cfg.Metrics.IncConsentHandled()
		resp, status = next, 0
		if resp != nil {
			status = resp.Status()
		}
		if html, err = s.page.Content(); err != nil {
			return nil, fmt.Errorf("playwright fetcher: read page content: %w", err)
		}
		kind = classifyPage(s.page.URL(), html)
	}

	if blocked := blockedErrorFor(kind, s.page.URL(), status, html); blocked != nil {
		f.cfg.Metrics.IncBlocked(blocked.Reason)
		return nil, blocked
	}

	if status == 429 {
		var retryAfter time.Duration
		if v, err := resp.HeaderValue("retry-after"); err == nil {
			retryAfter, _ = parseRetryAfter(v)
		}
		return nil, &RateLimitError{StatusCode: status, RetryAfter: retryAfter, Body: truncate(html, 512)}
	}
	if status >= 400 {
		return nil, &StatusError{StatusCode: status}
	}

	if f.cfg.WaitSelector != "" {
		// A missing selector isn't an error: the page is returned as
		// rendered and the parser's calibration check flags it, with the
		// raw body archived for selector retuning.
		if _, err := s.page.WaitForSelector(f.cfg.WaitSelector, playwright.PageWaitForSelectorOptions{
			State:   playwright.WaitForSelectorStateAttached,
			Timeout: playwright.Float(remainingMS()),
		}); err == nil {
			if html, err = s.page.Content(); err != nil {
				return nil, fmt.Errorf("playwright fetcher: read page content: %w", err)
			}
		}
	}

	body := []byte(html)
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes]
	}
	if status == 0 {
		status = 200
	}
	return &Response{StatusCode: status, Body: body, Latency: time.Since(start)}, nil
}

func (f *PlaywrightFetcher) navigationError(err error) error {
	if errors.Is(err, playwright.ErrTimeout) {
		f.cfg.Metrics.IncNavigationTimeouts()
		return fmt.Errorf("playwright fetcher: navigation timed out after %s: %w", f.cfg.Timeout, err)
	}
	// Chromium network failures (DNS, connect, proxy, TLS) surface as
	// net::ERR_* and are classified as transport errors.
	if strings.Contains(err.Error(), "net::ERR_") {
		return &TransportError{Err: fmt.Errorf("playwright fetcher: navigation failed: %w", err)}
	}
	return fmt.Errorf("playwright fetcher: navigation failed: %w", err)
}

// acquire waits for a free slot, then returns an idle session matching
// req's profile or opens a new one, evicting an idle session of another
// profile if the pool is full.
func (f *PlaywrightFetcher) acquire(ctx context.Context, req Request) (*browserSession, error) {
	select {
	case f.slots <- struct{}{}:
	case <-f.done:
		return nil, ErrFetcherClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	key := sessionKey(req)
	if f.closed.Load() {
		<-f.slots
		return nil, ErrFetcherClosed
	}
	f.mu.Lock()
	browser, err := f.browserLocked()
	if err != nil {
		f.mu.Unlock()
		<-f.slots
		return nil, err
	}
	var stale []*browserSession
	var reuse *browserSession
	kept := f.idle[:0]
	for _, s := range f.idle {
		switch {
		case s.crashed.Load() || s.page.IsClosed():
			// Died while idle (renderer crash, closed page): drop it.
			stale = append(stale, s)
			f.open--
		case reuse == nil && s.key == key && s.browser == browser:
			reuse = s
		default:
			kept = append(kept, s)
		}
	}
	f.idle = kept
	if reuse != nil {
		f.mu.Unlock()
		for _, s := range stale {
			f.closeSession(s)
		}
		return reuse, nil
	}
	var evict *browserSession
	if f.open >= f.cfg.PoolSize && len(f.idle) > 0 {
		evict, f.idle = f.idle[0], f.idle[1:]
		f.open--
	}
	f.open++ // reserve before unlocking so concurrent acquires respect the cap
	f.mu.Unlock()

	for _, s := range stale {
		f.closeSession(s)
	}
	if evict != nil {
		f.closeSession(evict)
	}
	s, err := f.newSession(browser, req, key)
	if err != nil {
		f.mu.Lock()
		f.open--
		f.mu.Unlock()
		<-f.slots
		return nil, err
	}
	f.cfg.Metrics.AddSessionsOpen(1)
	return s, nil
}

// release returns s to the idle pool, or closes it if the fetch failed, it
// has reached MaxSessionUses, its browser has been replaced, or the fetcher
// is closing.
func (f *PlaywrightFetcher) release(s *browserSession, healthy bool) {
	f.cfg.Metrics.AddSessionsInUse(-1)
	s.uses++
	f.mu.Lock()
	keep := healthy && !f.closed.Load() && !s.crashed.Load() && s.uses < f.cfg.MaxSessionUses &&
		s.browser == f.browser && s.browser.IsConnected()
	if keep {
		f.idle = append(f.idle, s)
	} else {
		f.open--
	}
	f.mu.Unlock()
	if !keep {
		f.closeSession(s)
	}
	<-f.slots
}

func (f *PlaywrightFetcher) closeSession(s *browserSession) {
	s.context.Close() // closes the page too; errors mean it's already gone
	f.cfg.Metrics.AddSessionsOpen(-1)
}

// browserLocked returns the shared browser, launching (or relaunching after
// a crash) as needed. Idle sessions of a dead browser are dropped. f.mu must
// be held.
func (f *PlaywrightFetcher) browserLocked() (playwright.Browser, error) {
	if f.browser != nil && f.browser.IsConnected() {
		return f.browser, nil
	}
	if f.closed.Load() {
		// Never relaunch after Close, e.g. for a fetch that outlived
		// Close's drain timeout.
		return nil, ErrFetcherClosed
	}
	if time.Now().Before(f.nextLaunch) {
		return nil, fmt.Errorf("%w: last launch error: %v", ErrBrowserUnavailable, f.lastLaunchErr)
	}
	if f.browser != nil {
		kept := f.idle[:0]
		for _, s := range f.idle {
			if s.browser == f.browser {
				f.open--
				f.cfg.Metrics.AddSessionsOpen(-1)
				continue
			}
			kept = append(kept, s)
		}
		f.idle = kept
	}

	opts := playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(f.cfg.Headless),
		// Shutdown is driven by Close, not by the driver reacting to
		// signals the harvester already handles.
		HandleSIGINT:  playwright.Bool(false),
		HandleSIGTERM: playwright.Bool(false),
		HandleSIGHUP:  playwright.Bool(false),
	}
	if f.cfg.ExecutablePath != "" {
		opts.ExecutablePath = playwright.String(f.cfg.ExecutablePath)
	}
	b, err := f.pw.Chromium.Launch(opts)
	if err != nil {
		// Back off exponentially (1s, 2s, 4s ... 30s) so a browser that
		// can't start doesn't turn every fetch into a launch attempt.
		f.launchFailures++
		backoff := time.Second << min(f.launchFailures-1, 5)
		if backoff > maxLaunchBackoff {
			backoff = maxLaunchBackoff
		}
		f.nextLaunch = time.Now().Add(backoff)
		f.lastLaunchErr = err
		f.cfg.Metrics.IncLaunchFailures()
		return nil, fmt.Errorf("playwright fetcher: launch chromium: %w", err)
	}
	f.launchFailures, f.nextLaunch, f.lastLaunchErr = 0, time.Time{}, nil
	b.OnDisconnected(func(playwright.Browser) {
		if !f.closed.Load() {
			f.cfg.Metrics.IncDisconnects()
		}
	})
	f.browser = b
	f.cfg.Metrics.IncLaunches()
	return b, nil
}

func (f *PlaywrightFetcher) newSession(browser playwright.Browser, req Request, key string) (*browserSession, error) {
	d := deviceProfileFor(req.Device)
	opts := playwright.BrowserNewContextOptions{
		Viewport:          &playwright.Size{Width: d.width, Height: d.height},
		IsMobile:          playwright.Bool(d.mobile),
		HasTouch:          playwright.Bool(d.touch),
		DeviceScaleFactor: playwright.Float(d.scale),
	}
	if req.UserAgent != "" {
		opts.UserAgent = playwright.String(req.UserAgent)
	}
	if tag := localeTag(req.Language, req.Country); tag != "" {
		opts.Locale = playwright.String(tag)
	}
	if req.ProxyURL != "" {
		p, err := proxySettings(req.ProxyURL)
		if err != nil {
			return nil, err
		}
		opts.Proxy = p
	}

	bctx, err := browser.NewContext(opts)
	if err != nil {
		return nil, fmt.Errorf("playwright fetcher: new browser context: %w", err)
	}
	// Same pre-seeded consent cookie HTTPFetcher uses, host-only for the
	// endpoint. It affects only the regulatory consent screen.
	if err := bctx.AddCookies([]playwright.OptionalCookie{{
		Name: "CONSENT", Value: "YES+", URL: playwright.String(f.cfg.Endpoint),
	}}); err != nil {
		bctx.Close()
		return nil, fmt.Errorf("playwright fetcher: seed consent cookie: %w", err)
	}
	page, err := bctx.NewPage()
	if err != nil {
		bctx.Close()
		return nil, fmt.Errorf("playwright fetcher: new page: %w", err)
	}
	s := &browserSession{key: key, browser: browser, context: bctx, page: page}
	page.OnCrash(func(playwright.Page) { f.onPageCrash(s) })
	return s, nil
}

// onPageCrash handles a renderer crash reported for s's page. The in-flight
// navigation fails on its own; the flag makes sure the session is discarded
// rather than reused.
func (f *PlaywrightFetcher) onPageCrash(s *browserSession) {
	s.crashed.Store(true)
	f.cfg.Metrics.IncPageCrashes()
}

// Close closes every session, the browser and the Playwright driver. It
// waits (up to Timeout plus a margin) for in-flight fetches to release
// their sessions first. Safe to call more than once.
func (f *PlaywrightFetcher) Close() error {
	var err error
	f.closeOnce.Do(func() {
		f.closed.Store(true)
		close(f.done)
		f.mu.Lock()
		idle := f.idle
		f.idle = nil
		f.open -= len(idle)
		f.mu.Unlock()
		for _, s := range idle {
			f.closeSession(s)
		}

		// Holding every slot means no fetch is in flight.
		drain := time.After(f.cfg.Timeout + 5*time.Second)
	wait:
		for i := 0; i < cap(f.slots); i++ {
			select {
			case f.slots <- struct{}{}:
			case <-drain:
				break wait
			}
		}

		f.mu.Lock()
		b := f.browser
		f.mu.Unlock()
		if b != nil {
			if cerr := b.Close(); cerr != nil && !errors.Is(cerr, playwright.ErrTargetClosed) {
				err = fmt.Errorf("playwright fetcher: close browser: %w", cerr)
			}
		}
		if serr := f.pw.Stop(); serr != nil && err == nil {
			err = fmt.Errorf("playwright fetcher: stop driver: %w", serr)
		}
	})
	return err
}

// searchURL builds the same query string HTTPFetcher sends.
func searchURL(endpoint string, req Request) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("playwright fetcher: invalid endpoint: %w", err)
	}
	q := u.Query()
	q.Set("q", req.Query)
	if req.Country != "" {
		q.Set("gl", req.Country)
	}
	if req.Language != "" {
		q.Set("hl", req.Language)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// sessionKey groups requests that can share a browser context: proxy,
// User-Agent, locale and viewport are all fixed per context.
func sessionKey(req Request) string {
	return strings.Join([]string{req.ProxyURL, req.UserAgent, localeTag(req.Language, req.Country), deviceProfileFor(req.Device).name}, "\x00")
}

// localeTag turns the worker's language and country hints into a BCP 47
// tag for the browser locale (and so its Accept-Language header), e.g.
// "en-US". Empty language means "browser default".
func localeTag(language, country string) string {
	if language == "" {
		return ""
	}
	if country == "" {
		return language
	}
	return language + "-" + strings.ToUpper(country)
}

type deviceProfile struct {
	name          string
	width, height int
	scale         float64
	mobile, touch bool
}

// deviceProfileFor maps the job's device hint to viewport emulation, the
// browser counterpart of the provider fetcher's device parameter. Unknown
// or empty values mean desktop.
func deviceProfileFor(device string) deviceProfile {
	switch strings.ToLower(device) {
	case "mobile":
		return deviceProfile{name: "mobile", width: 412, height: 915, scale: 2.625, mobile: true, touch: true}
	case "tablet":
		return deviceProfile{name: "tablet", width: 820, height: 1180, scale: 2, mobile: true, touch: true}
	default:
		return deviceProfile{name: "desktop", width: 1366, height: 768, scale: 1}
	}
}

// proxySettings converts a proxy URL, including one carrying
// user:pass credentials, into Playwright's proxy options.
func proxySettings(proxyURL string) (*playwright.Proxy, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("playwright fetcher: invalid proxy url %q", proxyURL)
	}
	p := &playwright.Proxy{Server: u.Scheme + "://" + u.Host}
	if u.User != nil {
		p.Username = playwright.String(u.User.Username())
		if pass, ok := u.User.Password(); ok {
			p.Password = playwright.String(pass)
		}
	}
	return p, nil
}

// submitRejectConsent clicks the submit control of the consent page's
// "reject all" form and waits for the resulting navigation away from the
// consent page, returning its response. ok is false if there is no such
// form; err is set if the click or the navigation fails.
func submitRejectConsent(page playwright.Page, timeoutMS float64) (resp playwright.Response, ok bool, err error) {
	button := page.Locator(rejectConsentForm + ` :is(button, input[type="submit"])`).First()
	if n, cerr := button.Count(); cerr != nil || n == 0 {
		return nil, false, nil
	}
	consentURL := page.URL()
	resp, err = page.ExpectNavigation(func() error {
		return button.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(timeoutMS)})
	}, playwright.PageExpectNavigationOptions{
		URL:       func(u string) bool { return u != consentURL },
		WaitUntil: playwright.WaitUntilStateLoad,
		Timeout:   playwright.Float(timeoutMS),
	})
	if err != nil {
		return nil, false, err
	}
	return resp, true, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
