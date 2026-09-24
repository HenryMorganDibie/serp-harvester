# serp-harvester

A reference implementation of the engineering backbone behind a large-scale
Google SERP collection pipeline: concurrent worker pool, per-proxy rate
limiting, proxy health/rotation, retry with backoff, structured extraction
(organic results, featured snippets, "people also ask", AI Overviews), drift
detection when a page's layout no longer matches expected selectors, and
throughput metrics reported in the same terms a request-volume SLA is
measured in.

It runs end to end, offline, with `go run ./cmd/harvester` — no API keys, no
proxies, no network access required.

## Why this exists

This is a portfolio sample, built to demonstrate architecture and code
quality for a Google SERP / AI Overviews collection engagement at 10M+
requests/day. It is not itself a production scraper pointed at live Google
traffic — see [Honest limitations](#honest-limitations) for exactly where
the line is drawn and why.

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
queries ──▶ queue.New ──▶ worker.Pool (N goroutines)
                              │
                              ├─▶ proxy.Pool.Next()      (rotation + cooldown on repeated failure)
                              ├─▶ ratelimit.Limiter.Wait (token bucket, keyed per proxy)
                              ├─▶ fetcher.Fetcher.Fetch  (mock fixtures, or real HTTP in live mode)
                              ├─▶ parser.Parser.Parse    (structured extraction + drift detection)
                              └─▶ store.Sink.Write       (JSON-Lines; swap for Kafka/warehouse)
                              
metrics.Counters ──▶ periodic req/s and projected-daily-volume reporting
```

Every stage is an interface (`Fetcher`, `Sink`) or a small struct
(`proxy.Pool`, `ratelimit.Limiter`) so each one is independently swappable
and independently testable. Going from this sample to a 10M+/day production
system is a matter of:

| Sample (this repo)                          | Production                                                        |
|----------------------------------------------|--------------------------------------------------------------------|
| In-memory channel queue (`internal/queue`)    | Kafka / SQS / Redis Streams — same consumer interface              |
| JSON-Lines file/stdout sink                   | Bulk warehouse writer or streaming sink implementing `store.Sink`  |
| A handful of hardcoded proxy URLs             | A managed residential/datacenter proxy pool, hot-reloaded          |
| Plain `net/http` fetch                        | Headless rendering (for JS-rendered AI Overview content) + a compliant CAPTCHA-handling layer, scoped with the client |
| Single process, N goroutines                  | N processes across M hosts, each running this same worker pool     |
| Selectors tuned to fixture HTML               | Selectors tuned to live Google markup, versioned and monitored for drift (see below) |

The reason this table exists instead of the repo pretending to already be
the production system: those right-hand items depend on decisions and
resources (proxy budget, legal sign-off, target markets/locales) that belong
to the client, not to a public sample repo.

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

## Live mode

```bash
go run ./cmd/harvester -config configs/config.example.yaml -mode live -i-have-reviewed-tos
```

Live mode swaps the mock fixture fetcher for a real `net/http` client and
issues actual requests. It deliberately:

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
below.

**Tested result:** even with the cookie jar and a pre-seeded consent cookie,
a live request against `https://www.google.com/search` from this
environment still came back as the "Before you continue to Google Search"
consent interstitial, not a results page — confirmed by inspecting the raw
response (status 200, page title matches the interstitial, no results
markup present). That's reported here rather than glossed over: getting a
plain, undisguised HTTP client past Google's consent/session flow reliably
is already nontrivial, before CAPTCHAs, rate limiting, or volume enter the
picture at all. It's honest evidence for exactly the scoping conversation in
[Honest limitations](#honest-limitations) below.

## Honest limitations

A sample repo that oversold its readiness would be a worse pitch than one
that's precise about where the real work is:

1. **Parser selectors target the bundled fixtures, not live Google markup.**
   Google's actual DOM is obfuscated and changes on its own schedule.
   Retargeting selectors to current production markup — and keeping them
   correct as Google ships layout changes — is real, ongoing work. The
   fixtures here (`internal/parser/testdata/*.html`) are deliberately
   generic, clearly-labeled synthetic HTML, not captured Google pages —
   redistributing real scraped Google markup in a public repo isn't
   something this sample does. What's demonstrated instead is the
   extraction *architecture*: structured output, optional-block handling,
   and drift detection, which the real selectors plug into unchanged.

2. **AI Overview content is frequently rendered client-side.** A
   request/response HTTP fetch (what `HTTPFetcher` does) won't see content
   that Google's frontend renders via JS after the initial page load in
   some surfaces. A production system needs headless browser rendering
   (e.g. Playwright) for those cases — intentionally not bundled here so
   this repo doesn't ship a Google-specific rendering/evasion toolkit
   without a client and a scope behind it.

3. **No CAPTCHA handling or anti-detection measures are implemented.** At
   real volume, direct-to-Google request patterns get challenged. Solving
   that (compliant CAPTCHA-handling, residential proxy rotation, request
   fingerprint normalization) is scoped, budgeted, engagement-specific work
   — not something to bake into a public sample.

4. **Google's Terms of Service restrict automated querying.** Whether and
   how to operate at volume against Google directly (vs. via a licensed
   data provider) is a legal/business decision for the client, made with
   their counsel — this repo surfaces that decision point (the
   `-i-have-reviewed-tos` flag) rather than deciding it for you.

## Scaling to 10M+ requests/day

10M requests/day is ~116 sustained requests/second. Nothing about that
number requires a different architecture from what's here — it requires:

- enough proxy/egress capacity that `rate_per_proxy_rps × len(proxies) ≥ 116`,
- enough worker concurrency (and, past a single host's ceiling, enough
  horizontally-scaled instances of this same worker pool) to keep that
  aggregate rate saturated,
- a queue and sink that can sustain that throughput (Kafka/SQS in, a
  streaming warehouse writer out — both drop-in behind the `Fetcher`/`Sink`
  interfaces already in this codebase),
- the live-mode gaps above (rendering, CAPTCHA handling, selector
  maintenance) closed for the client's actual target markup and locales.

## Layout

```
cmd/harvester/          CLI entrypoint and wiring
internal/model/         SerpResult and its sub-structures
internal/queue/         Job queue (channel-based; swap for Kafka/SQS in prod)
internal/proxy/         Proxy rotation pool with failure-based cooldown
internal/ratelimit/     Per-key token-bucket rate limiter
internal/fetcher/       Fetcher interface + Mock (offline) and HTTP (live) implementations
internal/parser/        HTML → SerpResult extraction, with drift detection
internal/worker/        The pool tying fetch → parse → sink together with retry/backoff
internal/metrics/       Run counters and periodic throughput reporting
internal/store/         Result sink interface + JSON-Lines implementation
internal/config/        YAML config loading
configs/                Example config
```

## Contact

Built by Henry Morgan Dibie. Open to discussing how this maps onto a real
collection engagement, including the production gaps called out above.
