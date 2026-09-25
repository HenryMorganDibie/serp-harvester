# Live tests

Integration tests that make real network requests. They are never run by
`go test ./...` in normal use or CI — every test here checks
`HARVESTER_LIVE=true` first and skips otherwise, so this package can't
accidentally fire against a real target.

## Running

```bash
# Direct-to-Google (no key needed, documents the current blocked state):
HARVESTER_LIVE=true go test ./tests/live/... -run GoogleDirect -v

# Direct-to-Google through headless Chromium (needs `make playwright-install`):
HARVESTER_LIVE=true go test ./tests/live/... -run Playwright -v

# Capture real Google pages (results and block pages) for parser work:
HARVESTER_LIVE=true SERP_CAPTURE_DIR=./captures go test ./tests/live/... -run Capture -v

# Measure pushback and recovery to calibrate rates and cooldowns (slow by design):
HARVESTER_LIVE=true SERP_CAPTURE_DIR=./captures SERP_CALIBRATION_REQUESTS=30 \
  SERP_CALIBRATION_INTERVAL=20s go test ./tests/live/... -run Calibration -v -timeout 60m

# Real provider smoke test (needs a real key):
HARVESTER_LIVE=true SERPAPI_KEY=... go test ./tests/live/... -run Provider -v
```

## What each one proves

- **`TestGoogleDirect_CurrentlyBlocked`** — reproduces the finding in
  README "Live mode": a plain HTTP fetch against Google doesn't currently
  return usable results. It's written to keep that finding honest over
  time — if Google's behavior changes and real results start coming back,
  this test logs that loudly rather than silently passing either way.
- **`TestGoogleCapture_SavesPages`**: a few queries (`SERP_CAPTURE_QUERIES`;
  `SERP_CAPTURE_MODES=http` or `playwright` limits it to one fetcher)
  via the HTTP fetcher and Chromium, 10s apart. Every page Google returns,
  results or block page, is saved to `SERP_CAPTURE_DIR` as `.html` with a
  `.json` of its classification and what the current parser extracted. These
  captures are what retargeting the parser at live markup needs; captured
  Google pages should stay out of the repository.
- **`TestGoogleCalibration_MeasuresPushback`**: `SERP_CALIBRATION_REQUESTS`
  (default 20, max 100) queries at `SERP_CALIBRATION_INTERVAL` (default 15s,
  min 3s) through one fetcher (`SERP_LIVE_MODE`, default playwright) and
  optionally one proxy (`SERP_LIVE_PROXY`). It keeps the same slow pace after
  a block to measure recovery, then writes a JSON report with outcomes, first
  pushback, largest `Retry-After`, time to recovery and suggested
  `rate_per_proxy_rps` / `proxy_ban_cooldown`. It records blocks; it never
  tries to get past them. A run blocked from its first request with no
  success is reported as an egress-level block with no supported rate,
  rather than as a rate to stay below. Relative `SERP_CAPTURE_DIR` paths
  resolve under `tests/live/`. Results of the first live run are in the
  top-level README, "Measuring against Google".
- **`TestGooglePlaywright_RecordsOutcome`**: one query through the
  Playwright fetcher, logging whether Google returned a rendered results
  page (and whether the fixture-targeted parser matched it), a block page
  (CAPTCHA, consent, interstitial) or a rate limit. A block is a logged
  outcome, not a failure, since the fetcher does not bypass blocks.
- **`TestProvider_RealKeySmokeTest`** — the one test in this repo that
  can't be verified without a real API key. It confirms the JSON field
  mapping in `internal/parser/json_provider.go` (built from public docs)
  actually matches what a real provider sends back. Skips cleanly with no
  key configured.
