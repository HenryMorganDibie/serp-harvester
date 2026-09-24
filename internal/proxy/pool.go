// Package proxy provides a rotating pool of egress identities with simple
// health tracking: a proxy that fails repeatedly is put in cooldown instead
// of being hammered, and comes back into rotation once the cooldown expires.
//
// With zero proxies configured, the pool hands out a single "direct"
// identity so the same worker code path runs unchanged in local/dev mode.
package proxy

import (
	"errors"
	"sync"
	"time"
)

// ErrAllBanned is returned when every proxy in the pool is currently cooling
// down. Callers should back off and retry the whole pool later.
var ErrAllBanned = errors.New("proxy: all proxies are in cooldown")

// Proxy is one egress identity (e.g. a proxy URL). Key() is what the rate
// limiter and metrics key off of.
type Proxy struct {
	URL string

	mu               sync.Mutex
	consecutiveFails int
	bannedUntil      time.Time
}

// Key returns a stable identifier for this proxy, or "direct" when no proxy
// URL is configured (i.e. outbound requests go straight out).
func (p *Proxy) Key() string {
	if p.URL == "" {
		return "direct"
	}
	return p.URL
}

func (p *Proxy) isBanned() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.bannedUntil)
}

// Pool hands out proxies in round-robin order, skipping any currently
// banned, and tracks failures to decide when to ban.
type Pool struct {
	mu           sync.Mutex
	proxies      []*Proxy
	idx          int
	banThreshold int
	banCooldown  time.Duration
}

// NewPool builds a pool from a list of proxy URLs. An empty list is valid
// and results in a single direct (no-proxy) identity.
func NewPool(urls []string, banThreshold int, banCooldown time.Duration) *Pool {
	proxies := make([]*Proxy, 0, len(urls))
	if len(urls) == 0 {
		proxies = append(proxies, &Proxy{})
	} else {
		for _, u := range urls {
			proxies = append(proxies, &Proxy{URL: u})
		}
	}
	return &Pool{
		proxies:      proxies,
		banThreshold: banThreshold,
		banCooldown:  banCooldown,
	}
}

// Next returns the next healthy proxy in rotation.
func (pl *Pool) Next() (*Proxy, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	n := len(pl.proxies)
	for i := 0; i < n; i++ {
		p := pl.proxies[pl.idx]
		pl.idx = (pl.idx + 1) % n
		if !p.isBanned() {
			return p, nil
		}
	}
	return nil, ErrAllBanned
}

// ReportResult records whether the last request through p succeeded, banning
// p for banCooldown once it has failed banThreshold times in a row.
func (pl *Pool) ReportResult(p *Proxy, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err == nil {
		p.consecutiveFails = 0
		return
	}
	p.consecutiveFails++
	if p.consecutiveFails >= pl.banThreshold {
		p.bannedUntil = time.Now().Add(pl.banCooldown)
	}
}

// Size returns how many proxies are configured (at least 1).
func (pl *Pool) Size() int {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return len(pl.proxies)
}
