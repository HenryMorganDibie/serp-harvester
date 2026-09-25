// Package store's PostgreSQL sink persists results with the metadata a
// client would actually query on: run ID, query text, locale/device,
// timestamps, and the parsed SERP fields as native JSONB columns — not just
// an opaque blob per row.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/HenryMorganDibie/serp-harvester/internal/model"
)

// schema is applied on every NewPostgresSink call via CREATE TABLE/INDEX IF
// NOT EXISTS — safe to run on every process startup, no separate migration
// tool required for this scope.
const schema = `
CREATE TABLE IF NOT EXISTS serp_results (
	id               BIGSERIAL PRIMARY KEY,
	run_id           TEXT,
	query            TEXT NOT NULL,
	locale           TEXT,
	device           TEXT,
	fetched_at       TIMESTAMPTZ NOT NULL,
	latency_ms       BIGINT NOT NULL DEFAULT 0,
	proxy_used       TEXT,
	organic          JSONB,
	featured_snippet JSONB,
	ai_overview      JSONB,
	people_also_ask  JSONB,
	calibration      JSONB,
	raw_response     BYTEA,
	created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_serp_results_run_id    ON serp_results (run_id);
CREATE INDEX IF NOT EXISTS idx_serp_results_query     ON serp_results (query);
CREATE INDEX IF NOT EXISTS idx_serp_results_fetched_at ON serp_results (fetched_at);

CREATE TABLE IF NOT EXISTS pages (
	id           BIGSERIAL PRIMARY KEY,
	run_id       TEXT,
	target       TEXT NOT NULL,
	url          TEXT NOT NULL,
	final_url    TEXT,
	depth        INT NOT NULL DEFAULT 0,
	status_code  INT NOT NULL DEFAULT 0,
	content_type TEXT,
	fetched_at   TIMESTAMPTZ NOT NULL,
	latency_ms   BIGINT NOT NULL DEFAULT 0,
	proxy_used   TEXT,
	title        TEXT,
	description  TEXT,
	canonical    TEXT,
	language     TEXT,
	headings     JSONB,
	open_graph   JSONB,
	json_ld      JSONB,
	text         TEXT,
	links        JSONB,
	fields       JSONB,
	items        JSONB,
	extraction   JSONB,
	raw_response BYTEA,
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_pages_run_id     ON pages (run_id);
CREATE INDEX IF NOT EXISTS idx_pages_target     ON pages (target);
CREATE INDEX IF NOT EXISTS idx_pages_url        ON pages (url);
CREATE INDEX IF NOT EXISTS idx_pages_fetched_at ON pages (fetched_at);
`

// PostgresSink implements Sink by inserting one row per result.
type PostgresSink struct {
	db *sql.DB
}

// NewPostgresSink opens dsn (e.g. "postgres://user:pass@host:5432/dbname"),
// verifies connectivity, and ensures the schema exists.
func NewPostgresSink(ctx context.Context, dsn string) (*PostgresSink, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres sink: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres sink: ping: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres sink: apply schema: %w", err)
	}
	return &PostgresSink{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *PostgresSink) Close() error {
	return s.db.Close()
}

// CountByRunID returns how many results have been written for runID so
// far — what a job/API layer's status endpoint uses to report progress
// against however many queries were submitted for that run.
func (s *PostgresSink) CountByRunID(ctx context.Context, runID string) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM serp_results WHERE run_id = $1", runID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("postgres sink: count by run_id: %w", err)
	}
	return count, nil
}

// WritePage implements PageSink: one row per fetched page, with the
// structured parts as JSONB.
func (s *PostgresSink) WritePage(p *model.Page) error {
	var cols [7][]byte
	for i, v := range []any{p.Headings, p.OpenGraph, p.JSONLD, p.Links, p.Fields, p.Items, p.Extraction} {
		b, err := marshalOptional(v)
		if err != nil {
			return fmt.Errorf("postgres sink: marshal page column %d: %w", i, err)
		}
		cols[i] = b
	}
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO pages
			(run_id, target, url, final_url, depth, status_code, content_type, fetched_at, latency_ms, proxy_used,
			 title, description, canonical, language, headings, open_graph, json_ld, text, links, fields, items,
			 extraction, raw_response)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
	`,
		nullIfEmpty(p.RunID), p.Target, p.URL, nullIfEmpty(p.FinalURL), p.Depth, p.StatusCode, nullIfEmpty(p.ContentType),
		p.FetchedAt, p.LatencyMS, nullIfEmpty(p.ProxyUsed),
		nullIfEmpty(p.Title), nullIfEmpty(p.Description), nullIfEmpty(p.Canonical), nullIfEmpty(p.Language),
		cols[0], cols[1], cols[2], nullIfEmpty(p.Text), cols[3], cols[4], cols[5], cols[6],
		nullBytes(p.RawBody),
	)
	if err != nil {
		return fmt.Errorf("postgres sink: insert page: %w", err)
	}
	return nil
}

// CountPagesByRunID returns how many pages have been written for runID.
func (s *PostgresSink) CountPagesByRunID(ctx context.Context, runID string) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pages WHERE run_id = $1", runID).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres sink: count pages by run_id: %w", err)
	}
	return count, nil
}

// Write implements Sink.
func (s *PostgresSink) Write(result *model.SerpResult) error {
	organic, err := json.Marshal(result.Organic)
	if err != nil {
		return fmt.Errorf("postgres sink: marshal organic: %w", err)
	}

	featuredSnippet, err := marshalOptional(result.FeaturedSnippet)
	if err != nil {
		return fmt.Errorf("postgres sink: marshal featured_snippet: %w", err)
	}
	aiOverview, err := marshalOptional(result.AIOverview)
	if err != nil {
		return fmt.Errorf("postgres sink: marshal ai_overview: %w", err)
	}
	peopleAlsoAsk, err := marshalOptional(result.PeopleAlsoAsk)
	if err != nil {
		return fmt.Errorf("postgres sink: marshal people_also_ask: %w", err)
	}
	calibration, err := marshalOptional(result.Calibration)
	if err != nil {
		return fmt.Errorf("postgres sink: marshal calibration: %w", err)
	}

	_, err = s.db.ExecContext(context.Background(), `
		INSERT INTO serp_results
			(run_id, query, locale, device, fetched_at, latency_ms, proxy_used,
			 organic, featured_snippet, ai_overview, people_also_ask, calibration, raw_response)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	`,
		nullIfEmpty(result.RunID), result.Query, nullIfEmpty(result.Locale), nullIfEmpty(result.Device),
		result.FetchedAt, result.LatencyMS, nullIfEmpty(result.ProxyUsed),
		organic, featuredSnippet, aiOverview, peopleAlsoAsk, calibration,
		nullBytes(result.RawBody),
	)
	if err != nil {
		return fmt.Errorf("postgres sink: insert: %w", err)
	}
	return nil
}

// marshalOptional marshals v unless it's a nil pointer/slice, in which case
// it returns nil so the column stores SQL NULL instead of the JSON literal
// "null".
func marshalOptional(v any) ([]byte, error) {
	if isNilValue(v) {
		return nil, nil
	}
	return json.Marshal(v)
}

func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
