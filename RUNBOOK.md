# Operations Runbook

Every check below reads a signal this codebase actually exposes: the
harvester's metrics (`internal/metrics`, scraped at `/metrics`), the job
API's HTTP endpoints, process logs, the Redis stream, or the Postgres
results table. Nothing here references a signal that doesn't exist yet.

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
3. In `mode: playwright`: the page is rendered, so a zero rate there points
   at the parser. Its AI Overview selectors target the bundled fixtures,
   not live Google markup; check `serp_harvester_parser_drift_total` and
   the archived raw bodies of drift-flagged results.

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

## Job API health (POST /jobs, GET /jobs/{run_id})

`cmd/api` exposes no Prometheus metrics of its own; every check here goes
through its HTTP endpoints, its logs, or the Redis stream it writes to.
Defaults below are the ones shipped in `deploy/` (listen address `:8080`,
stream `serp-harvester:queries`).

1. Verify the API process is alive:
   - `curl localhost:8080/healthz` should return `200` with body `ok`. If it
     fails, check `systemctl status serp-harvester-api` or
     `docker compose logs api`.
   - A process that keeps restarting is almost always a fatal startup
     error: an unreachable Postgres when `POSTGRES_DSN` is set, or an
     invalid schedule file (see "Scheduled harvests not firing"). The last
     log line before each exit names the cause.
2. Test job submission:
   - The request below should return `202 Accepted` with `run_id` and
     `queries_accepted`:

     ```bash
     curl -X POST localhost:8080/jobs -H "Content-Type: application/json" \
       -d '{"queries":["example query"]}'
     ```

   - `400` means a malformed request. `502` means the push to Redis failed;
     check that `-redis-addr` is reachable from the API.
3. Confirm jobs are actually reaching the queue:
   - `redis-cli XLEN serp-harvester:queries` (or `XINFO STREAM`) should grow
     by `queries_accepted` after each submission.
   - Do not use `serp_harvester_success_total` for this. That metric comes
     from the harvester (the consumer); it shows work finishing, not the API
     enqueuing it.
4. Track job progress with `curl localhost:8080/jobs/<run_id>`, which
   returns `submitted`, `submitted_known`, `completed` and
   `completed_known`:
   - `completed: -1` with `completed_known: false` has exactly one cause:
     the environment variable named by `-postgres-dsn-env` (default
     `POSTGRES_DSN`) was empty when the API started. The API logs
     `no POSTGRES_DSN set` at startup in that case.
   - A `500` from this endpoint means the DSN is set but the count query
     failed at request time; check Postgres health and the API logs.
   - `submitted_known: false` means this API process did not record that
     `run_id`. Causes: the process restarted (bookkeeping is in memory
     only), the request hit a different API replica, or the `run_id` is
     mistyped. `submitted` then reads `0`, which is not a claim that nothing
     was submitted. Restarting the API is safe for queued work but discards
     this bookkeeping for every earlier run.
