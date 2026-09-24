# Operations Runbook

Every check below reads a metric this codebase actually exposes
(`internal/metrics`, scraped at `/metrics`) — nothing here references a
signal that doesn't exist yet.

## Success rate drops

Symptom: `rate(serp_harvester_failure_total[5m])` climbing, or
`rate(serp_harvester_success_total[5m])` falling.

1. Check `serp_harvester_proxy_banned_total` — a spike means proxies are
   getting banned faster than they recover. Either the ban threshold
   (`proxy_ban_fails`) is too aggressive for current conditions, or the
   proxy pool itself is degraded (check with the proxy provider).
2. Check `serp_harvester_latency_p95_ms` / `p99_ms` — rising latency ahead
   of the failure spike usually means the target (or provider) is slow to
   respond, not rejecting requests outright; consider whether
   `request_timeout` needs raising or whether this is early warning of a
   block.
3. In `mode: live`, a failure spike with unchanged latency often means a
   response shape changed (a new interstitial, a different block page) —
   pull a raw sample manually and compare against what
   `internal/fetcher/http.go` and the [README's live-mode findings](README.md#live-mode)
   already documented, to see if this is a known pattern or a new one.
4. In `mode: provider`, check the provider's own status page and your
   account's rate limit / quota before assuming this codebase is at fault.
5. If proxies are the cause: rotate in a fresh batch, or temporarily reduce
   `rate_per_proxy_rps` so existing proxies recover faster than they get
   banned.

## Parser drift (`serp_harvester_parser_drift_total` climbing)

This means successful fetches are coming back with
`Calibration.MissingBlocks` set — the target's markup no longer matches the
selectors in `internal/parser/parser.go` (HTML mode) or the JSON shape
`internal/parser/json_provider.go` expects (provider mode).

1. Confirm it's not a one-off: check the rate, not a single occurrence.
2. Capture a raw response body from a currently-failing query (if using
   `mode: provider`, this is one API call away; if `mode: live`, be mindful
   of triggering more of the same anti-automation response documented in
   [Live mode](README.md#live-mode) while investigating).
3. Compare the captured markup/JSON against the current selectors/struct
   tags. Update `internal/parser/parser.go` or
   `internal/parser/json_provider.go` accordingly, add a fixture reproducing
   the new shape under `internal/parser/testdata/`, and add a test alongside
   the existing ones in `parser_test.go` / `json_provider_test.go` before
   shipping the fix.
4. Until fixed, calibration-flagged results should not be treated as
   trustworthy data downstream — this is exactly what the flag is for (see
   README "Handling selector drift").

## Queue backlog growing (Redis Streams mode)

Symptom: producers pushing faster than consumers drain, or consumers
falling behind.

1. Check `serp_harvester_success_total` growth rate against how fast
   `XADD` calls are happening on the producer side — if consumption rate is
   flat while production keeps rising, this is a capacity problem, not a
   bug.
2. Check `redis-cli XPENDING <stream> <group>` for a consumer that's stuck
   holding claimed-but-unacknowledged entries (a crashed worker that never
   called `XAck`). Use `XCLAIM` to hand those back to a healthy consumer, or
   restart the stuck one.
3. Scale out: run more harvester replicas against the same stream and group
   with unique `redis_consumer` values (see `deploy/README.md`).
4. If Redis itself is the bottleneck (check its own CPU/memory), that's an
   infrastructure sizing problem, not something the queue abstraction can
   fix — this is exactly the point where `queue.Source` could be pointed at
   Kafka/SQS instead, without changing anything else in the pipeline.

## AI Overview hit rate is unexpectedly zero

1. In `mode: provider`: check whether the provider's response actually
   contains `ai_overview` at all for this query mix — not every query
   triggers an AI Overview on Google's side, so zero isn't automatically a
   bug. If it's present with only a `page_token`, confirm the follow-up
   request is succeeding (`internal/fetcher/provider.go`'s
   `resolveAIOverview`); a failing follow-up degrades gracefully to "no AI
   Overview" rather than erroring, so check provider logs/latency for the
   `engine=google_ai_overview` calls specifically.
2. In `mode: live`: AI Overview content is frequently JS-rendered and won't
   appear in a plain HTTP fetch at all — see "Honest limitations" in the
   README. This isn't a runbook fix; it's the documented scope boundary.

## Metrics endpoint itself is unreachable

1. Confirm `metrics_addr` is set in the running config — it's empty (no
   server started) by default.
2. `curl localhost:<port>/healthz` — if this also fails, the process itself
   may be down; check `systemctl status serp-harvester` or
   `docker compose logs harvester`.
3. If `/healthz` succeeds but `/metrics` doesn't, that's a Prometheus
   client library issue, not a pipeline issue — check for a panic in the
   collector logs (shouldn't happen; `Collect` only reads atomic counters
   and a mutex-guarded slice, but worth ruling out first).

## Known gaps this runbook can't cover yet

- No automated alerting is wired up in this repo — the Grafana dashboard in
  `deploy/` is for humans watching it, not paging anyone. Wiring Prometheus
  alerting rules on top of these metrics is straightforward but not done
  here.
- No automated proxy-pool refresh/rotation-from-provider integration exists
  — the proxy list in config is static. A real deployment would source it
  from whichever proxy vendor is chosen.
