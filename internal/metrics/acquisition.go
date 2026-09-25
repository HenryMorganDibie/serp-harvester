package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Acquisition counters: how fetches end and how the pipeline reacted
// (proxy cooldowns, throttling, failover). They live on Counters so every
// mode reports them; see worker.Pool for where they are incremented.

// ProxyGauges is a point-in-time view of the proxy pool and rate limiter,
// read on every scrape via Counters.ProxyGauges.
type ProxyGauges struct {
	Available int // proxies not in cooldown
	Cooling   int // proxies in cooldown
	Throttled int // proxies whose adaptive rate is below the configured rate
}

// labeledCounter is a set of counters keyed by a label value, safe for
// concurrent use and usable as a zero value.
type labeledCounter struct {
	mu sync.Mutex
	m  map[string]*uint64
}

func (l *labeledCounter) inc(label string) {
	l.mu.Lock()
	if l.m == nil {
		l.m = make(map[string]*uint64)
	}
	c, ok := l.m[label]
	if !ok {
		c = new(uint64)
		l.m[label] = c
	}
	l.mu.Unlock()
	atomic.AddUint64(c, 1)
}

// snapshot returns label/value pairs sorted by label, for stable output.
func (l *labeledCounter) snapshot() []LabeledValue {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LabeledValue, 0, len(l.m))
	for k, c := range l.m {
		out = append(out, LabeledValue{Label: k, Value: atomic.LoadUint64(c)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// LabeledValue is one label's count.
type LabeledValue struct {
	Label string
	Value uint64
}

// IncOutcome records how one fetch attempt ended (fetcher.Outcome.String()).
func (c *Counters) IncOutcome(outcome string) { c.outcomes.inc(outcome) }

// IncProxyCooldown records a proxy entering cooldown, by reason
// ("failures", "blocked", "rate_limited").
func (c *Counters) IncProxyCooldown(reason string) { c.cooldowns.inc(reason) }

// IncRateDecrease records one adaptive rate decrease for a proxy.
func (c *Counters) IncRateDecrease() { atomic.AddUint64(&c.RateDecreases, 1) }

// IncFailover records a fetch retried on the fallback fetcher (hybrid mode).
func (c *Counters) IncFailover() { atomic.AddUint64(&c.Failovers, 1) }

// IncHTTPConsentHandled records a consent page dismissed by the HTTP
// fetcher's reject-all form submission.
func (c *Counters) IncHTTPConsentHandled() { atomic.AddUint64(&c.HTTPConsentHandled, 1) }

// IncRobotsDisallowed records a URL the web crawler did not fetch because
// the site's robots.txt disallows it.
func (c *Counters) IncRobotsDisallowed() { atomic.AddUint64(&c.RobotsDisallowed, 1) }

// Outcomes returns fetch-attempt counts by outcome.
func (c *Counters) Outcomes() []LabeledValue { return c.outcomes.snapshot() }

// Cooldowns returns proxy cooldown counts by reason.
func (c *Counters) Cooldowns() []LabeledValue { return c.cooldowns.snapshot() }
