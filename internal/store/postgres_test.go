package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/model"
)

// requirePostgres skips unless POSTGRES_TEST_DSN is set, so these tests
// never run in normal CI (no Postgres available there) but can be run
// locally or in an environment that has one, e.g.:
//
//	POSTGRES_TEST_DSN="postgres://postgres:testpass@localhost:15432/testdb?sslmode=disable" \
//	  go test ./internal/store/... -run Postgres -v
func requirePostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set POSTGRES_TEST_DSN to run PostgresSink tests against a real Postgres")
	}
	return dsn
}

func newTestSink(t *testing.T) *PostgresSink {
	t.Helper()
	dsn := requirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sink, err := NewPostgresSink(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresSink: %v", err)
	}
	t.Cleanup(func() { sink.Close() })

	// Isolate each test run: truncate rather than assume an empty table.
	if _, err := sink.db.ExecContext(ctx, "TRUNCATE TABLE serp_results"); err != nil {
		t.Fatalf("truncate serp_results: %v", err)
	}
	return sink
}

func TestPostgresSink_WriteAndReadBack(t *testing.T) {
	sink := newTestSink(t)

	result := &model.SerpResult{
		Query:     "golang worker pool pattern",
		RunID:     "run-123",
		Locale:    "US-en",
		Device:    "desktop",
		FetchedAt: time.Now().UTC().Truncate(time.Second),
		LatencyMS: 250,
		ProxyUsed: "direct",
		Organic: []model.OrganicResult{
			{Position: 1, Title: "t1", URL: "https://example.com/1", Snippet: "s1"},
			{Position: 2, Title: "t2", URL: "https://example.com/2", Snippet: "s2"},
		},
		FeaturedSnippet: &model.FeaturedSnippet{Title: "fs", URL: "https://example.com/fs", Text: "fs text"},
		AIOverview:      &model.AIOverview{Text: "overview text", Sources: []string{"https://example.com/src"}},
		PeopleAlsoAsk:   []string{"question one?", "question two?"},
	}

	if err := sink.Write(result); err != nil {
		t.Fatalf("Write: %v", err)
	}

	row := sink.db.QueryRow(`
		SELECT run_id, query, locale, device, latency_ms, proxy_used, organic, featured_snippet, ai_overview, people_also_ask, calibration, raw_response
		FROM serp_results WHERE query = $1
	`, result.Query)

	var runID, query, locale, device, proxyUsed string
	var latencyMS int64
	var organicJSON, featuredSnippetJSON, aiOverviewJSON, peopleAlsoAskJSON []byte
	var calibrationJSON []byte
	var rawResponse []byte
	if err := row.Scan(&runID, &query, &locale, &device, &latencyMS, &proxyUsed, &organicJSON, &featuredSnippetJSON, &aiOverviewJSON, &peopleAlsoAskJSON, &calibrationJSON, &rawResponse); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if runID != "run-123" || query != result.Query || locale != "US-en" || device != "desktop" || latencyMS != 250 || proxyUsed != "direct" {
		t.Errorf("unexpected scalar columns: run_id=%q query=%q locale=%q device=%q latency_ms=%d proxy_used=%q",
			runID, query, locale, device, latencyMS, proxyUsed)
	}
	if calibrationJSON != nil {
		t.Errorf("expected calibration to be NULL, got %s", calibrationJSON)
	}
	if rawResponse != nil {
		t.Errorf("expected raw_response to be NULL (no calibration), got %d bytes", len(rawResponse))
	}

	var organic []model.OrganicResult
	if err := json.Unmarshal(organicJSON, &organic); err != nil {
		t.Fatalf("unmarshal organic: %v", err)
	}
	if len(organic) != 2 || organic[0].Title != "t1" {
		t.Errorf("unexpected organic column contents: %+v", organic)
	}

	var fs model.FeaturedSnippet
	if err := json.Unmarshal(featuredSnippetJSON, &fs); err != nil {
		t.Fatalf("unmarshal featured_snippet: %v", err)
	}
	if fs.Title != "fs" {
		t.Errorf("unexpected featured_snippet: %+v", fs)
	}

	var ao model.AIOverview
	if err := json.Unmarshal(aiOverviewJSON, &ao); err != nil {
		t.Fatalf("unmarshal ai_overview: %v", err)
	}
	if ao.Text != "overview text" || len(ao.Sources) != 1 {
		t.Errorf("unexpected ai_overview: %+v", ao)
	}

	var paa []string
	if err := json.Unmarshal(peopleAlsoAskJSON, &paa); err != nil {
		t.Fatalf("unmarshal people_also_ask: %v", err)
	}
	if len(paa) != 2 {
		t.Errorf("expected 2 people_also_ask entries, got %d", len(paa))
	}
}

func TestPostgresSink_StoresRawBodyOnCalibration(t *testing.T) {
	sink := newTestSink(t)

	result := &model.SerpResult{
		Query:       "drifted query",
		FetchedAt:   time.Now().UTC(),
		Organic:     nil,
		Calibration: &model.CalibrationNote{MissingBlocks: []string{"organic_results"}},
		RawBody:     []byte(`<html>drifted markup</html>`),
	}
	if err := sink.Write(result); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var calibrationJSON []byte
	var rawResponse []byte
	err := sink.db.QueryRow(
		"SELECT calibration, raw_response FROM serp_results WHERE query = $1",
		result.Query,
	).Scan(&calibrationJSON, &rawResponse)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if calibrationJSON == nil {
		t.Fatal("expected calibration to be non-NULL")
	}
	var cal model.CalibrationNote
	if err := json.Unmarshal(calibrationJSON, &cal); err != nil {
		t.Fatalf("unmarshal calibration: %v", err)
	}
	if len(cal.MissingBlocks) != 1 || cal.MissingBlocks[0] != "organic_results" {
		t.Errorf("unexpected calibration: %+v", cal)
	}

	if string(rawResponse) != "<html>drifted markup</html>" {
		t.Errorf("expected raw_response to match RawBody, got %q", string(rawResponse))
	}
}

func TestPostgresSink_SchemaIsIdempotent(t *testing.T) {
	dsn := requirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sink1, err := NewPostgresSink(ctx, dsn)
	if err != nil {
		t.Fatalf("first NewPostgresSink: %v", err)
	}
	sink1.Close()

	sink2, err := NewPostgresSink(ctx, dsn) // re-applying schema must not error
	if err != nil {
		t.Fatalf("second NewPostgresSink (re-applying schema): %v", err)
	}
	defer sink2.Close()
}

func TestPostgresSink_InvalidDSNFailsFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := NewPostgresSink(ctx, "postgres://nouser:nopass@localhost:1/nonexistent?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("expected an error connecting to a nonexistent database")
	}
}
