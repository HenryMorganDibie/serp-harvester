package model

import (
	"encoding/json"
	"time"
)

// Page is the structured output of the web crawler for one fetched URL:
// generic page data every HTML page has, plus whatever a target's own
// extraction rules produced.
type Page struct {
	Target string `json:"target"`
	RunID  string `json:"run_id,omitempty"`
	// URL is the URL requested; FinalURL is where redirects ended.
	URL         string    `json:"url"`
	FinalURL    string    `json:"final_url,omitempty"`
	Depth       int       `json:"depth"`
	StatusCode  int       `json:"status_code"`
	ContentType string    `json:"content_type,omitempty"`
	FetchedAt   time.Time `json:"fetched_at"`
	LatencyMS   int64     `json:"latency_ms"`
	ProxyUsed   string    `json:"proxy_used,omitempty"`

	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Canonical   string            `json:"canonical,omitempty"`
	Language    string            `json:"language,omitempty"`
	Headings    []Heading         `json:"headings,omitempty"`
	OpenGraph   map[string]string `json:"open_graph,omitempty"`
	// JSONLD holds each schema.org JSON-LD block as published.
	JSONLD []json.RawMessage `json:"json_ld,omitempty"`
	// Text is the page's visible text, whitespace-collapsed and capped.
	Text  string   `json:"text,omitempty"`
	Links []string `json:"links,omitempty"`

	// Fields and Items are what the target's extraction rules produced.
	Fields map[string]any   `json:"fields,omitempty"`
	Items  []map[string]any `json:"items,omitempty"`

	// Extraction is set when a required field or the item selector
	// matched nothing: the rules no longer fit this page's layout, so the
	// result should not be trusted as complete.
	Extraction *ExtractionNote `json:"extraction,omitempty"`
	// RawBody is kept only when Extraction is set, for retuning rules.
	RawBody []byte `json:"raw_body,omitempty"`
}

// Heading is one h1-h3 heading.
type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
}

// ExtractionNote lists what a target's rules failed to find.
type ExtractionNote struct {
	Missing []string `json:"missing"`
}
