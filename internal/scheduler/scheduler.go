// Package scheduler runs recurring harvests on cron schedules — e.g. "query
// set A at 10:00, query set B at 10:15" — by pushing jobs onto the same
// queue.Producer the job/API layer uses. Like internal/api, it is strictly
// a producer: it never calls Fetcher or Parser itself, only PushJob.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/robfig/cron/v3"

	"github.com/HenryMorganDibie/web-harvester/internal/queue"
)

// Harvest describes one recurring job: run Queries on CronExpr's schedule.
type Harvest struct {
	Name     string   `yaml:"name"`
	CronExpr string   `yaml:"cron"` // standard 5-field cron expression, or "@every 15m" etc.
	Queries  []string `yaml:"queries"`
	Country  string   `yaml:"country"`
	Language string   `yaml:"language"`
	Device   string   `yaml:"device"`
}

// Scheduler wraps a cron.Cron, pushing each Harvest's queries onto Producer
// under a fresh run ID every time its schedule fires.
type Scheduler struct {
	Producer queue.Producer
	cron     *cron.Cron
}

// New builds a Scheduler backed by producer.
func New(producer queue.Producer) *Scheduler {
	return &Scheduler{
		Producer: producer,
		cron:     cron.New(),
	}
}

// AddHarvest registers h on its cron schedule. Returns an error if CronExpr
// doesn't parse — checked at registration time, not at the first missed
// tick.
func (s *Scheduler) AddHarvest(h Harvest) error {
	_, err := s.cron.AddFunc(h.CronExpr, func() {
		s.runOnce(h)
	})
	if err != nil {
		return fmt.Errorf("scheduler: invalid cron expression %q for harvest %q: %w", h.CronExpr, h.Name, err)
	}
	return nil
}

func (s *Scheduler) runOnce(h Harvest) {
	runID, err := newRunID()
	if err != nil {
		log.Printf("scheduler: harvest %q: failed to generate run ID: %v", h.Name, err)
		return
	}

	locale := ""
	if h.Country != "" || h.Language != "" {
		locale = h.Country + "-" + h.Language
	}

	ctx := context.Background()
	pushed := 0
	for _, q := range h.Queries {
		job := queue.Job{Query: q, RunID: runID, Locale: locale, Device: h.Device}
		if err := s.Producer.PushJob(ctx, job); err != nil {
			log.Printf("scheduler: harvest %q run %q: failed to push query %q: %v", h.Name, runID, q, err)
			continue
		}
		pushed++
	}
	log.Printf("scheduler: harvest %q fired, run_id=%s, pushed %d/%d queries", h.Name, runID, pushed, len(h.Queries))
}

// Start begins running registered harvests on their schedules. Non-blocking:
// schedules run in their own goroutine (via the underlying cron.Cron).
func (s *Scheduler) Start() {
	s.cron.Start()
}

// Stop halts the scheduler, waiting for any in-progress runOnce calls to
// finish (they're fast — just enqueueing — so this returns quickly).
func (s *Scheduler) Stop() {
	<-s.cron.Stop().Done()
}

func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sched-" + hex.EncodeToString(b), nil
}
