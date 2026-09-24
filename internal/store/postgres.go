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
	switch x := v.(type) {
	case *model.FeaturedSnippet:
		return x == nil
	case *model.AIOverview:
		return x == nil
	case *model.CalibrationNote:
		return x == nil
	case []string:
		return x == nil
	default:
		return v == nil
	}
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
