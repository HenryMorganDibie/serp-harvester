package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProviderFetcher_Fetch(t *testing.T) {
	var gotQuery, gotEngine, gotKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotEngine = r.URL.Query().Get("engine")
		gotKey = r.URL.Query().Get("api_key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"organic_results":[{"position":1,"title":"t","link":"https://example.com","snippet":"s"}]}`))
	}))
	defer srv.Close()

	f := NewProviderFetcher(srv.URL, "test-key", "", 5*time.Second)

	resp, err := f.Fetch(context.Background(), Request{Query: "golang worker pool"})
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if len(resp.Body) == 0 {
		t.Error("expected non-empty body")
	}

	if gotQuery != "golang worker pool" {
		t.Errorf("expected query param %q, got %q", "golang worker pool", gotQuery)
	}
	if gotEngine != "google" {
		t.Errorf("expected default engine %q, got %q", "google", gotEngine)
	}
	if gotKey != "test-key" {
		t.Errorf("expected api_key %q, got %q", "test-key", gotKey)
	}
}

func TestProviderFetcher_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid API key"}`))
	}))
	defer srv.Close()

	f := NewProviderFetcher(srv.URL, "bad-key", "google", 5*time.Second)
	if _, err := f.Fetch(context.Background(), Request{Query: "q"}); err == nil {
		t.Fatal("expected an error for a non-200 status")
	}
}

func TestProviderFetcher_InvalidProxyURL(t *testing.T) {
	f := NewProviderFetcher("https://example.com", "key", "google", 5*time.Second)
	_, err := f.Fetch(context.Background(), Request{Query: "q", ProxyURL: "://not-a-url"})
	if err == nil {
		t.Fatal("expected an error for an invalid proxy URL")
	}
}
