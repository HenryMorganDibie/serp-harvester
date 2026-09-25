package robots

import (
	"context"
	"errors"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllowed_MatchingRules(t *testing.T) {
	r := Parse(`
User-agent: *
Disallow: /private
Allow: /private/public
Disallow: /*.pdf$
Disallow: /search?*sort=
Disallow: /exact$
`, "serp-harvester")

	cases := map[string]bool{
		"/":                     true,
		"/private":              false,
		"/private/x":            false,
		"/private/public/page":  true, // longer Allow wins
		"/docs/a.pdf":           false,
		"/docs/a.pdf?x=1":       true, // "$" anchors the end
		"/search?q=a&sort=date": false,
		"/search?q=a":           true,
		"/exact":                false,
		"/exact/more":           true,
		"/robots.txt":           true,
		"":                      true,
	}
	for path, want := range cases {
		if got := r.Allowed(path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestAllowed_TieGoesToAllow(t *testing.T) {
	r := Parse("User-agent: *\nDisallow: /page\nAllow: /page\n", "bot")
	if !r.Allowed("/page") {
		t.Error("equal-length Allow and Disallow: Allow should win")
	}
}

func TestAllowed_PercentEncoding(t *testing.T) {
	r := Parse("User-agent: *\nDisallow: /caf%C3%A9\n", "bot")
	if r.Allowed("/café") || r.Allowed("/caf%C3%A9/menu") {
		t.Error("encoded and decoded forms of a path should match the same rule")
	}
}

func TestParse_PicksMostSpecificGroupAndMerges(t *testing.T) {
	content := `
# comment
User-agent: *
Disallow: /

User-agent: serp-harvester
User-agent: other-bot
Disallow: /admin
Crawl-delay: 2.5

User-agent: SERP-HARVESTER
Disallow: /tmp

Sitemap: https://example.com/sitemap.xml
`
	r := Parse(content, "Mozilla/5.0 (compatible; serp-harvester/0.1)")
	if !r.Allowed("/products") {
		t.Error("the named group replaces the * group entirely")
	}
	if r.Allowed("/admin") || r.Allowed("/tmp/x") {
		t.Error("both groups naming the agent should be merged")
	}
	if r.CrawlDelay != 2500*time.Millisecond {
		t.Errorf("crawl delay = %s", r.CrawlDelay)
	}
	if len(r.Sitemaps) != 1 || r.Sitemaps[0] != "https://example.com/sitemap.xml" {
		t.Errorf("sitemaps = %v", r.Sitemaps)
	}

	other := Parse(content, "unrelated-bot")
	if other.Allowed("/products") {
		t.Error("an agent no group names falls back to *")
	}
}

func TestParse_EmptyDisallowAllowsEverything(t *testing.T) {
	if r := Parse("User-agent: *\nDisallow:\n", "bot"); !r.Allowed("/anything") {
		t.Error("an empty Disallow permits everything")
	}
	if r := Parse("", "bot"); !r.Allowed("/anything") {
		t.Error("an empty file permits everything")
	}
}

func TestCache_StatusPolicyAndCaching(t *testing.T) {
	var calls atomic.Int32
	responses := map[string]struct {
		status int
		body   string
		err    error
	}{
		"https://ok.test/robots.txt":      {200, "User-agent: *\nDisallow: /no\n", nil},
		"https://missing.test/robots.txt": {404, "", nil},
		"https://broken.test/robots.txt":  {503, "", nil},
		"https://down.test/robots.txt":    {0, "", errors.New("connection refused")},
		"https://busy.test/robots.txt":    {429, "", nil},
	}
	c := &Cache{Agent: "bot", Get: func(_ context.Context, u string) (int, []byte, error) {
		calls.Add(1)
		r := responses[u]
		return r.status, []byte(r.body), r.err
	}}
	check := func(raw, path string, want bool) {
		t.Helper()
		u, _ := url.Parse(raw)
		if got := c.For(context.Background(), u).Allowed(path); got != want {
			t.Errorf("%s %s: allowed = %v, want %v", raw, path, got, want)
		}
	}
	check("https://ok.test/a", "/yes", true)
	check("https://ok.test/b", "/no", false)
	check("https://missing.test/", "/anything", true) // 4xx: no restrictions
	check("https://broken.test/", "/anything", false) // 5xx: assume disallow
	check("https://down.test/", "/anything", false)   // unreachable: disallow
	check("https://busy.test/", "/anything", false)   // 429: disallow for now
	if n := calls.Load(); n != 5 {
		t.Errorf("robots.txt fetched %d times, want once per origin (5)", n)
	}
}
