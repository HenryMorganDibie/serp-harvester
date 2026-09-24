package fetcher

import (
	"context"
	"encoding/json"
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

// TestProviderFetcher_ResolvesAIOverviewPageToken exercises the documented
// two-step flow: the main search response returns only a page_token (no
// inline text_blocks), and Fetch is expected to transparently follow up
// with engine=google_ai_overview and merge the resolved content back in,
// so JSONParser sees one complete result either way.
func TestProviderFetcher_ResolvesAIOverviewPageToken(t *testing.T) {
	var followUpCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("engine") {
		case "google_ai_overview":
			followUpCalls++
			if got := r.URL.Query().Get("page_token"); got != "tok-123" {
				t.Errorf("expected page_token %q, got %q", "tok-123", got)
			}
			w.Write([]byte(`{
				"text_blocks": [{"type": "paragraph", "snippet": "Resolved AI overview content."}],
				"references": [{"link": "https://example.com/resolved-source"}]
			}`))
		default:
			w.Write([]byte(`{
				"organic_results": [{"position": 1, "title": "t", "link": "https://example.com", "snippet": "s"}],
				"ai_overview": {"page_token": "tok-123", "serpapi_link": "https://serpapi.com/search?page_token=tok-123"}
			}`))
		}
	}))
	defer srv.Close()

	f := NewProviderFetcher(srv.URL, "test-key", "", 5*time.Second)
	resp, err := f.Fetch(context.Background(), Request{Query: "golang worker pool"})
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	if followUpCalls != 1 {
		t.Fatalf("expected exactly 1 follow-up call, got %d", followUpCalls)
	}

	var merged struct {
		AIOverview struct {
			TextBlocks []struct {
				Snippet string `json:"snippet"`
			} `json:"text_blocks"`
			References []struct {
				Link string `json:"link"`
			} `json:"references"`
		} `json:"ai_overview"`
		OrganicResults []struct {
			Title string `json:"title"`
		} `json:"organic_results"`
	}
	if err := json.Unmarshal(resp.Body, &merged); err != nil {
		t.Fatalf("failed to unmarshal merged body: %v", err)
	}

	if len(merged.AIOverview.TextBlocks) != 1 || merged.AIOverview.TextBlocks[0].Snippet != "Resolved AI overview content." {
		t.Errorf("expected resolved AI overview text in merged body, got %+v", merged.AIOverview)
	}
	if len(merged.AIOverview.References) != 1 || merged.AIOverview.References[0].Link != "https://example.com/resolved-source" {
		t.Errorf("expected resolved AI overview references in merged body, got %+v", merged.AIOverview)
	}
	if len(merged.OrganicResults) != 1 || merged.OrganicResults[0].Title != "t" {
		t.Errorf("expected organic_results to survive the merge unchanged, got %+v", merged.OrganicResults)
	}
}

// TestProviderFetcher_InlineAIOverviewSkipsFollowUp confirms no follow-up
// request is made when the main response already has inline content.
func TestProviderFetcher_InlineAIOverviewSkipsFollowUp(t *testing.T) {
	var followUpCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("engine") == "google_ai_overview" {
			followUpCalls++
		}
		w.Write([]byte(`{
			"organic_results": [{"position": 1, "title": "t", "link": "https://example.com", "snippet": "s"}],
			"ai_overview": {"text_blocks": [{"type": "paragraph", "snippet": "Already inline."}]}
		}`))
	}))
	defer srv.Close()

	f := NewProviderFetcher(srv.URL, "test-key", "", 5*time.Second)
	if _, err := f.Fetch(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	if followUpCalls != 0 {
		t.Errorf("expected no follow-up call when content is already inline, got %d", followUpCalls)
	}
}

// TestProviderFetcher_AIOverviewFollowUpFailureIsNonFatal confirms that if
// the follow-up request fails (expired token, network error, etc.), Fetch
// still succeeds with the original body — organic results and everything
// else still parse; the AI Overview just stays absent for that result.
func TestProviderFetcher_AIOverviewFollowUpFailureIsNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("engine") == "google_ai_overview" {
			w.WriteHeader(http.StatusGatewayTimeout) // simulates an expired/invalid page_token
			return
		}
		w.Write([]byte(`{
			"organic_results": [{"position": 1, "title": "t", "link": "https://example.com", "snippet": "s"}],
			"ai_overview": {"page_token": "expired-token"}
		}`))
	}))
	defer srv.Close()

	f := NewProviderFetcher(srv.URL, "test-key", "", 5*time.Second)
	resp, err := f.Fetch(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Fetch should succeed even when the AI overview follow-up fails, got error: %v", err)
	}

	var body struct {
		OrganicResults []struct {
			Title string `json:"title"`
		} `json:"organic_results"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}
	if len(body.OrganicResults) != 1 {
		t.Errorf("expected organic_results to survive a failed follow-up, got %+v", body.OrganicResults)
	}
}
