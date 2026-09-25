package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/extract"
	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/model"
	"github.com/HenryMorganDibie/serp-harvester/internal/proxy"
	"github.com/HenryMorganDibie/serp-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/serp-harvester/internal/robots"
	"github.com/HenryMorganDibie/serp-harvester/internal/worker"
)

// site is a small test website. Every page is plain HTML with links; see
// the handlers for what each path exercises.
type site struct {
	*httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	times []time.Time
}

func newSite(t *testing.T, robotsTxt string) *site {
	t.Helper()
	s := &site{hits: map[string]int{}}
	page := func(title string, links ...string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "<html><head><title>%s</title></head><body><h1>%s</h1>", title, title)
		for _, l := range links {
			fmt.Fprintf(&b, `<a href="%s">%s</a>`, l, l)
		}
		return b.String() + "</body></html>"
	}
	pages := map[string]string{
		"/": page("Home", "/a", "/b", "/a#section", "/private/secret", "/file.pdf",
			"https://elsewhere.test/", "mailto:x@example.test", "/notes.txt", "/moved"),
		"/a":              page("A", "/c", "/b", "/"),
		"/b":              page("B", "/a"),
		"/c":              page("C", "/d"),
		"/d":              page("D"),
		"/private/secret": page("Secret"),
		"/landing/":       page("Landing", "next"), // relative to the redirect target
		"/landing/next":   page("Next"),
		"/products": `<html><body>
			<div class="item"><h3>Kettle</h3><span class="price">29.99</span></div>
			<div class="item"><h3>Toaster</h3><span class="price">19.99</span></div></body></html>`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.times = append(s.times, time.Now())
		s.mu.Unlock()
		switch r.URL.Path {
		case "/robots.txt":
			if robotsTxt == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, robotsTxt)
			return
		case "/challenge":
			// A bot-protection challenge: classified, never solved.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<html><head><title>Just a moment...</title></head><body><form id="challenge-form"></form></body></html>`)
			return
		case "/notes.txt":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "plain notes <a href=/d>not a link</a>")
			return
		case "/moved":
			http.Redirect(w, r, "/landing/", http.StatusFound)
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *site) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

type memSink struct {
	mu    sync.Mutex
	pages []*model.Page
}

func (m *memSink) WritePage(p *model.Page) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pages = append(m.pages, p)
	return nil
}

func (m *memSink) byURL() map[string]*model.Page {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]*model.Page{}
	for _, p := range m.pages {
		out[p.URL] = p
	}
	return out
}

