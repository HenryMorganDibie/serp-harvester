// Command api runs the job-submission HTTP layer: POST /jobs enqueues
// queries onto the same Redis Stream cmd/harvester consumes from, GET
// /jobs/{run_id} reports progress. It also optionally runs scheduled
// harvests (recurring cron-based query sets) from the same process, since
// both the HTTP handlers and the scheduler are pure producers. This binary
// never fetches or parses a SERP itself — it only ever pushes onto
// queue.Producer, keeping job submission strictly separate from harvesting
// (see internal/api's package doc for why that boundary matters).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/HenryMorganDibie/serp-harvester/internal/api"
	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
	"github.com/HenryMorganDibie/serp-harvester/internal/scheduler"
	"github.com/HenryMorganDibie/serp-harvester/internal/store"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	redisAddr := flag.String("redis-addr", "localhost:6379", "Redis address")
	redisStream := flag.String("redis-stream", "serp-harvester:queries", "Redis stream name (must match cmd/harvester's redis_stream)")
	postgresDSNEnv := flag.String("postgres-dsn-env", "POSTGRES_DSN", "environment variable holding the Postgres DSN, for job-status completion counts (optional)")
	scheduleFile := flag.String("schedule", "", "optional YAML file of scheduled harvests (see internal/scheduler.LoadHarvests); unset disables scheduling entirely")
	flag.Parse()

	client := redis.NewClient(&redis.Options{Addr: *redisAddr})
	producer := &queue.RedisStreamSource{Client: client, Stream: *redisStream}

	var counter api.ResultCounter
	if dsn := os.Getenv(*postgresDSNEnv); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		sink, err := store.NewPostgresSink(ctx, dsn)
		cancel()
		if err != nil {
			log.Fatalf("connect to postgres for job-status counting: %v", err)
		}
		defer sink.Close()
		counter = sink
		log.Println("job status will report completion counts from PostgreSQL")
	} else {
		log.Printf("no %s set: job status will report submitted counts only, completion as unknown (set sink_backend: postgres on the harvester side and point this at the same DSN to enable it)", *postgresDSNEnv)
	}

	srv := api.NewServer(producer, counter)

	httpServer := &http.Server{Addr: *addr, Handler: srv.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *scheduleFile != "" {
		harvests, err := scheduler.LoadHarvests(*scheduleFile)
		if err != nil {
			log.Fatalf("load schedule: %v", err)
		}
		sched := scheduler.New(producer)
		for _, h := range harvests {
			if err := sched.AddHarvest(h); err != nil {
				log.Fatalf("register scheduled harvest: %v", err)
			}
			log.Printf("scheduled harvest %q registered: %q", h.Name, h.CronExpr)
		}
		sched.Start()
		defer sched.Stop()
		log.Printf("scheduler running with %d harvest(s) from %s", len(harvests), *scheduleFile)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("job API listening on %s (POST /jobs, GET /jobs/{run_id}, GET /healthz), pushing to redis stream %q", *addr, *redisStream)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
