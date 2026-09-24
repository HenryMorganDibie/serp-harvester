package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHTTPFetcher_AuthenticatedProxy confirms an authenticated proxy URL
// (http://user:pass@host:port) actually results in a Proxy-Authorization
// header on the wire — not just that Go's http.Transport is documented to
// do this, but that HTTPFetcher's own Transport wiring doesn't
// accidentally drop it. The fake "proxy" here is just an httptest.Server
// that echoes back whether it received the header.
func TestHTTPFetcher_AuthenticatedProxy(t *testing.T) {
	var gotAuthHeader string

	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Proxy-Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body>ok via proxy</body></html>"))
	}))
	defer fakeProxy.Close()

	proxyURLWithAuth := "http://myuser:mypass@" + fakeProxy.Listener.Addr().String()

	f, err := NewHTTPFetcher("http://example.com/search", 5*time.Second)
	if err != nil {
		t.Fatalf("build fetcher: %v", err)
	}

	_, err = f.Fetch(context.Background(), Request{Query: "q", ProxyURL: proxyURLWithAuth})
	if err != nil {
		t.Fatalf("Fetch through authenticated proxy failed: %v", err)
	}

	if gotAuthHeader == "" {
		t.Fatal("expected the fake proxy to receive a Proxy-Authorization header, got none")
	}
	if !isBasicAuthFor(gotAuthHeader, "myuser", "mypass") {
		t.Errorf("Proxy-Authorization header %q did not decode to expected credentials", gotAuthHeader)
	}
}

func isBasicAuthFor(header, wantUser, wantPass string) bool {
	req := &http.Request{Header: http.Header{"Proxy-Authorization": {header}}}
	user, pass, ok := req.BasicAuth() // BasicAuth also reads "Authorization"; workaround below
	if ok && user == wantUser && pass == wantPass {
		return true
	}
	// http.Request.BasicAuth only reads the "Authorization" header, not
	// "Proxy-Authorization", so re-check by swapping the header name.
	req2 := &http.Request{Header: http.Header{"Authorization": {header}}}
	user2, pass2, ok2 := req2.BasicAuth()
	return ok2 && user2 == wantUser && pass2 == wantPass
}
