package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyGenericPage(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		status int
		html   string
		want   pageKind
	}{
		{"ordinary page", "https://shop.test/", 200, `<html><body><h1>Shop</h1></body></html>`, pageNormal},
		{"contact form with reCAPTCHA", "https://shop.test/contact", 200,
			`<html><body><form><div class="g-recaptcha"></div></form></body></html>`, pageNormal},
		{"login form with Turnstile", "https://shop.test/login", 200,
			`<html><body><form><div class="cf-turnstile"></div></form></body></html>`, pageNormal},
		{"CAPTCHA wall (error status)", "https://shop.test/", 403,
			`<html><body><div class="h-captcha"></div></body></html>`, pageCaptcha},
		{"Cloudflare challenge form", "https://shop.test/", 200,
			`<html><body><form id="challenge-form"></form></body></html>`, pageChallenge},
		{"Cloudflare interstitial title", "https://shop.test/", 503,
			`<html><head><title>Just a moment...</title></head><body></body></html>`, pageChallenge},
		{"'Just a moment' as a normal title", "https://blog.test/", 200,
			`<html><head><title>Just a moment of calm</title></head><body></body></html>`, pageNormal},
		{"challenge platform path", "https://shop.test/cdn-cgi/challenge-platform/h/b/orchestrate", 200, `<html></html>`, pageChallenge},
		{"DataDome", "https://shop.test/", 403,
			`<html><body><iframe src="https://geo.captcha-delivery.com/captcha/?x=1"></iframe></body></html>`, pageChallenge},
		{"PerimeterX", "https://shop.test/", 403, `<html><body><div id="px-captcha"></div></body></html>`, pageChallenge},
		{"Google's /sorry/ path is not special on other sites", "https://shop.test/sorry/returns", 200,
			`<html><body>Returns policy</body></html>`, pageNormal},
	}
	for _, c := range cases {
		if got := classifyGenericPage(c.url, c.status, c.html); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLooksLikeJSShell(t *testing.T) {
	long := strings.Repeat("Real server-rendered content. ", 20)
	cases := map[string]bool{
		`<html><body><div id="root"></div><script src="/app.js"></script></body></html>`:                                       true,
		`<html><body><noscript>You need to enable JavaScript to run this app.</noscript><div id="app"></div><script></script>`: true,
		`<html><body><p>` + long + `</p><script src="/analytics.js"></script></body></html>`:                                   false,
		`<html><body><p>Short page, no scripts.</p></body></html>`:                                                             false,
		`<html><head><script type="application/ld+json">{"@type":"Store"}</script></head><body><h1>Tiny</h1></body></html>`:    false,
	}
	for html, want := range cases {
		if got := looksLikeJSShell(html); got != want {
			t.Errorf("looksLikeJSShell(%.60q...) = %v, want %v", html, got, want)
		}
	}
}

func TestHTTPFetcher_GenericURLFetch(t *testing.T) {
	var gotPath, gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new?x=1", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body><p>` + strings.Repeat("content ", 50) + `</p></body></html>`))
	})
	mux.HandleFunc("/feed.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[]}`))
	})
	mux.HandleFunc("/shell", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><div id="root"></div><script src="/bundle.js"></script></body></html>`))
	})
	mux.HandleFunc("/challenge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`<html><head><title>Just a moment...</title></head><body></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newTestHTTPFetcher(t, "")
	f.Generic = true
	f.DetectAppShells = true
	ctx := context.Background()

	resp, err := f.Fetch(ctx, Request{URL: srv.URL + "/old", Query: "ignored", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/new" || gotQuery != "x=1" {
		t.Errorf("fetched %s?%s: the URL must be used as-is, without q/hl", gotPath, gotQuery)
	}
	if resp.FinalURL != srv.URL+"/new?x=1" || !strings.HasPrefix(resp.ContentType, "text/html") {
		t.Errorf("final url %q, content type %q", resp.FinalURL, resp.ContentType)
	}

	resp, err = f.Fetch(ctx, Request{URL: srv.URL + "/feed.json"})
	if err != nil || resp.ContentType != "application/json" {
		t.Errorf("non-HTML: resp=%+v err=%v", resp, err)
	}

	_, err = f.Fetch(ctx, Request{URL: srv.URL + "/shell"})
	if Classify(err) != OutcomeInterstitial {
		t.Errorf("app shell: %v classified %s, want interstitial (render it in the browser)", err, Classify(err))
	}

	f.DetectAppShells = false
	if resp, err := f.Fetch(ctx, Request{URL: srv.URL + "/shell"}); err != nil || resp.StatusCode != 200 {
		t.Errorf("without a browser to fall back to, an app shell is just the page: %v", err)
	}

	_, err = f.Fetch(ctx, Request{URL: srv.URL + "/challenge"})
	if Classify(err) != OutcomeChallenge {
		t.Errorf("challenge: %v classified %s, want challenge", err, Classify(err))
	}
	if DefaultFailoverOn(Classify(err)) {
		t.Error("a challenge must never fail over to the browser")
	}
}
