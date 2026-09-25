// Package fixture serves a fictional search site and forward proxies with
// scripted behavior, for integration tests and the Docker end-to-end run.
// Nothing here resembles or contacts a real search engine: the site is
// served under a made-up host (serp.fixture.test) that only resolves
// through these proxies.
package fixture

import (
	_ "embed"
	"net/http"
	"sync/atomic"
)

// Host is the fictional search host; point live_endpoint at
// "http://"+Host+"/search" and route through a fixture proxy.
const Host = "serp.fixture.test"

//go:embed testdata/js_check.html
var jsCheckPage []byte

//go:embed testdata/captcha.html
var captchaPage []byte

// Origin serves the search page: a JS-check shell whose results only exist
// after its script runs (a plain HTTP client sees the no-JS fallback; a
// browser renders three results).
func Origin() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(jsCheckPage)
	})
}

// Behavior selects how a fixture proxy answers.
type Behavior string

const (
	// Good serves the origin, as a working proxy would.
	Good Behavior = "good"
	// Captcha answers every request with a CAPTCHA page (status 429).
	Captcha Behavior = "captcha"
	// RateLimitOnce answers its first request with 429 Retry-After: 30,
	// then behaves like Good.
	RateLimitOnce Behavior = "ratelimit-once"
)

// Proxy is a forward-proxy handler with the given behavior. It serves the
// fictional site itself instead of forwarding, so no traffic leaves the
// test. Requests counts requests it received.
type Proxy struct {
	Behavior Behavior
	Requests atomic.Int64
	origin   http.Handler
}

// NewProxy returns a fixture proxy.
func NewProxy(b Behavior) *Proxy {
	return &Proxy{Behavior: b, origin: Origin()}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		http.Error(w, "fixture proxy: no TLS", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/healthz" && r.URL.Host == "" {
		w.Write([]byte("ok")) // the proxy's own health check, not a proxied request
		return
	}
	n := p.Requests.Add(1)
	switch {
	case p.Behavior == Captcha:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(captchaPage)
	case p.Behavior == RateLimitOnce && n == 1:
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("slow down"))
	default:
		p.origin.ServeHTTP(w, r)
	}
}
