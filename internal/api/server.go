// Package api exposes an HTTP layer for submitting harvest jobs. It is
// strictly a producer: every handler here does exactly one thing — push
// Job values onto a queue.Producer — and never calls a Fetcher or Parser
// itself. That boundary is what keeps the acquisition backend (direct HTTP
// vs. third-party provider) swappable without the job-submission layer
// knowing or caring; workers (cmd/harvester) remain the only thing that
// fetches or parses.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
)

// ResultCounter reports how many results exist for a run so far. PostgresSink
// implements this; JSONLSink doesn't (there's no efficient way to count rows
// in a flat file), so status reporting is best-effort when using JSONL —
// documented in Server.JobStatus, not silently wrong.
type ResultCounter interface {
	CountByRunID(ctx context.Context, runID string) (int64, error)
}

// Server holds the dependencies every handler needs.
type Server struct {
	Producer queue.Producer
	Counter  ResultCounter // optional; nil means "completed" is always unknown

	mu        sync.Mutex
	submitted map[string]int // run ID -> queries submitted, this process's lifetime only
}

// NewServer builds a Server. Counter may be nil.
func NewServer(producer queue.Producer, counter ResultCounter) *Server {
	return &Server{
		Producer:  producer,
		Counter:   counter,
		submitted: make(map[string]int),
	}
}

// Handler returns the full set of routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.handleCreateJob)
	mux.HandleFunc("GET /jobs/{run_id}", s.handleJobStatus)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// createJobRequest is the POST /jobs body.
type createJobRequest struct {
	Queries  []string `json:"queries"`
	Country  string   `json:"country"`  // e.g. "US" — optional
	Language string   `json:"language"` // e.g. "en" — optional
	Device   string   `json:"device"`   // e.g. "desktop", "mobile" — optional
}

type createJobResponse struct {
	RunID           string `json:"run_id"`
	QueriesAccepted int    `json:"queries_accepted"`
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if len(req.Queries) == 0 {
		writeError(w, http.StatusBadRequest, "queries must be a non-empty array")
		return
	}

	runID, err := newRunID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate run ID")
		return
	}

	locale := ""
	if req.Country != "" || req.Language != "" {
		locale = req.Country + "-" + req.Language
	}

	ctx := r.Context()
	for _, q := range req.Queries {
		job := queue.Job{Query: q, RunID: runID, Locale: locale, Device: req.Device}
		if err := s.Producer.PushJob(ctx, job); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("failed to enqueue job: %v", err))
			return
		}
	}

	s.mu.Lock()
	s.submitted[runID] = len(req.Queries)
	s.mu.Unlock()

	writeJSON(w, http.StatusAccepted, createJobResponse{RunID: runID, QueriesAccepted: len(req.Queries)})
}

type jobStatusResponse struct {
	RunID     string `json:"run_id"`
	Submitted int    `json:"submitted"`
	// Completed is -1 when it can't be determined (no ResultCounter
	// configured, e.g. sink_backend: jsonl) — never a fabricated 0.
	Completed int64 `json:"completed"`
	// Known is false when Completed couldn't be determined.
	Known bool `json:"completed_known"`
	// SubmittedKnown is false for a run_id this process didn't submit
	// (e.g. after a restart) — Submitted is 0 in that case, not a claim
	// that 0 queries were submitted.
	SubmittedKnown bool `json:"submitted_known"`
}

func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}

	s.mu.Lock()
	submitted, submittedKnown := s.submitted[runID]
	s.mu.Unlock()

	resp := jobStatusResponse{RunID: runID, Submitted: submitted, SubmittedKnown: submittedKnown, Completed: -1}

	if s.Counter != nil {
		count, err := s.Counter.CountByRunID(r.Context(), runID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to count results: %v", err))
			return
		}
		resp.Completed = count
		resp.Known = true
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "run-" + hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
