package fetcher

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func newTestHTTPFetcher(t *testing.T, endpoint string) *HTTPFetcher {
	t.Helper()
	f, err := NewHTTPFetcher(endpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/playwright/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHTTPFetcher_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "heat pump" || r.URL.Query().Get("hl") != "en" {
			http.Error(w, "bad params", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`<html><body><div class="organic-result">ok</div></body></html>`))
	}))
	defer srv.Close()

	resp, err := newTestHTTPFetcher(t, srv.URL+"/search").Fetch(context.Background(), Request{Query: "heat pump", Language: "en"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.StatusCode != 200 || !bytes.Contains(resp.Body, []byte("organic-result")) {
		t.Errorf("unexpected response: %d %q", resp.StatusCode, resp.Body)
	}
}

func TestHTTPFetcher_RateLimitWithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	_, err := newTestHTTPFetcher(t, srv.URL+"/search").Fetch(context.Background(), Request{Query: "q"})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 5*time.Second {
		t.Fatalf("err = %v, want RateLimitError with RetryAfter 5s", err)
	}
	if Classify(err) != OutcomeRateLimited {
		t.Errorf("Classify = %s", Classify(err))
	}
}

func TestHTTPFetcher_CaptchaPageIsBlockedEvenWith429(t *testing.T) {
	page := readFixture(t, "captcha.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(page)
	}))
	defer srv.Close()

	_, err := newTestHTTPFetcher(t, srv.URL+"/search").Fetch(context.Background(), Request{Query: "q"})
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "captcha" || blocked.StatusCode != 429 {
		t.Fatalf("err = %v, want captcha BlockedError with status 429", err)
	}
}

func TestHTTPFetcher_RedirectsToBlockPages(t *testing.T) {
	cases := map[string]string{
		"/sorry/index":                "captcha",
		"/httpservice/retry/enablejs": "interstitial",
	}
	for target, reason := range cases {
		t.Run(reason, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target+"?continue=x", http.StatusFound)
			})
			mux.HandleFunc(target, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("<html><body>please wait</body></html>"))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			_, err := newTestHTTPFetcher(t, srv.URL+"/search").Fetch(context.Background(), Request{Query: "q"})
			var blocked *BlockedError
			if !errors.As(err, &blocked) || blocked.Reason != reason {
				t.Fatalf("err = %v, want %s BlockedError", err, reason)
			}
		})
	}
}

// consentServer models a consent wall: /search redirects to /consent until
// a decision cookie is set by POST /save, which then redirects back.
func consentServer(t *testing.T, consentPage []byte, saves *atomic.Int32, choice *atomic.Value) *httptest.Server {
	t.Helper()
	results := readFixture(t, "js_results.html")
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("fixture_consent"); err != nil || c.Value == "" {
			http.Redirect(w, r, "/consent?continue="+url.QueryEscape(r.URL.String()), http.StatusFound)
			return
		}
		w.Write(results)
	})
	mux.HandleFunc("/consent", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.ReplaceAll(consentPage, []byte("{{CONTINUE}}"), []byte(r.URL.Query().Get("continue"))))
	})
	mux.HandleFunc("/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		saves.Add(1)
		choice.Store(r.FormValue("set_eom"))
		http.SetCookie(w, &http.Cookie{Name: "fixture_consent", Value: "decided", Path: "/"})
		http.Redirect(w, r, r.FormValue("continue"), http.StatusSeeOther)
	})
	return httptest.NewServer(mux)
}

