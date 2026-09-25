// Package crawl is the general-purpose web crawler: for each configured
// target it starts from a set of URLs, fetches pages through the same
// acquisition stack as the SERP harvester (proxy pool, adaptive per-host
// rate limiting, block classification and cooldowns, HTTP or Chromium),
// extracts structured data from every page (internal/extract), and follows
// links within the target's scope up to a depth and page budget.
//
// It obeys robots.txt by default (RFC 9309, including Crawl-delay), never
// interacts with CAPTCHAs or bot-protection challenges, and does not
// retry a challenged page in the browser: a block is recorded and the page
// skipped.
package crawl

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/HenryMorganDibie/web-harvester/internal/extract"
)

// Target is one site (or set of sites) to crawl.
type Target struct {
	Name      string   `yaml:"name"`
	StartURLs []string `yaml:"start_urls"`
	// AllowedDomains limits which hosts are followed; each entry also
	// covers its subdomains. Default: the hosts of StartURLs.
	AllowedDomains []string `yaml:"allowed_domains"`
	// MaxDepth is how many links away from a start URL to follow. 0 (the
	// default) fetches only the start URLs.
	MaxDepth int `yaml:"max_depth"`
	// MaxPages caps the pages fetched for this target. Default 100.
	MaxPages int `yaml:"max_pages"`
	// Follow is the CSS selector for links to follow. Default "a[href]".
	Follow string `yaml:"follow"`
	// Include and Exclude are regular expressions matched against each
	// absolute URL before it is followed: it must match an Include (if
	// any are set) and no Exclude. Start URLs are not filtered.
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
	// Render selects the fetcher: "http" (default), "browser" (headless
	// Chromium for every page) or "auto" (HTTP, rendering in Chromium only
	// pages that are client-side app shells with no server-rendered
	// content).
	Render string `yaml:"render"`
	// WaitSelector, for rendered pages, is awaited after load.
	WaitSelector string `yaml:"wait_selector"`
	// Extract adds target-specific fields and items to the generic page
	// data.
	Extract extract.Spec `yaml:"extract"`
	// RespectRobots obeys the site's robots.txt. Default true; set false
	// only for sites you own or have permission to crawl regardless.
	RespectRobots *bool `yaml:"respect_robots"`
	// Concurrency is this target's worker count. Default: the global
	// concurrency.
	Concurrency int `yaml:"concurrency"`
	// Locale ("US-en") and Device ("desktop", "mobile") hints, as for
	// SERP jobs; used by the browser for locale and viewport.
	Locale string `yaml:"locale"`
	Device string `yaml:"device"`
}

// DefaultMaxPages caps a target's pages when MaxPages is unset.
const DefaultMaxPages = 100

// skipExtensions are file types the crawler does not follow links to:
// they are not HTML pages and are often large.
var skipExtensions = map[string]bool{
	".7z": true, ".avi": true, ".bmp": true, ".css": true, ".dmg": true, ".doc": true, ".docx": true,
	".eot": true, ".exe": true, ".gif": true, ".gz": true, ".ico": true, ".iso": true, ".jpeg": true,
	".jpg": true, ".js": true, ".m4a": true, ".mov": true, ".mp3": true, ".mp4": true, ".pdf": true,
	".png": true, ".ppt": true, ".pptx": true, ".rar": true, ".svg": true, ".tar": true, ".tgz": true,
	".ttf": true, ".wav": true, ".webm": true, ".webp": true, ".woff": true, ".woff2": true,
	".xls": true, ".xlsx": true, ".zip": true,
}

// scope is a validated, compiled Target.
type scope struct {
	Target
	starts  []string
	domains []string
	include []*regexp.Regexp
	exclude []*regexp.Regexp
}

// compile validates t and applies defaults.
func compile(t Target) (*scope, error) {
	if t.Name == "" {
		return nil, fmt.Errorf("crawl: target without a name")
	}
	if len(t.StartURLs) == 0 {
		return nil, fmt.Errorf("crawl: target %q: no start_urls", t.Name)
	}
	if t.MaxDepth < 0 {
		return nil, fmt.Errorf("crawl: target %q: max_depth must be >= 0", t.Name)
	}
	switch t.Render {
	case "":
		t.Render = "http"
	case "http", "browser", "auto":
	default:
		return nil, fmt.Errorf("crawl: target %q: render %q (want http|browser|auto)", t.Name, t.Render)
	}
	if t.MaxPages <= 0 {
		t.MaxPages = DefaultMaxPages
	}
	if t.Follow == "" {
		t.Follow = "a[href]"
	}
	if t.RespectRobots == nil {
		yes := true
		t.RespectRobots = &yes
	}
	if err := t.Extract.Validate(); err != nil {
		return nil, fmt.Errorf("crawl: target %q: %w", t.Name, err)
	}
	if err := (extract.Spec{Fields: map[string]extract.Rule{"follow": {Selector: t.Follow}}}).Validate(); err != nil {
		return nil, fmt.Errorf("crawl: target %q: follow: %w", t.Name, err)
	}

	s := &scope{Target: t}
	for _, raw := range t.StartURLs {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("crawl: target %q: start url %q must be an absolute http(s) URL", t.Name, raw)
		}
		s.starts = append(s.starts, normalize(u))
		if len(t.AllowedDomains) == 0 {
			s.domains = append(s.domains, u.Hostname())
		}
	}
	for _, d := range t.AllowedDomains {
		s.domains = append(s.domains, strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), ".")))
	}
	for _, expr := range t.Include {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("crawl: target %q: include %q: %w", t.Name, expr, err)
		}
		s.include = append(s.include, re)
	}
	for _, expr := range t.Exclude {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("crawl: target %q: exclude %q: %w", t.Name, expr, err)
		}
		s.exclude = append(s.exclude, re)
	}
	return s, nil
}

// follows reports whether the normalized absolute URL raw is within scope.
func (s *scope) follows(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if skipExtensions[strings.ToLower(path.Ext(u.Path))] {
		return false
	}
	host := strings.ToLower(u.Hostname())
	inDomain := false
	for _, d := range s.domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			inDomain = true
			break
		}
	}
	if !inDomain {
		return false
	}
	if len(s.include) > 0 {
		matched := false
		for _, re := range s.include {
			if re.MatchString(raw) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, re := range s.exclude {
		if re.MatchString(raw) {
			return false
		}
	}
	return true
}

// normalize canonicalizes u for deduplication: lower-case scheme and host,
// no default port, no fragment, "/" for an empty path. The query is kept
// as-is, since its order can matter to the site.
func normalize(u *url.URL) string {
	n := *u
	n.Scheme = strings.ToLower(n.Scheme)
	host := strings.ToLower(n.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6 literal
	}
	if port := n.Port(); port != "" && !(n.Scheme == "http" && port == "80") && !(n.Scheme == "https" && port == "443") {
		host += ":" + port
	}
	n.Host = host
	n.Fragment, n.RawFragment = "", ""
	if n.Path == "" {
		n.Path = "/"
	}
	return n.String()
}
