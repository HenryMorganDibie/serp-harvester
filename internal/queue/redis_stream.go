package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStreamSource consumes jobs from a Redis Stream via a consumer group,
// so many worker processes — on one host or spread across many — can share
// a single durable queue instead of each needing its own in-memory list of
// queries. This is the concrete answer to "how does a single-process demo
// become N processes across M hosts": they all point at the same stream and
// consumer group, and Redis divides work between them.
//
// Producers call PushQuery (e.g. from an API, a scheduler, or a backfill
// script); any number of harvester processes call Jobs to consume.
type RedisStreamSource struct {
	Client   *redis.Client
	Stream   string
	Group    string
	Consumer string

	// BlockFor is how long a single XREADGROUP call waits for new entries
	// before returning empty. Defaults to 2s if zero.
	BlockFor time.Duration
}

// EnsureGroup creates the consumer group (and the stream, if it doesn't
// exist yet) starting from the beginning of the stream. Safe to call every
// time a consumer starts up: an already-existing group is not an error.
func (s *RedisStreamSource) EnsureGroup(ctx context.Context) error {
	err := s.Client.XGroupCreateMkStream(ctx, s.Stream, s.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("queue: create consumer group: %w", err)
	}
	return nil
}

// PushQuery adds one query to the stream with no run/locale/device metadata.
func (s *RedisStreamSource) PushQuery(ctx context.Context, query string) error {
	return s.PushJob(ctx, Job{Query: query})
}

// PushJob adds one job to the stream, carrying RunID/Locale/Device through
// so a consumer's Jobs() reconstructs the same Job. This is what the job/API
// layer and scheduler use instead of PushQuery.
func (s *RedisStreamSource) PushJob(ctx context.Context, job Job) error {
	err := s.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: s.Stream,
		Values: map[string]interface{}{
			"query":  job.Query,
			"run_id": job.RunID,
			"locale": job.Locale,
			"device": job.Device,
		},
	}).Err()
	if err != nil {
		return fmt.Errorf("queue: push job: %w", err)
	}
	return nil
}

// Jobs implements Source. It reads new entries for this consumer within
// Group, acknowledging each one only after it has been handed off on the
// returned channel — so a worker that crashes mid-job leaves the entry
// pending for another consumer to claim, instead of silently dropping it.
func (s *RedisStreamSource) Jobs(ctx context.Context) <-chan Job {
	block := s.BlockFor
	if block <= 0 {
		block = 2 * time.Second
	}

	ch := make(chan Job)
	go func() {
		defer close(ch)
		for {
			if ctx.Err() != nil {
				return
			}

			streams, err := s.Client.XReadGroup(ctx, &redis.XReadGroupArgs{
				Group:    s.Group,
				Consumer: s.Consumer,
				Streams:  []string{s.Stream, ">"},
				Count:    10,
				Block:    block,
			}).Result()
			if err != nil {
				if errors.Is(err, redis.Nil) || ctx.Err() != nil {
					continue // no new entries in this window, or shutting down
				}
				// Transient error (e.g. connection blip): brief backoff, retry.
				select {
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					return
				}
				continue
			}

			for _, stream := range streams {
				for _, msg := range stream.Messages {
					query, _ := msg.Values["query"].(string)
					runID, _ := msg.Values["run_id"].(string)
					locale, _ := msg.Values["locale"].(string)
					device, _ := msg.Values["device"].(string)
					job := Job{Query: query, RunID: runID, Locale: locale, Device: device}
					select {
					case ch <- job:
						s.Client.XAck(ctx, s.Stream, s.Group, msg.ID)
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch
}
