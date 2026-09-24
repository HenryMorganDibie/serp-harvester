# serp-harvester

The engineering backbone for a large-scale Google SERP collection pipeline:
concurrent worker pool, proxy health tracking with pluggable rotation
strategies, per-proxy rate limiting with Retry-After-aware backoff, a
distributed queue (Redis Streams) for scaling across processes and hosts,
structured extraction (organic results, featured snippets, "people also
ask", AI Overviews), drift detection when a page's layout no longer matches
expected selectors, a third-party SERP-provider fetch path, PostgreSQL or
JSON-Lines persistence with run/locale/device metadata, and Prometheus
metrics for the same throughput numbers a request-volume SLA is measured in.

It runs end to end, offline, with `go run ./cmd/harvester` — no API keys, no
proxies, no network access required.

## At a glance

- **What it does**: Google SERP collection (organic, featured snippet,
  "people also ask", AI Overview including its page-token follow-up),
  direct-to-Google or via a third-party provider, behind one interface.
- **Deployment**: self-hosted. `docker compose -f deploy/docker-compose.yml up -d`
  runs the full stack (harvester + Redis + Prometheus + Grafana) on your own
  infrastructure. No Apify dependency.
- **Scaling**: Redis Streams distributed queue, horizontal worker scaling,
  per-proxy rate limiting, retry/backoff, Prometheus metrics.
- **Measured capacity**: see [Load testing](#load-testing) below for actual
  numbers (orchestration throughput and the latest soak test), not
  projections.
- **Production history**: none yet — this is a new build. See
  [CAPABILITIES.md](CAPABILITIES.md) for the direct, no-spin answer to that
  question and the other four this project needs to answer.
- **Limitations**: documented in full, not glossed over — see
  [Honest limitations](#honest-limitations).

Full document index: [CAPABILITIES.md](CAPABILITIES.md) (answers to the five
questions this was built for) · [PRODUCTION_READINESS.md](PRODUCTION_READINESS.md)
(architecture-to-operations checklist) · [RUNBOOK.md](RUNBOOK.md) (what to
do when a metric looks wrong) · [deploy/](deploy/) (Docker Compose / systemd).

## Why this exists

This is the engineering foundation for a Google SERP / AI Overviews
collection engagement at 10M+ requests/day: the queueing, concurrency,
proxy rotation, rate limiting, structured extraction, and observability a
production pipeline is built on. It is not itself a working large-scale
Google scraper today — see [Honest limitations](#honest-limitations) for
exactly what's built versus what real production volume against Google
still requires, and why that line is drawn deliberately rather than glossed
over.

## Quickstart

```bash
go test ./...
go run ./cmd/harvester -config configs/config.example.yaml -mode mock
```

This runs the full pipeline — queue, worker pool, proxy rotation, rate
limiter, fetch, parse, sink — against bundled fixture HTML, and prints
JSON-Lines results plus periodic throughput metrics to stdout:

```
[metrics] elapsed=2s success=5 failure=0 retried=0 dropped=0 completed=5 req/s=2.50 projected/day=216000
```

## Architecture

```
jobs (query+run_id+locale+device)
  │
  ▼
queue.Source ──▶ worker.Pool (N goroutines)
(Memory or           │
 Redis Streams)       ├─▶ proxy.Pool.Next()      (health-tracked rotation: round-robin, random, or weighted-by-success-rate)
                       ├─▶ ratelimit.Limiter.Wait (token bucket, keyed per proxy; honors provider Retry-After on 429)
                       ├─▶ fetcher.Fetcher.Fetch  (mock fixtures, live HTTP, or a third-party provider)
                       ├─▶ worker.Parser.Parse    (HTML or provider-JSON, structured extraction + drift detection)
                       └─▶ store.Sink.Write       (JSON-Lines, or PostgreSQL with JSONB columns)

metrics.Counters ──▶ periodic req/s + projected-daily-volume reporting, and
                      a /metrics + /healthz endpoint for Prometheus (-metrics-addr)
```

Every stage is an interface (`Fetcher`, `worker.Parser`, `Sink`, `queue.Source`)
or a small struct (`proxy.Pool`, `ratelimit.Limiter`) so each one is
independently swappable and independently testable. Note what this diagram
does *not* have: the API/scheduler layer (see "Job/API layer" and
"Scheduled harvesting" below, where they exist) only ever produces jobs onto
`queue.Source` — it never calls `Fetcher` or `Parser` itself. That boundary
is deliberate: it's what lets the acquisition backend change (direct HTTP vs.
provider) without the job-submission layer knowing or caring. Going from
here to a 10M+/day production system is a matter of:

| This repo today                                       | Production                                                        |
|---------------------------------------------------------|----------------------------------------------------------------------|
| `queue.MemorySource` or `queue.RedisStreamSource`        | Already the same interface Kafka/SQS would sit behind                |
| JSON-Lines sink, or PostgreSQL with JSONB columns        | Already real; add a warehouse-streaming `Sink` if that's preferred   |
| Proxy pool with health tracking + 3 rotation strategies  | Already real; point `proxy.Pool.Reload` at a vendor's proxy-list API |
| Plain `net/http` fetch, or a third-party provider fetch  | Headless rendering for JS-rendered content if going direct-to-Google, or just more provider budget if not |
| Single process, N goroutines                             | N processes across M hosts, all pointed at the same Redis stream (or Kafka/SQS) |
| Selectors tuned to fixture HTML                           | Selectors tuned to live Google markup, versioned and monitored for drift (see below) |
| `println` + Prometheus counters, no alerting              | Alerting rules on top of the same `/metrics` (see `deploy/prometheus/alerts.yml` where present) |

The reason this table exists instead of the repo pretending to already be
the production system: the remaining right-hand items depend on decisions
and resources (proxy budget, legal sign-off, target markets/locales) that
belong to the client, not to a codebase alone.

## Handling selector drift

Google's SERP markup is obfuscated and changes without notice. A scraper
that silently returns an empty result when its selectors stop matching is a
liability — it fails quietly, and nobody notices until a report downstream
is wrong. This pipeline treats "expected block not found" as a distinct
outcome from "found and empty":

```go
if len(result.Organic) == 0 {
    missing = append(missing, "organic_results")
}
if len(missing) > 0 {
    result.Calibration = &model.CalibrationNote{MissingBlocks: missing}
}
```

See `internal/parser/testdata/uncalibrated.html` and the corresponding test
for a worked example: a page whose organic-results wrapper class changed
still parses without error, but comes back flagged for review instead of
silently empty. In production, calibration-flagged results route to a
review queue (and would typically be paired with archiving the raw HTML for
whoever is retuning selectors that week) instead of being written to the
same sink as trusted results.

## Distributed queue (Redis Streams)

`internal/queue.RedisStreamSource` consumes jobs from a Redis Stream via a
consumer group, so any number of harvester processes — on one host or
spread across many — share one durable queue instead of each needing its
own list of queries. This is the concrete mechanism behind "10M/day is many
processes across many hosts": they all point at the same stream and group,
and Redis divides the work.

```yaml
queue_backend: redis
redis_addr: "localhost:6379"
redis_stream: "serp-harvester:queries"
redis_group: "harvesters"
redis_consumer: "worker-1"   # unique per process
redis_seed_queue: true        # local demo only: push `queries` before consuming
```

Run two consumers against the same stream (different `redis_consumer`
values, or the same via `docker-compose up redis` for local testing) and
Redis hands each message to exactly one of them, re-delivering to another
consumer if one dies mid-job without acknowledging — see
`internal/queue/redis_stream_test.go` (run against an in-memory `miniredis`,
no real Redis server needed for `go test`).

## Proxy management

`internal/proxy.Pool` tracks per-proxy health (success/failure counts,
consecutive-failure cooldown) and supports three rotation strategies:

```yaml
proxy_strategy: round_robin   # default: predictable, fair
# proxy_strategy: random               # avoids synchronized patterns across many processes
# proxy_strategy: weighted_success_rate # biases toward proxies with a better observed success rate
```

`Pool.Reload(urls)` swaps the proxy list at runtime — the seam for pointing
at a vendor's proxy-list API or a file watcher instead of a static config
list — and preserves accumulated health stats for any URL that stays in the
list. `Pool.AllStats()` returns a per-proxy snapshot (success/failure
counts, ban state, last-used time) for an operator dashboard or a health
endpoint. Authenticated proxies (`http://user:pass@host:port`) work with no
special handling — Go's `net/http` applies the Basic auth automatically, for
both plain HTTP proxying and HTTPS `CONNECT` tunneling, confirmed by
`TestHTTPFetcher_AuthenticatedProxy` against a fake proxy server that
verifies the header actually arrives. See `internal/proxy/pool_test.go`
(9 tests, including a real bug this test suite caught: `Reload` originally
lost health stats for retained proxies due to a map that was only populated
on the empty-list code path).

## Persistent storage (PostgreSQL)

JSON-Lines (`store.JSONLSink`) is fine for the pipeline itself; for a client
who wants to query results — by run, by query, by date range, by whether AI
Overview was present — `store.PostgresSink` stores each result as a row with
real columns (`run_id`, `query`, `locale`, `device`, `fetched_at`,
`latency_ms`, `proxy_used`) plus the parsed SERP fields as native JSONB
(`organic`, `featured_snippet`, `ai_overview`, `people_also_ask`,
`calibration`), queryable with Postgres's own JSON operators instead of
being an opaque blob:

```yaml
sink_backend: postgres
postgres_dsn_env: POSTGRES_DSN   # read from the environment, never from this file
```

```bash
export POSTGRES_DSN="postgres://user:pass@host:5432/dbname?sslmode=disable"
go run ./cmd/harvester -config your-config.yaml
```

Schema (`CREATE TABLE IF NOT EXISTS`) is applied automatically on startup —
safe to run on every deploy, no separate migration tool for this scope. When
a result is calibration-flagged (selector drift), the raw pre-parse response
body is stored in `raw_response` for whoever is retuning selectors —
otherwise that column stays NULL, keeping normal rows small.

**Tested against a real Postgres, not just mocked:** `internal/store/postgres_test.go`
runs against an actual PostgreSQL instance (gated behind `POSTGRES_TEST_DSN`,
skipped in normal CI the same way `tests/live` is) — write-then-read-back of
every JSONB column, schema idempotency across repeated startups, and a
fast-fail on an invalid DSN. Beyond the automated tests, the full CLI was
run end-to-end against a real local Postgres container (`mode: mock`,
`sink_backend: postgres`) and the resulting rows were queried directly:
organic results, AI Overview presence, and calibration+raw-body capture all
landed correctly.

## Third-party provider fetcher

`internal/fetcher.ProviderFetcher` calls a SerpApi-compatible JSON API
(`GET {base_url}?engine=google&q=...&api_key=...`) instead of requesting
Google directly, and `internal/parser.JSONParser` maps that provider's
structured JSON onto the same `model.SerpResult` the HTML parser produces —
so the queue, worker pool, retries, proxy rotation, metrics, and sink are
identical regardless of which fetch path is active.

```yaml
mode: provider
provider_base_url: "https://serpapi.com/search"
provider_api_key_env: "SERPAPI_KEY"   # read from the environment, never from this file
provider_engine: "google"
```

```bash
export SERPAPI_KEY=...
go run ./cmd/harvester -config your-config.yaml -mode provider
```

This is the honest answer to "handle CAPTCHAs and blocks": a licensed SERP
data provider has already solved consent walls, CAPTCHAs, and proxy scale
as their product. Plugging into one means the pipeline being delivered here
(queue, worker pool, retries, parsing, metrics, ops) is real and owned by
this codebase, while the legally risky edge — automated querying of Google
directly — is a paid vendor's product, not code written here to defeat it.

**AI Overview's documented two-step fetch is handled.** SerpApi documents
that AI Overview content sometimes comes back as only a `page_token` (valid
~4 minutes) rather than inline `text_blocks`, requiring a follow-up request
with `engine=google_ai_overview&page_token=...`. `ProviderFetcher.Fetch`
detects this automatically, makes the follow-up request, and merges the
resolved content back in before `JSONParser` ever sees the body — so the
two-step flow is invisible to everything downstream. If the follow-up fails
(expired token, network error) the original body is still returned
unmodified: organic results and everything else still parse, and that
query's result just comes back with no AI Overview, the same as if the SERP
genuinely had none. See `TestProviderFetcher_ResolvesAIOverviewPageToken`,
`_InlineAIOverviewSkipsFollowUp`, and `_AIOverviewFollowUpFailureIsNonFatal`
in `internal/fetcher/provider_test.go` — verified against a synthetic
two-request fixture, per the same no-real-key caveat below.

**Rate limits are handled per the provider's own signal, not guessed.** A
429 response is parsed into a `*fetcher.RateLimitError` carrying whatever
`Retry-After` the provider sent (seconds or HTTP-date form, per RFC 9110),
and `worker.Pool` waits out that exact duration before retrying instead of
applying its generic exponential backoff —
`TestPool_HonorsRateLimitRetryAfter` confirms this by timing an actual
retry cycle. Three more tests
(`TestProviderFetcher_RateLimitWithRetryAfterSeconds` /
`_HTTPDate` / `_WithoutRetryAfter`) cover both `Retry-After` formats and the
case where none is given.

**No API key yet? Nothing else in this repo needs one.** Mock mode requires
none, `go test ./...` never touches the network, and
`internal/fetcher/provider_test.go` / `internal/parser/json_provider_test.go`
verify the full request/response and JSON-mapping cycle against a local
`httptest` server and a realistic fixture, so this path is fully tested
without a real key. What hasn't been verified yet, precisely because no key
exists to test with, is a real provider's actual response matching this
schema exactly — copy `.env.example` to `.env`, set whichever variable name
`provider_api_key_env` points at once a trial or paid key exists, and it
works with zero code changes. Worth a quick real-key smoke test before
relying on this path for a client, since a vendor's exact field names are
usually close but not guaranteed identical to what `json_provider.go`
currently expects.

## Observability (Prometheus metrics)

```yaml
metrics_addr: ":9090"
```

```bash
curl localhost:9090/metrics
# serp_harvester_success_total 42
# serp_harvester_failure_total 3
# serp_harvester_dropped_total 0
# serp_harvester_retried_total 5
```

`internal/metrics.PrometheusCollector` reads the same atomic counters the
`println` reporter uses, on every scrape — so a client's existing
Prometheus/Grafana stack can alert on failure rate, dropped-job rate, and
throughput without this repo needing to know anything about their
dashboards. See `internal/metrics/prometheus_test.go`.

## Load testing

`cmd/loadtest` measures the pipeline's own orchestration overhead —
queue → worker pool → proxy pool → rate limiter → mock fetch → parse →
discard sink — at high concurrency, entirely offline. It answers one
specific question: does this codebase's own plumbing bottleneck before a
proxy budget or target site would? It does **not** measure real network
throughput against Google or any other live target. Three modes:

```bash
go run ./cmd/loadtest -n 100000 -concurrency 800            # fixed-count benchmark
go run ./cmd/loadtest -sweep 1,4,8,16,50,200,800 -n 3000     # concurrency sweep, one table
go run ./cmd/loadtest -duration 90m -sample-interval 2m      # soak test
```

### Concurrency sweep

Real numbers measured on the development machine this was built on (mock
fetcher, simulated 20-80ms latency per fetch, zero network calls). The
first three rows are from `-sweep`; the last three are earlier fixed-count
runs from before per-request latency tracking existed, kept here rather
than re-run purely for a prettier table — the `req/s` and `0 drops` figures
are directly comparable either way, only the latency columns differ:

| Concurrency | Queries | Wall time | Throughput | p50 | p95 | p99 | Failures/Drops |
|-------------|---------|-----------|------------|-----|-----|-----|----------------|
| 1           | 2,000   | 1m39.7s   | ~20 req/s  | 49ms | 78ms | 80ms | 0 / 0 |
| 4           | 2,000   | 25.9s     | ~77 req/s  | 52ms | 78ms | 90ms | 0 / 0 |
| 8           | 2,000   | 12.6s     | ~159 req/s | 50ms | 78ms | 80ms | 0 / 0 |
| 50          | 20,000  | 20.2s     | ~990 req/s | n/a (pre-latency-tracking run) | | | 0 / 0 |
| 200         | 50,000  | 12.7s     | ~3,945 req/s | n/a | | | 0 / 0 |
| 800         | 100,000 | 6.5s      | ~15,290 req/s | n/a | | | 0 / 0 |

Throughput scales roughly linearly with concurrency here because the
bottleneck is the mock fetcher's simulated per-request latency, not
pipeline overhead — exactly the property you want: the orchestration layer
gets out of the way, and real-world throughput becomes a function of proxy
count and target-site latency, not this codebase. Note concurrency=8
(~159 req/s) already clears the ~116 req/s that 10M/day works out to, in
this orchestration-only benchmark.

### Soak test

A sustained run tracks throughput, latency percentiles, memory, and
goroutine count over time via `-duration`, so stability can be checked
directly rather than assumed from a short benchmark:

```text
[soak 10s] success=19486 failure=0 dropped=0 interval_rps=1949 p50=51ms p95=77ms p99=81ms alloc=2MB goroutines=103
[soak 20s] success=39019 failure=0 dropped=0 interval_rps=1953 p50=50ms p95=78ms p99=81ms alloc=2MB goroutines=103
[soak 30s] success=58691 failure=97 dropped=0 interval_rps=1967 p50=51ms p95=77ms p99=80ms alloc=3MB goroutines=100
```

(30-second smoke test shown above, concurrency=100 — stable throughput,
stable memory, stable goroutine count across samples; the 97 failures at
the 30s mark are jobs in flight exactly at the shutdown cutoff hitting
context cancellation, not a fetch failure — see `PRODUCTION_READINESS.md`
§5 for the accounting fix this run surfaced.)

**Full 90-minute run, completed:**

```text
actual elapsed: 1h30m0s (requested: 1h30m0s)
total success=20756754 failure=186 dropped=0 retried=186
ai_overview=6918916 calibration_flagged=6918920 proxy_banned=0
latency (last 200000 samples): min=20ms p50=50ms p95=77ms p99=79ms max=200ms
average throughput: 3844 req/s => 332098180/day if sustained
final memory: alloc=5MB sys=61MB goroutines=1
```

Concurrency 200, sampled every 2 minutes for the full run: throughput held
at 3,700-4,000 req/s the entire 90 minutes with no degradation, `alloc`
oscillated between 3-7MB the whole time with no upward trend (no memory
leak), and goroutine count stayed exactly at 203 across all 45 samples
before winding down to 1 at shutdown (no goroutine leak). `dropped=0` across
20.76 million requests processed — the 186 `failure`/`retried` count is
first-attempt failures exactly at the process's hard 90-minute cutoff that
were then retried successfully, not lost work; nothing was dropped.
`ai_overview` and `calibration_flagged` landing within 4 of each other
(6,918,916 vs. 6,918,920) is the expected internal consistency check given
the 3-fixture rotation the mock fetcher cycles through.

This is still orchestration-only evidence (mock fetcher, zero network), not
a live-throughput claim — but it's real, sustained, and reproducible:
`go run ./cmd/loadtest -duration 90m -sample-interval 2m -concurrency 200`.

## Live mode

```bash
go run ./cmd/harvester -config configs/config.example.yaml -mode live -i-have-reviewed-tos
```

Live mode issues real HTTP requests directly to the configured endpoint
(default: Google Search) via a plain `net/http` client. It deliberately:

- requires an explicit `-i-have-reviewed-tos` flag (the tool refuses to run
  otherwise, and prints why),
- defaults to a conservative rate (`rate_per_proxy_rps: 1` in the example
  config),
- carries a cookie jar so a consent decision persists across requests within
  a run (`internal/fetcher/http.go`), the same way a browser remembers you
  clicked "I agree",
- does **not** implement headless rendering, CAPTCHA solving, or browser
  fingerprint spoofing.

That last point is a deliberate scope boundary, not an oversight — see
[Honest limitations](#honest-limitations) below, and use
[the provider fetcher](#third-party-provider-fetcher) instead for anything
beyond light, occasional direct queries.

**Tested result:** even with the cookie jar and a pre-seeded consent cookie,
live requests against `https://www.google.com/search` from this environment
never reached a results page — confirmed by inspecting the raw response
each time. Two different outcomes were observed across separate attempts,
neither of which is real search results:

1. The "Before you continue to Google Search" consent interstitial (status
   200, page title matches, no results markup).
2. A JavaScript-execution check (`/httpservice/retry/enablejs`, "click here
   if you are not redirected") — no consent form present at all, no cookie
   or header fixes this one, since it requires an actual JS engine to
   execute and follow through, which is browser-automation territory, not a
   fetch-layer change.

Persisting whatever cookies Google's own responses set (`SEARCH_SAMESITE`,
`AEC`, `__Secure-ENID`) across repeated requests — ordinary cookie-jar
behavior, not fabricating anything — did not change either outcome. That's
reported here rather than glossed over: this isn't a single small gap with
one fix, it's Google's layered defenses doing what they're designed to do,
and getting a plain, undisguised HTTP client past them reliably is already
nontrivial before CAPTCHAs, rate limiting, or volume enter the picture at
all.

## Honest limitations

Overselling readiness here would be a worse outcome than being precise about
where the real work is — especially once this is running against a real
client's volume, not just a pitch:

1. **Parser selectors target the bundled fixtures, not live Google markup.**
   Google's actual DOM is obfuscated and changes on its own schedule.
   Retargeting selectors to current production markup — and keeping them
   correct as Google ships layout changes — is real, ongoing work. The
   fixtures here (`internal/parser/testdata/*.html`) are deliberately
   generic, clearly-labeled synthetic HTML, not captured Google pages —
   redistributing real scraped Google markup in this repo isn't something
   it does. What's demonstrated instead is the extraction *architecture*:
   structured output, optional-block handling, and drift detection, which
   the real selectors plug into unchanged. Using the
   [provider fetcher](#third-party-provider-fetcher) instead sidesteps this
   entirely, since the provider returns already-structured JSON.

2. **AI Overview content is frequently rendered client-side.** A
   request/response HTTP fetch (what `HTTPFetcher` does) won't see content
   that Google's frontend renders via JS after the initial page load in
   some surfaces. A production system going direct-to-Google needs headless
   browser rendering (e.g. Playwright) for those cases — intentionally not
   bundled here so this repo doesn't ship a Google-specific
   rendering/evasion toolkit without a client and a scope behind it. The
   provider fetcher sidesteps this too: providers that support AI Overview
   extraction return it as structured JSON already (see
   `internal/parser/json_provider.go`).

3. **No CAPTCHA handling or anti-detection measures are implemented** for
   the direct-to-Google live mode. At real volume, direct-to-Google request
   patterns get challenged. Solving that (compliant CAPTCHA-handling,
   residential proxy rotation, request fingerprint normalization) is scoped,
   budgeted, engagement-specific work if going direct — the
   [third-party provider fetcher](#third-party-provider-fetcher) is the
   honest way this actually gets closed for a real client instead.

4. **Google's Terms of Service restrict automated querying.** Whether and
   how to operate at volume against Google directly (vs. via a licensed
   data provider) is a legal/business decision for the client, made with
   their counsel — this repo surfaces that decision point (the
   `-i-have-reviewed-tos` flag for live mode) rather than deciding it for
   you.

## Scaling to 10M+ requests/day

10M requests/day is ~116 sustained requests/second. Nothing about that
number requires a different architecture from what's here — it requires:

- enough proxy/egress capacity (direct mode) or provider request quota
  (provider mode) that the sustained rate reaches ~116 req/s,
- enough worker concurrency per process, and enough `RedisStreamSource`
  consumer processes across enough hosts once a single process's ceiling is
  reached — the load-test numbers above show this codebase's own
  orchestration sustaining well past that number in isolation,
- a queue and sink that can sustain that throughput (Redis Streams is
  already wired in; Kafka/SQS are the same `queue.Source`/`store.Sink`
  interface if preferred),
- for direct-to-Google mode specifically: the live-mode gaps above
  (rendering, CAPTCHA handling, selector maintenance) closed for the
  client's actual target markup and locales — or, more realistically at
  this volume, the provider fetcher instead.

## Layout

```
cmd/harvester/          CLI entrypoint and wiring
cmd/loadtest/           Offline orchestration measurement: fixed-count, concurrency sweep, and soak-test modes
internal/model/         SerpResult and its sub-structures (incl. run_id/locale/device, raw body on drift)
internal/queue/         Job sources: in-memory, and Redis Streams for multi-process/host scaling
internal/proxy/         Proxy pool: health tracking, dynamic reload, 3 rotation strategies
internal/ratelimit/     Per-key token-bucket rate limiter
internal/fetcher/       Fetcher interface + Mock, direct HTTP, and third-party provider implementations,
                        plus RateLimitError/Retry-After parsing (errors.go)
internal/parser/        HTML and provider-JSON → SerpResult extraction, with drift detection
internal/worker/        The pool tying fetch → parse → sink together with retry/backoff
internal/metrics/       Run counters, latency percentile tracking, periodic reporting,
                        and a Prometheus /metrics + /healthz endpoint
internal/store/         Sink interface + JSON-Lines and PostgreSQL implementations
internal/config/        YAML config loading
configs/                Example config
deploy/                 Docker Compose (harvester+Redis+Prometheus+Grafana), systemd unit, Dockerfile
tests/live/             Real-network integration tests, gated behind HARVESTER_LIVE=true (never run in CI)
```

## Contact

Built by Henry Morgan Dibie. Open to discussing how this maps onto a real
collection engagement, including the production gaps called out above.
