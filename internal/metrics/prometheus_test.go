package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServeHTTP_ExposesCounters(t *testing.T) {
	c := &Counters{}
	c.IncSuccess()
	c.IncSuccess()
	c.IncFailure()
	c.IncDropped()
	c.IncRetried()
	c.IncRetried()
	c.IncRetried()

	srv := httptest.NewServer(ServeHTTP(c))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)

	checks := []string{
		"serp_harvester_success_total 2",
		"serp_harvester_failure_total 1",
		"serp_harvester_dropped_total 1",
		"serp_harvester_retried_total 3",
	}
	for _, want := range checks {
		if !strings.Contains(text, want) {
			t.Errorf("expected metrics output to contain %q, got:\n%s", want, text)
		}
	}
}