func TestHTTPFetcher_DismissesConsentWithRejectAll(t *testing.T) {
	var saves atomic.Int32
	var choice atomic.Value
	srv := consentServer(t, readFixture(t, "consent.html"), &saves, &choice)
	defer srv.Close()

	f := newTestHTTPFetcher(t, srv.URL+"/search")
	var handled atomic.Int32
	f.OnConsentHandled = func() { handled.Add(1) }

	resp, err := f.Fetch(context.Background(), Request{Query: "q1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Contains(resp.Body, []byte("Rendered results fixture")) {
		t.Errorf("expected the results page after consent, got %q", resp.Body[:80])
	}
	if got, _ := choice.Load().(string); got != "true" {
		t.Errorf("submitted set_eom=%q, want true (reject all)", got)
	}

	// The decision cookie persists in the jar: no second consent round.
	if _, err := f.Fetch(context.Background(), Request{Query: "q2"}); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if saves.Load() != 1 || handled.Load() != 1 {
		t.Errorf("saves=%d handled=%d, want 1 each", saves.Load(), handled.Load())
	}
}

func TestHTTPFetcher_ConsentWithoutRejectFormIsBlocked(t *testing.T) {
	var saves atomic.Int32
	var choice atomic.Value
	acceptOnly := []byte(`<html><body><form action="/save" method="POST"><input type="hidden" name="set_eom" value="false"><button>Accept all</button></form></body></html>`)
	srv := consentServer(t, acceptOnly, &saves, &choice)
	defer srv.Close()

	_, err := newTestHTTPFetcher(t, srv.URL+"/search").Fetch(context.Background(), Request{Query: "q"})
	if Classify(err) != OutcomeConsent {
		t.Fatalf("err = %v, want a consent block", err)
	}
	if saves.Load() != 0 {
		t.Error("fetcher must not submit a form that isn't reject-all")
	}
}

func TestHTTPFetcher_StatusErrors(t *testing.T) {
	for status, want := range map[int]Outcome{503: OutcomeServerError, 404: OutcomeClientError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		_, err := newTestHTTPFetcher(t, srv.URL+"/search").Fetch(context.Background(), Request{Query: "q"})
		srv.Close()
		var se *StatusError
		if !errors.As(err, &se) || se.StatusCode != status || Classify(err) != want {
			t.Errorf("status %d: err = %v, outcome %s, want %s", status, err, Classify(err), want)
		}
	}
}

func TestHTTPFetcher_ConnectionRefusedIsTransport(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens here any more

	_, err = newTestHTTPFetcher(t, "http://"+addr+"/search").Fetch(context.Background(), Request{Query: "q"})
	if Classify(err) != OutcomeTransport {
		t.Fatalf("err = %v classified %s, want transport", err, Classify(err))
	}
}

func TestHTTPFetcher_ReusesTransportPerProxy(t *testing.T) {
	f := newTestHTTPFetcher(t, "http://example.test/search")
	a1, _ := f.transportFor("http://p1:8080")
	a2, _ := f.transportFor("http://p1:8080")
	b, _ := f.transportFor("http://p2:8080")
	direct, _ := f.transportFor("")
	if a1 != a2 {
		t.Error("same proxy should reuse one transport (connection pool)")
	}
	if a1 == b || a1 == direct {
		t.Error("different proxies must not share a transport")
	}
	if _, err := f.transportFor("://bad"); err == nil {
		t.Error("expected an error for an invalid proxy url")
	}
}

func TestHTTPFetcher_JSCheckShellIsInterstitial(t *testing.T) {
	page := readFixture(t, "js_check.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(page)
	}))
	defer srv.Close()

	_, err := newTestHTTPFetcher(t, srv.URL+"/search").Fetch(context.Background(), Request{Query: "q"})
	if Classify(err) != OutcomeInterstitial {
		t.Fatalf("err = %v, want an interstitial (page needs JavaScript)", err)
	}
}

func TestRequiresJS(t *testing.T) {
	if !requiresJS(`<html><noscript><meta content="0;url=/httpservice/retry/enablejs?sei=1" http-equiv="refresh"></noscript></html>`) {
		t.Error("attribute order should not matter")
	}
	if requiresJS(`<html><body><a href="/httpservice/retry/enablejs">help</a></body></html>`) {
		t.Error("a plain link outside noscript is not a JS-check shell")
	}
	if requiresJS(`<html><noscript><meta http-equiv="refresh" content="0;url=/other"></noscript></html>`) {
		t.Error("a noscript refresh elsewhere is not the JS check")
	}
}