func newCrawler(t *testing.T, rps float64) (*Crawler, *memSink, *metrics.Counters) {
	t.Helper()
	f, err := fetcher.NewHTTPFetcher("", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f.Generic = true
	counters := &metrics.Counters{}
	pool := &worker.Pool{
		Concurrency: 3,
		MaxRetries:  1,
		Fetcher:     f,
		ProxyPool:   proxy.NewPool(nil, 1000, time.Hour),
		Limiter:     ratelimit.New(rps, 1),
		Metrics:     counters,
		UserAgent:   "test-crawler/1.0",
	}
	sink := &memSink{}
	return &Crawler{
		Pool:   pool,
		Robots: &robots.Cache{Agent: pool.UserAgent, Get: RobotsGetter(pool)},
		Sink:   sink,
		RunID:  "run-1",
	}, sink, counters
}

func paths(base string, pages map[string]*model.Page) []string {
	var out []string
	for u := range pages {
		out = append(out, strings.TrimPrefix(u, base))
	}
	sort.Strings(out)
	return out
}

func TestCrawl_DepthScopeRobotsAndDedup(t *testing.T) {
	s := newSite(t, "User-agent: *\nDisallow: /private/\n")
	c, sink, counters := newCrawler(t, 1000)

	stats, err := c.Run(context.Background(), Target{Name: "site", StartURLs: []string{s.URL + "/"}, MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	pages := sink.byURL()
	// Depth 0: /. Depth 1: /a /b /notes.txt /moved. Depth 2: /c (from /a)
	// and /landing/next (from the redirect's page). /d is depth 3.
	// /private is disallowed, /file.pdf and other hosts are out of scope,
	// and /a#section is /a.
	want := []string{"/", "/a", "/b", "/c", "/landing/next", "/moved", "/notes.txt"}
	if got := paths(s.URL, pages); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("pages = %v, want %v", got, want)
	}
	for _, p := range []string{"/", "/a", "/b"} {
		if n := s.hitCount(p); n != 1 {
			t.Errorf("%s fetched %d times; every URL should be fetched once", p, n)
		}
	}
	if s.hitCount("/private/secret") != 0 || s.hitCount("/file.pdf") != 0 || s.hitCount("/d") != 0 {
		t.Error("disallowed, non-HTML-extension or too-deep URLs were fetched")
	}
	if s.hitCount("/robots.txt") != 1 {
		t.Errorf("robots.txt fetched %d times, want once", s.hitCount("/robots.txt"))
	}
	if stats.Fetched != 7 || stats.RobotsDisallow != 1 || stats.Failed != 0 {
		t.Errorf("stats = %+v, want 7 fetched, 1 robots-disallowed, none failed", stats)
	}
	if counters.Snapshot().Success != 7 || counters.RobotsDisallowed != 1 {
		t.Errorf("metrics: success=%d robots_disallowed=%d", counters.Snapshot().Success, counters.RobotsDisallowed)
	}

	home := pages[s.URL+"/"]
	if home.Title != "Home" || home.Target != "site" || home.RunID != "run-1" || home.Depth != 0 || home.StatusCode != 200 {
		t.Errorf("home page = %+v", home)
	}
	if c := pages[s.URL+"/c"]; c.Depth != 2 {
		t.Errorf("/c depth = %d", c.Depth)
	}
}

func TestCrawl_ChallengeCoolsTheEgressAndIsNeverSolved(t *testing.T) {
	s := newSite(t, "")
	c, sink, counters := newCrawler(t, 1000)
	// One worker, so /challenge is fetched first. The challenge cools the
	// only egress (direct) for this target, so the crawler stops sending
	// requests to the site instead of pressing on through the block.
	stats, _ := c.Run(context.Background(), Target{Name: "site", StartURLs: []string{s.URL + "/challenge", s.URL + "/a"}, Concurrency: 1})
	if stats.Failed != 2 || len(sink.pages) != 0 {
		t.Errorf("stats = %+v, pages = %d", stats, len(sink.pages))
	}
	if s.hitCount("/challenge") != 1 || s.hitCount("/a") != 0 {
		t.Errorf("challenge fetched %d times, /a %d times; want 1 and 0 (no retry, egress cooled)", s.hitCount("/challenge"), s.hitCount("/a"))
	}
	outcomes := map[string]uint64{}
	for _, v := range counters.Outcomes() {
		outcomes[v.Label] = v.Value
	}
	if outcomes["challenge"] != 1 || counters.Snapshot().ProxyBanned != 1 {
		t.Errorf("outcomes = %v, proxy cooldowns = %d", outcomes, counters.Snapshot().ProxyBanned)
	}
}

func TestCrawl_RedirectsResolveAgainstTheFinalURL(t *testing.T) {
	s := newSite(t, "")
	c, sink, _ := newCrawler(t, 1000)
	if _, err := c.Run(context.Background(), Target{Name: "site", StartURLs: []string{s.URL + "/moved"}, MaxDepth: 1}); err != nil {
		t.Fatal(err)
	}
	pages := sink.byURL()
	moved := pages[s.URL+"/moved"]
	if moved == nil || moved.FinalURL != s.URL+"/landing/" || moved.Title != "Landing" {
		t.Fatalf("redirected page = %+v", moved)
	}
	if pages[s.URL+"/landing/next"] == nil {
		t.Errorf("relative link on the redirect target not followed: %v", paths(s.URL, pages))
	}
}

func TestCrawl_NonHTMLIsStoredButNotParsed(t *testing.T) {
	s := newSite(t, "")
	c, sink, _ := newCrawler(t, 1000)
	c.Run(context.Background(), Target{Name: "site", StartURLs: []string{s.URL + "/notes.txt"}, MaxDepth: 3})
	if len(sink.pages) != 1 {
		t.Fatalf("pages = %d", len(sink.pages))
	}
	p := sink.pages[0]
	if !strings.HasPrefix(p.ContentType, "text/plain") || p.Title != "" || len(p.Links) != 0 || s.hitCount("/d") != 0 {
		t.Errorf("text page = %+v; it should be recorded without extraction or link following", p)
	}
}

func TestCrawl_BudgetIncludeExclude(t *testing.T) {
	s := newSite(t, "")
	c, sink, _ := newCrawler(t, 1000)
	stats, _ := c.Run(context.Background(), Target{Name: "site", StartURLs: []string{s.URL + "/"}, MaxDepth: 5, MaxPages: 3})
	if stats.Fetched+stats.Failed != 3 || !stats.BudgetExhausted {
		t.Errorf("stats = %+v, want exactly 3 URLs attempted and the budget reported", stats)
	}

	c, sink, _ = newCrawler(t, 1000)
	c.Run(context.Background(), Target{
		Name: "site", StartURLs: []string{s.URL + "/"}, MaxDepth: 5,
		Include: []string{`/(a|b|c|d)$`}, Exclude: []string{`/c$`},
	})
	if got := paths(s.URL, sink.byURL()); strings.Join(got, " ") != "/ /a /b" {
		t.Errorf("pages = %v, want / /a /b (/c excluded, so /d is never found)", got)
	}
}

func TestCrawl_RobotsDisallowAllAndOptOut(t *testing.T) {
	s := newSite(t, "User-agent: *\nDisallow: /\n\nUser-agent: other\nAllow: /\n")
	c, sink, _ := newCrawler(t, 1000)
	stats, _ := c.Run(context.Background(), Target{Name: "site", StartURLs: []string{s.URL + "/"}, MaxDepth: 2})
	if len(sink.pages) != 0 || stats.RobotsDisallow != 1 || s.hitCount("/") != 0 {
		t.Errorf("stats = %+v: a site that disallows everything must not be fetched", stats)
	}

	no := false
	c, sink, _ = newCrawler(t, 1000)
	c.Robots = nil
	c.Run(context.Background(), Target{Name: "site", StartURLs: []string{s.URL + "/"}, RespectRobots: &no})
	if len(sink.pages) != 1 {
		t.Errorf("respect_robots: false should fetch: %d pages", len(sink.pages))
	}
}

func TestCrawl_CrawlDelayAndHostRateAreHonored(t *testing.T) {
	s := newSite(t, "User-agent: *\nCrawl-delay: 0.2\n")
	c, _, _ := newCrawler(t, 1000)
	start := time.Now()
	c.Run(context.Background(), Target{Name: "site", StartURLs: []string{s.URL + "/a"}, MaxDepth: 1, Concurrency: 4})
	// /a, then /c /b / in parallel workers: 4 page fetches at least 200ms
	// apart despite 4 workers and a 1000 req/s limit.
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Errorf("4 pages took %s; Crawl-delay 0.2s should space them >= 600ms", elapsed)
	}

	s2 := newSite(t, "")
	c2, sink, _ := newCrawler(t, 10) // 10 req/s per host, across workers
	start = time.Now()
	c2.Run(context.Background(), Target{Name: "site", StartURLs: []string{s2.URL + "/"}, MaxDepth: 1, Concurrency: 8})
	n := len(sink.pages)
	if min := time.Duration(n-2) * 100 * time.Millisecond; time.Since(start) < min {
		t.Errorf("%d requests in %s at 10 req/s per host (want >= %s)", n, time.Since(start), min)
	}
}

func TestCrawl_ExtractionRulesAndDriftFlag(t *testing.T) {
	s := newSite(t, "")
	c, sink, counters := newCrawler(t, 1000)
	spec := extract.Spec{
		Items:  &extract.ItemsSpec{Selector: ".item", Required: true, Fields: map[string]extract.Rule{"name": {Selector: "h3"}, "price": {Selector: ".price"}}},
		Fields: map[string]extract.Rule{"banner": {Selector: ".banner", Required: true}},
	}
	stats, _ := c.Run(context.Background(), Target{Name: "shop", StartURLs: []string{s.URL + "/products"}, Extract: spec})
	p := sink.pages[0]
	if len(p.Items) != 2 || p.Items[1]["name"] != "Toaster" || p.Items[1]["price"] != "19.99" {
		t.Errorf("items = %v", p.Items)
	}
	if p.Extraction == nil || p.Extraction.Missing[0] != "banner" || len(p.RawBody) == 0 {
		t.Errorf("a missing required field should flag the page and keep its HTML: %+v", p.Extraction)
	}
	if stats.Flagged != 1 || counters.Snapshot().CalibrationFlagged != 1 {
		t.Errorf("flagged = %d", stats.Flagged)
	}
}

func TestCrawl_CancellationStopsPromptly(t *testing.T) {
	s := newSite(t, "")
	c, _, _ := newCrawler(t, 2) // slow enough that the crawl can't finish
	// Cancel, as SIGINT/SIGTERM does (a deadline would instead make the
	// rate limiter fail fast on any wait that ends past it).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.Run(ctx, Target{Name: "site", StartURLs: []string{s.URL + "/"}, MaxDepth: 5})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("err = %v after %s; want the context error, promptly", err, time.Since(start))
	}
}

