package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
)

type recordingProducer struct {
	mu   sync.Mutex
	jobs []queue.Job
}

func (p *recordingProducer) PushJob(ctx context.Context, job queue.Job) error {
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

func TestScheduler_RejectsInvalidCronExpression(t *testing.T) {
	s := New(&recordingProducer{})
	err := s.AddHarvest(Harvest{Name: "bad", CronExpr: "not a cron expression", Queries: []string{"q"}})
	if err == nil {
		t.Fatal("expected an error for an invalid cron expression")
	}
}

func TestScheduler_FiresOnScheduleAndPushesQueries(t *testing.T) {
	producer := &recordingProducer{}
	s := New(producer)

	err := s.AddHarvest(Harvest{
		Name:     "fast-tick",
		CronExpr: "@every 100ms",
		Queries:  []string{"query one", "query two"},
		Country:  "US",
		Language: "en",
		Device:   "mobile",
	})
	if err != nil {
		t.Fatalf("AddHarvest: %v", err)
	}

	s.Start()
	defer s.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(producer.Jobs()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	jobs := producer.Jobs()
	if len(jobs) < 2 {
		t.Fatalf("expected at least one full tick (2 queries) within 2s, got %d jobs", len(jobs))
	}

	// First tick's two jobs should share a run ID and carry locale/device.
	first, second := jobs[0], jobs[1]
	if first.RunID == "" || first.RunID != second.RunID {
		t.Errorf("expected the first tick's jobs to share a non-empty run ID, got %q and %q", first.RunID, second.RunID)
	}
	if first.Locale != "US-en" || first.Device != "mobile" {
		t.Errorf("expected locale=US-en device=mobile, got locale=%q device=%q", first.Locale, first.Device)
	}
	if first.Query != "query one" || second.Query != "query two" {
		t.Errorf("unexpected query values: %q, %q", first.Query, second.Query)
	}
}

func TestScheduler_EachTickGetsAFreshRunID(t *testing.T) {
	producer := &recordingProducer{}
	s := New(producer)

	if err := s.AddHarvest(Harvest{Name: "fast-tick", CronExpr: "@every 100ms", Queries: []string{"q"}}); err != nil {
		t.Fatalf("AddHarvest: %v", err)
	}

	s.Start()
	defer s.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(producer.Jobs()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	jobs := producer.Jobs()
	if len(jobs) < 2 {
		t.Fatalf("expected at least 2 ticks within 2s, got %d", len(jobs))
	}
	if jobs[0].RunID == jobs[1].RunID {
		t.Error("expected different ticks to get different run IDs")
	}
}
