# Production Readiness

A structured answer to "is this ready for production," section by section.
Where something is already covered in depth elsewhere, this points there
instead of duplicating it — this document is the index, not a second copy.

## 1. Architecture

Queue → worker pool → proxy pool → rate limiter → fetcher → parser → sink,
every stage an interface or a small struct. See [README "Architecture"](README.md#architecture).

## 2. Deployment

Docker Compose (full stack: harvester + Redis + Prometheus + Grafana) or
systemd, both running entirely on infrastructure you control. See
[`deploy/README.md`](deploy/README.md).

## 3. Horizontal scaling

`queue.RedisStreamSource` lets any number of harvester processes share one
durable queue via a consumer group. Measured orchestration throughput
scaling by concurrency is in [README "Load testing"](README.md#load-testing)
— real numbers, not projections, from `cmd/loadtest -sweep`.

## 4. Queue durability

Redis Streams with consumer groups: unacknowledged entries (a worker that
crashes mid-job) stay claimable by another consumer rather than being lost.
See [RUNBOOK.md "Queue backlog growing"](RUNBOOK.md#queue-backlog-growing-redis-streams-mode)
for the operational side of this.

## 5. Retry semantics

Configurable `max_retries` with exponential backoff and jitter
(`internal/worker/pool.go`'s `backoff`). A job that exhausts retries, or is
still in flight when the run shuts down, is counted as dropped
(`serp_harvester_dropped_total`), never silently unaccounted for —
success+dropped is a complete count of every job actually attempted. This
specific accounting edge case (in-flight jobs at shutdown) was found and
fixed during the soak test run in [README "Load testing"](README.md#load-testing),
not designed in from the start — worth knowing when judging how this repo's
test coverage was actually built.

## 6. Proxy management

Health-tracked pool (`internal/proxy`): per-proxy success/failure counts,
failure-threshold cooldown, and a choice of three rotation strategies
(round-robin, random, weighted-by-success-rate). `Pool.Reload(urls)`
reconfigures the proxy list at runtime without dropping accumulated health
stats for URLs that stay — the seam for pointing at a vendor's proxy-list
API instead of static config. Authenticated proxies
(`http://user:pass@host:port`) work with no special handling; confirmed by
an integration test that checks the header actually arrives at a fake proxy
server, not just that Go's docs say it should
(`TestHTTPFetcher_AuthenticatedProxy`). `serp_harvester_proxy_banned_total`
tracks ban events for alerting. What's not included: automatically sourcing
proxies from a vendor's API — `Reload` is the integration point, but nothing
calls it on a schedule yet.

## 7. Rate limiting

Two layers: a per-key (per-proxy, or per-direct-egress-IP) token bucket
(`internal/ratelimit`) for self-imposed pacing, and provider-signaled
rate-limit handling for the provider fetch path — a 429 is parsed into a
`*fetcher.RateLimitError` carrying the provider's own `Retry-After` value,
and the worker pool waits that exact duration before retrying instead of
guessing with generic backoff (`TestPool_HonorsRateLimitRetryAfter`).

## 8. Google response handling

Two fetch paths with very different maturity — see
[README "Honest limitations"](README.md#honest-limitations) and
[README "Live mode"](README.md#live-mode) for the tested, specific findings
on direct-to-Google requests (both a consent interstitial and a
JS-execution check were observed, neither bypassed). The provider fetch
path is the one built for real reliability here.

## 9. AI Overview extraction

Handled on both fetch paths differently: the HTML parser looks for an
inline block (fixture-verified, not live-Google-verified — see the first
item in "Honest limitations" below); the provider path handles the documented two-step
`page_token` → `engine=google_ai_overview` follow-up automatically,
including graceful degradation if the follow-up fails. See
[README "Third-party provider fetcher"](README.md#third-party-provider-fetcher).

## 10. Observability

Prometheus `/metrics` (throughput, failure/retry/drop counts, AI Overview
hit rate, parser drift rate, proxy ban rate, latency p50/p95/p99) plus a
`/healthz` liveness endpoint. Pre-built Grafana dashboard in
`deploy/grafana/provisioning/`. See
[README "Observability"](README.md#observability-prometheus-metrics).

## 11. Failure recovery

Per-job retry with backoff; proxy cooldown and automatic recovery after
`proxy_ban_cooldown`; a failed AI Overview follow-up degrades to "no AI
Overview" rather than failing the whole result; a failed metrics-server
start doesn't crash the harvest. Process-level recovery (systemd
`Restart=on-failure`, or Docker's restart policy) is configured in
`deploy/`.

## 12. Capacity planning

10M requests/day = ~116 req/s sustained. See
[README "Scaling to 10M+ requests/day"](README.md#scaling-to-10m-requests-day)
for the model, and [README "Load testing"](README.md#load-testing) for
measured orchestration throughput well past that number — the real
constraint at that volume is proxy/provider capacity, not this codebase.

## 13. Security

- Provider API keys and the Postgres DSN are both read from environment
  variables (`provider_api_key_env`, `postgres_dsn_env`), never written to
  config files or committed — see `.env.example`.
- The systemd unit runs as a dedicated non-root user with
  `NoNewPrivileges`/`ProtectSystem` hardening.
- No secrets are baked into the Docker image (`deploy/Dockerfile`).
- Not yet addressed: TLS termination in front of `/metrics` and `/healthz`
  (currently plain HTTP, intended for an internal network / behind a
  reverse proxy, not public internet exposure).

## 14. Data retention

Two sinks, two answers. `JSONLSink` has no retention logic of its own — it's
whatever file/log rotation you put around it. `PostgresSink` stores every
result as a row with a `created_at` timestamp, so retention is a matter of a
scheduled `DELETE ... WHERE created_at < ...` or a partition-based policy —
neither is included here, since retention windows are a client policy
decision, not something to hardcode. The `store.Sink` interface remains the
integration point for either a custom retention-aware sink or a
warehouse-streaming one.

## 15. Disaster recovery

Redis Streams persistence configuration (AOF/RDB) is Redis's own concern,
not configured by this repo — a production deployment should enable
whichever persistence mode matches the durability the client needs, and
that decision belongs with whoever operates the Redis instance. No backup
automation is included here.

## 16. Operational runbook

[`RUNBOOK.md`](RUNBOOK.md) — success-rate drops, parser drift, queue
backlog, AI Overview anomalies, metrics endpoint failures, all keyed to
metrics this codebase actually emits.

## 17. Known limitations

The full, honest list is in [README "Honest limitations"](README.md#honest-limitations)
and is intentionally not duplicated here. In short: direct-to-Google fetching
is confirmed blocked at the consent/JS-check layer with no CAPTCHA handling
or headless rendering built (a deliberate scope boundary, not an oversight);
the provider fetch path is the one built for real volume; and no number in
this repository claims a production operating history that doesn't exist —
see the "Production history" note in the README's top summary.