func TestCompile_ValidatesTargets(t *testing.T) {
	bad := []Target{
		{StartURLs: []string{"https://x.test/"}},
		{Name: "n"},
		{Name: "n", StartURLs: []string{"/relative"}},
		{Name: "n", StartURLs: []string{"ftp://x.test/"}},
		{Name: "n", StartURLs: []string{"https://x.test/"}, Render: "magic"},
		{Name: "n", StartURLs: []string{"https://x.test/"}, Include: []string{"("}},
		{Name: "n", StartURLs: []string{"https://x.test/"}, Follow: "a["},
		{Name: "n", StartURLs: []string{"https://x.test/"}, MaxDepth: -1},
	}
	for _, target := range bad {
		if _, err := compile(target); err == nil {
			t.Errorf("%+v: want an error", target)
		}
	}
	s, err := compile(Target{Name: "n", StartURLs: []string{"HTTPS://Shop.Example.TEST:443/Path#x"}, AllowedDomains: []string{".example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.starts[0] != "https://shop.example.test/Path" || s.MaxPages != DefaultMaxPages || s.Render != "http" || !*s.RespectRobots {
		t.Errorf("compiled = %+v", s)
	}
	if !s.follows("https://www.example.test/x") || s.follows("https://example.test.evil/x") || s.follows("https://notexample.test/") {
		t.Error("allowed_domains should cover the domain and its subdomains only")
	}
}
