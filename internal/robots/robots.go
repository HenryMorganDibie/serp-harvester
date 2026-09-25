// Package robots implements the Robots Exclusion Protocol (RFC 9309) for
// the web crawler: parsing robots.txt, matching a URL against the group
// for the crawler's user agent, and caching each site's rules.
package robots

import (
	"bufio"
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rules is a parsed robots.txt, reduced to what applies to one user agent.
type Rules struct {
	rules []rule
	// CrawlDelay is the group's Crawl-delay, zero if none. Not part of
	// RFC 9309, but widely published and honored here.
	CrawlDelay time.Duration
	// Sitemaps lists the file's Sitemap URLs (they apply to every agent).
	Sitemaps []string
}

type rule struct {
	allow   bool
	pattern string // as written; its length ranks matches
	re      *regexp.Regexp
}

func newRule(allow bool, pattern string) rule {
	return rule{allow: allow, pattern: pattern, re: compile(pattern)}
}

// AllowAll is the policy when a site has no robots.txt (any 4xx).
var AllowAll = &Rules{}

// DisallowAll is the policy when robots.txt is unreachable (5xx or a
// network error): RFC 9309 section 2.3.1.4 says to assume full disallow.
var DisallowAll = &Rules{rules: []rule{newRule(false, "/")}}

// Parse reads robots.txt content and keeps the group that applies to
// agent (the crawler's product token, e.g. "serp-harvester"): the group
// whose user-agent line matches agent case-insensitively, else the "*"
// group, else no rules. Several groups naming the same agent are merged,
// as RFC 9309 requires.
func Parse(content, agent string) *Rules {
	agent = strings.ToLower(agent)
	type group struct {
		agents []string
		rules  []rule
		delay  time.Duration
	}
	var groups []*group
	var cur *group
	lastWasAgent := false
	var sitemaps []string

	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if !lastWasAgent || cur == nil {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(value))
			lastWasAgent = true
			continue
		case "allow", "disallow":
			if cur != nil && value != "" {
				cur.rules = append(cur.rules, newRule(key == "allow", value))
			}
		case "crawl-delay":
			if cur != nil {
				if secs, err := strconv.ParseFloat(value, 64); err == nil && secs > 0 {
					cur.delay = time.Duration(secs * float64(time.Second))
				}
			}
		case "sitemap":
			sitemaps = append(sitemaps, value)
		}
		lastWasAgent = false
	}

	out := &Rules{Sitemaps: sitemaps}
	matched := false
	for _, g := range groups {
		for _, a := range g.agents {
			if a != "*" && a != "" && strings.Contains(agent, a) {
				matched = true
				out.rules = append(out.rules, g.rules...)
				out.CrawlDelay = max(out.CrawlDelay, g.delay)
				break
			}
		}
	}
	if !matched {
		for _, g := range groups {
			for _, a := range g.agents {
				if a == "*" {
					out.rules = append(out.rules, g.rules...)
					out.CrawlDelay = max(out.CrawlDelay, g.delay)
					break
				}
			}
		}
	}
	return out
}

// Allowed reports whether path (a URL path plus optional query) may be
// fetched: the longest matching rule wins, Allow wins a tie, and no match
// means allowed. /robots.txt itself is always allowed.
func (r *Rules) Allowed(path string) bool {
	if path == "" {
		path = "/"
	}
	if path == "/robots.txt" {
		return true
	}
	path = unescape(path)
	best, allow := -1, true
	for _, ru := range r.rules {
		if !ru.re.MatchString(path) {
			continue
		}
		if n := len(ru.pattern); n > best || (n == best && ru.allow) {
			best, allow = n, ru.allow
		}
	}
	return allow
}

// compile turns a robots.txt path pattern into a regexp anchored at the
// start of the path: "*" matches any run of characters and a trailing "$"
// anchors the end. The pattern is percent-decoded first, as paths are.
func compile(pattern string) *regexp.Regexp {
	anchored := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	parts := strings.Split(unescape(pattern), "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	expr := "^" + strings.Join(parts, ".*")
	if anchored {
		expr += "$"
	}
	return regexp.MustCompile(expr)
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// Getter fetches a robots.txt URL. status is the HTTP status; err is a
// network failure (no response).
type Getter func(ctx context.Context, robotsURL string) (status int, body []byte, err error)

// Cache fetches and caches each origin's rules for one user agent.
type Cache struct {
	Agent string
	Get   Getter

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	once  sync.Once
	rules *Rules
}

// For returns the rules for u's origin (scheme://host:port), fetching
// robots.txt on first use: 2xx is parsed, 4xx means no restrictions,
// anything else (5xx, a network error, a redirect loop) means disallow
// everything, per RFC 9309. 429 is treated as unavailable (disallow), not
// as "no robots.txt": the site is asking for fewer requests.
func (c *Cache) For(ctx context.Context, u *url.URL) *Rules {
	origin := u.Scheme + "://" + u.Host
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*entry{}
	}
	e, ok := c.entries[origin]
	if !ok {
		e = &entry{}
		c.entries[origin] = e
	}
	c.mu.Unlock()

	e.once.Do(func() {
		status, body, err := c.Get(ctx, origin+"/robots.txt")
		switch {
		case err != nil:
			e.rules = DisallowAll
		case status >= 200 && status < 300:
			e.rules = Parse(string(body), c.Agent)
		case status >= 400 && status < 500 && status != 429:
			e.rules = AllowAll
		default:
			e.rules = DisallowAll
		}
	})
	return e.rules
}
