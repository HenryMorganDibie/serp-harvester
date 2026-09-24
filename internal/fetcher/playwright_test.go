package fetcher

import (
	"net/url"
	"os"
	"testing"
)

// These tests cover the Playwright fetcher's pure logic and need no
// browser. Browser-backed tests live in playwright_browser_test.go.

func TestClassifyPage(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("testdata/playwright/" + name)
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		return string(b)
	}

	cases := []struct {
		name string
		url  string
		html string
		want pageKind
	}{
		{"results", "https://www.google.com/search?q=x", read("js_results.html"), pageNormal},
		{"captcha by DOM", "https://www.google.com/search?q=x", read("captcha.html"), pageCaptcha},
		{"captcha by /sorry/ path", "https://www.google.com/sorry/index?continue=x", "<html></html>", pageCaptcha},
		{"consent by DOM", "https://www.google.com/search?q=x", read("consent.html"), pageConsent},
		{"consent by host", "https://consent.google.com/ml?continue=x", "<html></html>", pageConsent},
		{"js-check interstitial", "https://www.google.com/httpservice/retry/enablejs?sei=x", "<html></html>", pageInterstitial},
		// With scripting on, <noscript> content is raw text, not elements,
		// so an ordinary page's noscript fallback link isn't a block.
		{"noscript fallback is not a block", "https://www.google.com/search?q=x",
			`<html><body><noscript><a href="/httpservice/retry/enablejs">x</a><form action="/sorry/">`,
			pageNormal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPage(c.url, c.html); got != c.want {
				t.Errorf("classifyPage = %d, want %d", got, c.want)
			}
		})
	}
}

func TestSearchURL_MatchesHTTPFetcherParams(t *testing.T) {
	got, err := searchURL("https://www.google.com/search", Request{Query: "heat pump", Country: "GB", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	q := u.Query()
	if q.Get("q") != "heat pump" || q.Get("gl") != "GB" || q.Get("hl") != "en" {
		t.Errorf("unexpected query params in %s", got)
	}

	got, _ = searchURL("https://www.google.com/search", Request{Query: "x"})
	u, _ = url.Parse(got)
	if _, ok := u.Query()["gl"]; ok {
		t.Errorf("gl should be omitted when Country is empty: %s", got)
	}
}

func TestLocaleTag(t *testing.T) {
	cases := map[[2]string]string{
		{"en", "US"}: "en-US",
		{"de", "de"}: "de-DE",
		{"fr", ""}:   "fr",
		{"", "US"}:   "",
	}
	for in, want := range cases {
		if got := localeTag(in[0], in[1]); got != want {
			t.Errorf("localeTag(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestDeviceProfileFor(t *testing.T) {
	if d := deviceProfileFor("mobile"); !d.mobile || !d.touch || d.width >= 600 {
		t.Errorf("mobile profile = %+v, want a narrow touch viewport", d)
	}
	if d := deviceProfileFor("Tablet"); !d.touch || d.name != "tablet" {
		t.Errorf("tablet profile = %+v", d)
	}
	for _, in := range []string{"", "desktop", "smartwatch"} {
		if d := deviceProfileFor(in); d.name != "desktop" || d.mobile {
			t.Errorf("deviceProfileFor(%q) = %+v, want desktop", in, d)
		}
	}
}

func TestProxySettings(t *testing.T) {
	p, err := proxySettings("http://user:p%40ss@proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	if p.Server != "http://proxy.example:8080" {
		t.Errorf("server = %q", p.Server)
	}
	if p.Username == nil || *p.Username != "user" || p.Password == nil || *p.Password != "p@ss" {
		t.Errorf("credentials not carried over: %+v", p)
	}

	p, err = proxySettings("socks5://proxy.example:1080")
	if err != nil || p.Username != nil || p.Server != "socks5://proxy.example:1080" {
		t.Errorf("socks5 without auth: %+v, %v", p, err)
	}

	if _, err := proxySettings("not a url"); err == nil {
		t.Error("expected an error for an invalid proxy url")
	}
}

func TestSessionKey_SeparatesContextLevelSettings(t *testing.T) {
	base := Request{Query: "a", ProxyURL: "http://p1:1", UserAgent: "ua", Language: "en", Country: "US", Device: "desktop"}
	sameProfile := base
	sameProfile.Query = "a different query"
	if sessionKey(base) != sessionKey(sameProfile) {
		t.Error("the query must not affect the session key")
	}
	if sessionKey(base) != sessionKey(Request{Query: "a", ProxyURL: "http://p1:1", UserAgent: "ua", Language: "en", Country: "US"}) {
		t.Error("empty device and desktop should share a session")
	}
	for _, mut := range []func(*Request){
		func(r *Request) { r.ProxyURL = "http://p2:1" },
		func(r *Request) { r.UserAgent = "other" },
		func(r *Request) { r.Country = "GB" },
		func(r *Request) { r.Device = "mobile" },
	} {
		r := base
		mut(&r)
		if sessionKey(r) == sessionKey(base) {
			t.Errorf("expected a different session key for %+v", r)
		}
	}
}
