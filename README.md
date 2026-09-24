# serp-harvester

The engineering backbone for a large-scale Google SERP collection pipeline:
concurrent worker pool, per-proxy rate limiting, proxy health/rotation,
retry with backoff, a distributed queue (Redis Streams) for scaling across
processes and hosts, structured extraction (organic results, featured
snippets, "people also ask", AI Overviews), drift detection when a page's
layout no longer matches expected selectors, a third-party SERP-provider
fetch path, and Prometheus metrics for the same throughput numbers a
request-volume SLA is measured in.

It runs end to end, offline, with `go run ./cmd/harvester` — no API keys, no
proxies, no network access required.

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
queries ──▶ queue.Source ──▶ worker.Pool (N goroutines)
 (Memory or                      │
  Redis Streams)                 ├─▶ proxy.Pool.Next()      (rotation + cooldown on repeated failure)
                                  ├─▶ ratelimit.Limiter.Wait (token bucket, keyed per proxy)
                                  ├─▶ fetcher.Fetcher.Fetch  (mock fixtures, live HTTP, or a third-party provider)
                                  ├─▶ worker.Parser.Parse    (HTML or provider-JSON, structured extraction + drift detection)
                                  └─▶ store.Sink.Write       (JSON-Lines; swap for Kafka/warehouse)

metrics.Counters ──▶ periodic req/s + projected-daily-volume reporting, and
                      a /metrics endpoint for Prometheus (-metrics-addr)
```

Every stage is an interface (`Fetcher`, `worker.Parser`, `Sink`, `queue.Source`)
or a small struct (`proxy.Pool`, `ratelimit.Limiter`) so each one is
independently swappable and independently testable. Going from here to a
10M+/day production system is a matter of:

| This repo today                                     | Production                                                        |
|-------------------------------------------------------|--------------------------------------------------------------------|
| `queue.MemorySource` or `queue.RedisStreamSource`      | Already the same interface Kafka/SQS would sit behind              |
| JSON-Lines file/stdout sink                            | Bulk warehouse writer or streaming sink implementing `store.Sink`  |
| A handful of hardcoded proxy URLs                      | A managed residential/datacenter proxy pool, hot-reloaded          |
| Plain `net/http` fetch, or a third-party provider fetch| Headless rendering for JS-rendered content if going direct-to-Google, or just more provider budget if not |
| Single process, N goroutines                           | N processes across M hosts, all pointed at the same Redis stream (or Kafka/SQS) |
| Selectors tuned to fixture HTML                        | Selectors tuned to live Google markup, versioned and monitored for drift (see below) |
| `println`-based throughput reporting                   | Already solved: `/metrics` scrapes into the client's existing Prometheus/Grafana stack |

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
`internal/fetcher/provider_test.go` exercises the full request/response
cycle against a local `httptest` server, so this path is verified without
needing a real API key.

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
throughput against Google or any other live target.

```bash
go run ./cmd/loadtest -n 100000 -concurrency 800
```

Real numbers measured on the development machine this was built on
(mock fetcher, simulated 20-80ms latency per fetch, zero network calls):

| Concurrency | Queries | Wall time | Throughput | Failures/Drops |
|-------------|---------|-----------|------------|----------------|
| 50          | 20,000  | 20.2s     | ~990 req/s | 0 / 0          |
| 200         | 50,000  | 12.7s     | ~3,945 req/s | 0 / 0        |
| 800         | 100,000 | 6.5s      | ~15,290 req/s | 0 / 0       |

Throughput scales roughly linearly with concurrency here because the
bottleneck is the mock fetcher's simulated per-request latency, not
pipeline overhead — exactly the property you want: the orchestration layer
gets out of the way, and real-world throughput becomes a function of proxy
count and target-site latency, not this codebase.

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
a live request against `https://www.google.com/search` from this
environment still came back as the "Before you continue to Google Search"
consent interstitial, not a results page — confirmed by inspecting the raw
response (status 200, page title matches the interstitial, no results
markup present). That's reported here rather than glossed over: getting a
plain, undisguised HTTP client past Google's consent/session flow reliably
is already nontrivial, before CAPTCHAs, rate limiting, or volume enter the
picture at all.

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
cmd/loadtest/           Offline orchestration-throughput measurement tool
internal/model/         SerpResult and its sub-structures
internal/queue/         Job sources: in-memory, and Redis Streams for multi-process/host scaling
internal/proxy/         Proxy rotation pool with failure-based cooldown
internal/ratelimit/     Per-key token-bucket rate limiter
internal/fetcher/       Fetcher interface + Mock, direct HTTP, and third-party provider implementations
internal/parser/        HTML and provider-JSON → SerpResult extraction, with drift detection
internal/worker/        The pool tying fetch → parse → sink together with retry/backoff
internal/metrics/       Run counters, periodic throughput reporting, and a Prometheus /metrics endpoint
internal/store/         Result sink interface + JSON-Lines implementation
internal/config/        YAML config loading
configs/                Example config
```

## Contact

Built by Henry Morgan Dibie. Open to discussing how this maps onto a real
collection engagement, including the production gaps called out above.
