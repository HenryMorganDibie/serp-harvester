package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/HenryMorganDibie/web-harvester/internal/queue"
)

// recordingProducer captures every job pushed to it — proves the API layer
// only ever enqueues, never fetches or parses itself.
type recordingProducer struct {
	mu   sync.Mutex
	jobs []queue.Job
	err  error
}

func (p *recordingProducer) PushJob(ctx context.Context, job queue.Job) error {
	if p.err != nil {
		return p.err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobs = append(p.jobs, job)
	return nil
}

func (p *recordingProducer) Jobs() []queue.Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]queue.Job(nil), p.jobs...)
}

type fakeCounter struct {
	counts map[string]int64
	err    error
}

func (c *fakeCounter) CountByRunID(ctx context.Context, runID string) (int64, error) {
	if c.err != nil {
		return 0, c.err
	}
	return c.counts[runID], nil
}

func TestServer_CreateJob_EnqueuesOneJobPerQuery(t *testing.T) {
	producer := &recordingProducer{}
	srv := NewServer(producer, nil)

	body := `{"queries": ["query one", "query two"], "country": "US", "language": "en", "device": "mobile"}`
	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp createJobResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.QueriesAccepted != 2 {
		t.Errorf("expected 2 queries accepted, got %d", resp.QueriesAccepted)
	}
	if resp.RunID == "" {
		t.Error("expected a non-empty run_id")
	}

	jobs := producer.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs pushed to the producer, got %d", len(jobs))
	}
	for _, j := range jobs {
		if j.RunID != resp.RunID {
			t.Errorf("expected job RunID %q to match response, got %q", resp.RunID, j.RunID)
		}
		if j.Locale != "US-en" {
			t.Errorf("expected locale 'US-en', got %q", j.Locale)
		}
		if j.Device != "mobile" {
			t.Errorf("expected device 'mobile', got %q", j.Device)
		}
	}
	if jobs[0].Query != "query one" || jobs[1].Query != "query two" {
		t.Errorf("unexpected query values: %+v", jobs)
	}
}

func TestServer_CreateJob_RejectsEmptyQueries(t *testing.T) {
	srv := NewServer(&recordingProducer{}, nil)

	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{"queries": []}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty queries, got %d", w.Code)
	}
}

func TestServer_CreateJob_RejectsInvalidJSON(t *testing.T) {
	srv := NewServer(&recordingProducer{}, nil)

	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`not json`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestServer_CreateJob_ProducerFailureReturnsBadGateway(t *testing.T) {
	producer := &recordingProducer{err: errors.New("redis unreachable")}
	srv := NewServer(producer, nil)

	req := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{"queries": ["q"]}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 when the producer fails, got %d", w.Code)
	}
}

func TestServer_JobStatus_WithCounter(t *testing.T) {
	producer := &recordingProducer{}
	counter := &fakeCounter{counts: map[string]int64{}}
	srv := NewServer(producer, counter)

	createReq := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{"queries": ["a", "b", "c"]}`))
	createW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createW, createReq)

	var created createJobResponse
	json.Unmarshal(createW.Body.Bytes(), &created)

	counter.counts[created.RunID] = 2 // 2 of 3 completed so far

	statusReq := httptest.NewRequest(http.MethodGet, "/jobs/"+created.RunID, nil)
	statusW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(statusW, statusReq)

	if statusW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", statusW.Code, statusW.Body.String())
	}

	var status jobStatusResponse
	if err := json.Unmarshal(statusW.Body.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if status.Submitted != 3 || !status.SubmittedKnown {
		t.Errorf("expected submitted=3 (known), got %+v", status)
	}
	if status.Completed != 2 || !status.Known {
		t.Errorf("expected completed=2 (known), got %+v", status)
	}
}

func TestServer_JobStatus_WithoutCounter_CompletedUnknown(t *testing.T) {
	producer := &recordingProducer{}
	srv := NewServer(producer, nil) // no counter, e.g. sink_backend: jsonl

	createReq := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewBufferString(`{"queries": ["a"]}`))
	createW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createW, createReq)
	var created createJobResponse
	json.Unmarshal(createW.Body.Bytes(), &created)

	statusReq := httptest.NewRequest(http.MethodGet, "/jobs/"+created.RunID, nil)
	statusW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(statusW, statusReq)

	var status jobStatusResponse
	json.Unmarshal(statusW.Body.Bytes(), &status)

	if status.Known {
		t.Error("expected Known=false when no ResultCounter is configured")
	}
	if status.Completed != -1 {
		t.Errorf("expected Completed=-1 (undetermined) rather than a fabricated value, got %d", status.Completed)
	}
}

func TestServer_JobStatus_UnknownRunID_SubmittedNotClaimedAsZero(t *testing.T) {
	srv := NewServer(&recordingProducer{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/jobs/never-submitted", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var status jobStatusResponse
	json.Unmarshal(w.Body.Bytes(), &status)

	if status.SubmittedKnown {
		t.Error("expected SubmittedKnown=false for a run_id this process never submitted")
	}
}

func TestServer_Healthz(t *testing.T) {
	srv := NewServer(&recordingProducer{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from /healthz, got %d", w.Code)
	}
}