5. Alerting: `NoSuccessfulRequests` fires when the harvester completes
   nothing for 10m. Use step 3 to tell the two causes apart: if `XLEN` is
   not growing, the producers (API or scheduler) have stopped; if it is
   growing, the harvesters are failing or not consuming (see "Success rate
   drops" and "Queue backlog growing").

## Scheduled harvests not firing

1. Check that scheduling is enabled at all. It is off unless `cmd/api` is
   started with `-schedule <file>`, and neither the `api` service in
   `deploy/docker-compose.yml` nor `deploy/systemd/serp-harvester-api.service`
   passes that flag or mounts a schedule file. Out of the box, this is the
   most likely cause.
2. With the flag set, the API logs one `scheduled harvest "<name>"
   registered` line per entry and then `scheduler running with N harvest(s)
   from <file>` at startup. If those lines are missing, scheduling is not
   running in that process.
3. An unreadable file, bad YAML, or an invalid `cron` expression is fatal:
   the API exits at startup (`load schedule: ...` or
   `register scheduled harvest: ...`) and, under `restart: unless-stopped`
   or `Restart=on-failure`, restarts in a loop. Expressions use the
   standard 5-field format (no seconds field) or descriptors such as
   `@every 15m`; see `configs/schedule.example.yaml`.
4. Schedules are evaluated in the API process's local time zone. The
   shipped image (`deploy/api.Dockerfile`, plain Alpine with no `tzdata`)
   always runs in UTC, so `0 10 * * *` means 10:00 UTC; setting `TZ` there
   has no effect unless `tzdata` is added to the image. Under systemd it is
   the host's time zone.
5. Confirm a tick produced work: after a scheduled time passes,
   `redis-cli XLEN serp-harvester:queries` should have grown by that
   harvest's query count.
6. The scheduler lives exactly as long as the API process; it has no
   separate lifecycle. If ticks stop while the process is up, capture the
   logs and restart the API.

## Job completion visibility (PostgreSQL integration)

1. `POSTGRES_DSN` must be set for both the API (`cmd/api`) and the harvester
   (`cmd/harvester`), pointing at the same database.
2. The harvester config must set `sink_backend: postgres`; with the default
   `jsonl` sink, results never reach the table the API counts from.
3. Spot check: submit a job, let it drain, then run
   `SELECT COUNT(*) FROM serp_results WHERE run_id = '<run_id>';`. Do not
   expect this to equal `queries_accepted` exactly:
   - Dropped jobs (retries exhausted, or cancelled while waiting on the
     rate limiter) write no row, so the count can be lower. Compare against
     `serp_harvester_dropped_total` over the same window.
   - The table has no uniqueness constraint on `(run_id, query)`, so a
     stream entry redelivered after a consumer crash can write a second
     row, so the count can be higher.

## Playwright mode (`mode: playwright`)

Browser mode renders pages; it does not get past CAPTCHAs, blocks or rate
limits. Most alerts below mean "the target is challenging this egress", and
the fix is lower volume, different egress, or the provider path, never a
code workaround.

1. The process exits at startup with `build playwright fetcher: ...`:
   - `start driver` means the Playwright driver is missing or the wrong
     version. In Docker, rebuild `deploy/playwright.Dockerfile`; elsewhere
     run `make playwright-install` with the same `PLAYWRIGHT_DRIVER_PATH` /
     `PLAYWRIGHT_BROWSERS_PATH` the process uses (systemd's
     `ProtectHome=true` hides `~/.cache`, see the unit file).
   - `launch chromium` means the browser or its system libraries are
     missing; `playwright install --with-deps chromium` installs both.
2. `BrowserBlockedPagesHigh` fired
   (`serp_harvester_browser_blocked_total` rising). Check the `reason`
   label:
   - `captcha`: the target served a CAPTCHA or "unusual traffic" page. The
     fetcher never interacts with it. Those failures already push the
     proxies involved into cooldown (`serp_harvester_proxy_banned_total`).
     Reduce `rate_per_proxy_rps`, add or change egress, or move this
     traffic to the provider path.
   - `consent`: a consent page with no "reject all" form the fetcher
     recognises. Capture one (run with `browser_headful: true` locally)
     and compare it with `classifyPage` / `rejectConsentForm` in
     `internal/fetcher/playwright.go`; the markup may have changed.
   - `interstitial`: the JS check persisted after rendering. Treat it like
     `captcha`.
3. `serp_harvester_browser_navigation_timeouts_total` rising: pages aren't
   loading within `request_timeout`. Check target or proxy latency first;
   raise `request_timeout` (30s is typical) only if pages are slow rather
   than hanging. A `browser_wait_selector` that never matches does not time
   out a fetch; it shows up as parser drift instead.
4. `BrowserDisconnectsHigh` fired, or `serp_harvester_browser_launches_total`
   keeps growing: Chromium is crashing. Usually memory: compare container
   memory against `browser_pool_size` x 100 to 300MB, and confirm
   `shm_size` is set (Docker's 64MB `/dev/shm` crashes tabs). Lower
   `browser_pool_size` before raising limits. Fetches in flight during a
   crash fail and are retried; the browser relaunches on the next fetch.
5. Throughput lower than expected: compare
   `serp_harvester_browser_sessions_in_use` with `browser_pool_size`. If
   it's pinned at the pool size, workers are waiting for sessions; raising
   `concurrency` above `browser_pool_size` adds nothing. Many distinct
   proxy x device x locale combinations also force session churn (visible
   as `serp_harvester_browser_sessions_open` at the cap with frequent
   evictions).
6. Unexpected traffic to Google services from the host, outside the proxy
   pool: check `browser_executable_path` and `browser_headful`. A full
   Chrome/Chromium build makes its own background requests to Google;
   Playwright's headless shell (the default) does not.

## Known gaps this runbook can't cover yet

- Prometheus alerting rules are included (`deploy/prometheus/alerts.yml`),
  but no Alertmanager is configured, so nothing routes alerts to Slack,
  PagerDuty or email. Firing alerts are visible only in Prometheus's UI.
- No automated proxy-pool refresh/rotation-from-provider integration exists
  — the proxy list in config is static. A real deployment would source it
  from whichever proxy vendor is chosen.
- `cmd/api` has no metrics endpoint, so API request rates and errors can
  only be observed through its logs.
