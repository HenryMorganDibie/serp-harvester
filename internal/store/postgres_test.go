package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/HenryMorganDibie/web-harvester/internal/model"
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

func TestPostgresSink_CountByRunID(t *testing.T) {
	sink := newTestSink(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		err := sink.Write(&model.SerpResult{
			Query:     "query",
			RunID:     "run-a",
			FetchedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := sink.Write(&model.SerpResult{Query: "query", RunID: "run-b", FetchedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	count, err := sink.CountByRunID(ctx, "run-a")
	if err != nil {
		t.Fatalf("CountByRunID: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 results for run-a, got %d", count)
	}

	count, err = sink.CountByRunID(ctx, "run-b")
	if err != nil {
		t.Fatalf("CountByRunID: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 result for run-b, got %d", count)
	}

	count, err = sink.CountByRunID(ctx, "nonexistent-run")
	if err != nil {
		t.Fatalf("CountByRunID: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 results for a nonexistent run, got %d", count)
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

func TestPostgresSink_WritePageAndReadBack(t *testing.T) {
	sink := newTestSink(t)
	ctx := context.Background()
	if _, err := sink.db.ExecContext(ctx, "TRUNCATE TABLE pages"); err != nil {
		t.Fatal(err)
	}

	full := &model.Page{
		Target: "shop", RunID: "crawl-1", URL: "https://shop.test/k", FinalURL: "https://shop.test/kettles",
		Depth: 1, StatusCode: 200, ContentType: "text/html", FetchedAt: time.Now().UTC(), LatencyMS: 42,
		Title: "Kettles", Description: "d", Canonical: "https://shop.test/kettles", Language: "en",
		Headings:  []model.Heading{{Level: 1, Text: "Kettles"}},
		OpenGraph: map[string]string{"og:title": "Kettles"},
		JSONLD:    []json.RawMessage{json.RawMessage(`{"@type":"ItemList"}`)},
		Text:      "Kettles Steel £29.99",
		Links:     []string{"https://shop.test/p/1"},
		Fields:    map[string]any{"category": "Kettles"},
		Items:     []map[string]any{{"name": "Steel", "price": "£29.99"}},
	}
	flagged := &model.Page{
		Target: "shop", RunID: "crawl-1", URL: "https://shop.test/odd", FetchedAt: time.Now().UTC(),
		Extraction: &model.ExtractionNote{Missing: []string{"items"}}, RawBody: []byte("<html>odd</html>"),
	}
	for _, p := range []*model.Page{full, flagged} {
		if err := sink.WritePage(p); err != nil {
			t.Fatalf("WritePage: %v", err)
		}
	}
	if n, err := sink.CountPagesByRunID(ctx, "crawl-1"); err != nil || n != 2 {
		t.Fatalf("count = %d, %v", n, err)
	}

	var itemName, ogTitle, jsonldType string
	var nullFields, nullRaw bool
	err := sink.db.QueryRowContext(ctx, `
		SELECT items->0->>'name', open_graph->>'og:title', json_ld->0->>'@type', fields IS NULL, raw_response IS NULL
		FROM pages WHERE url = $1`, full.URL).Scan(&itemName, &ogTitle, &jsonldType, &nullFields, &nullRaw)
	if err != nil {
		t.Fatal(err)
	}
	if itemName != "Steel" || ogTitle != "Kettles" || jsonldType != "ItemList" || nullFields || !nullRaw {
		t.Errorf("row: item=%q og=%q jsonld=%q fields_null=%v raw_null=%v", itemName, ogTitle, jsonldType, nullFields, nullRaw)
	}

	var missing string
	var linksNull, itemsNull bool
	var raw []byte
	err = sink.db.QueryRowContext(ctx, `
		SELECT extraction->'missing'->>0, links IS NULL, items IS NULL, raw_response FROM pages WHERE url = $1`,
		flagged.URL).Scan(&missing, &linksNull, &itemsNull, &raw)
	if err != nil {
		t.Fatal(err)
	}
	if missing != "items" || !linksNull || !itemsNull || string(raw) != "<html>odd</html>" {
		t.Errorf("flagged row: missing=%q links_null=%v items_null=%v raw=%q (empty columns must be SQL NULL)", missing, linksNull, itemsNull, raw)
	}
}
