package queue

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

func TestRedisStreamSource_PushAndConsume(t *testing.T) {
	client := newTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	src := &RedisStreamSource{
		Client:   client,
		Stream:   "serp-queries",
		Group:    "harvesters",
		Consumer: "worker-1",
		BlockFor: 200 * time.Millisecond,
	}
	if err := src.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	queries := []string{"query one", "query two", "query three"}
	for _, q := range queries {
		if err := src.PushQuery(ctx, q); err != nil {
			t.Fatalf("PushQuery(%q): %v", q, err)
		}
	}

	jobs := src.Jobs(ctx)

	got := make(map[string]bool)
	for i := 0; i < len(queries); i++ {
		select {
		case job, ok := <-jobs:
			if !ok {
				t.Fatalf("jobs channel closed early after %d jobs", i)
			}
			got[job.Query] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for job %d", i)
		}
	}

	for _, q := range queries {
		if !got[q] {
			t.Errorf("expected to receive query %q, did not", q)
		}
	}
}

func TestRedisStreamSource_EnsureGroupIdempotent(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()

	src := &RedisStreamSource{Client: client, Stream: "s", Group: "g", Consumer: "c"}
	if err := src.EnsureGroup(ctx); err != nil {
		t.Fatalf("first EnsureGroup: %v", err)
	}
	if err := src.EnsureGroup(ctx); err != nil {
		t.Fatalf("second EnsureGroup should not error (BUSYGROUP is expected): %v", err)
	}
}

func TestRedisStreamSource_StopsOnContextCancel(t *testing.T) {
	client := newTestRedis(t)
	ctx, cancel := context.WithCancel(context.Background())

	src := &RedisStreamSource{
		Client:   client,
		Stream:   "empty-stream",
		Group:    "g",
		Consumer: "c",
		BlockFor: 100 * time.Millisecond,
	}
	if err := src.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	jobs := src.Jobs(ctx)
	cancel()

	select {
	case _, ok := <-jobs:
		if ok {
			t.Fatal("expected no jobs from an empty stream")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Jobs channel did not close after context cancellation")
	}
}
