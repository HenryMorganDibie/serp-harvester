# Capabilities

A direct answer to the five questions this project was built to answer.
Every claim here links to the code, test, or measurement that backs it —
nothing here is asserted without something to point at.

## Which working scrapers do you currently have, built/operated by you?

My current working SERP collection system is this distributed Go pipeline:
self-hosted worker pool, Redis Streams for horizontal scaling, proxy
rotation, rate limiting, structured extraction with drift detection, and
Prometheus observability, running end to end today. See
[PRODUCTION_READINESS.md](PRODUCTION_READINESS.md) for the full
architecture-to-operations breakdown. What it hasn't done yet is operate
against your specific volume in production — see the volume question below
for exactly what that means and doesn't mean.

## Do you have a Google SERP / AI Overviews solution? Direct from Google, or third-party?

Both, behind one interface (`internal/fetcher.Fetcher`):

| Capability                | Direct Google (`mode: live`) | Third-party provider (`mode: provider`) |
|----------------------------|------------------------------|-------------------------------------------|
| Organic results            | Built; parser verified against fixtures, not live markup | Built and structurally verified against a real, public API schema |
| Featured snippet / PAA     | Built (fixture-verified)     | Built (fixture-verified) |
| AI Overview (inline)       | Built (fixture-verified)     | Built (fixture-verified) |
| AI Overview (page-token follow-up) | Not applicable — this is a provider-specific async mechanism | Built and tested (`internal/fetcher/provider_test.go`) |
| Consent/session handling   | Cookie jar implemented; **confirmed insufficient** — see below | Not applicable — provider handles this |
| CAPTCHA / bot-detection handling | Not implemented (deliberately — see [README "Honest limitations"](README.md#honest-limitations)) | Handled by the provider as their product |
| Proxy rotation & rate limiting | Built (`internal/proxy`, `internal/ratelimit`) | Built, same code path |
| Self-hostable               | Yes | Yes |
| Third-party dependency      | None | The provider itself |
| Who owns the ToS/legal risk | You, if used at volume | The provider, priced into their product |

**Tested, not assumed:** direct-to-Google requests were run against the live
site. Two distinct blocking responses were observed on different attempts —
a consent interstitial and a JavaScript-execution check — neither is real
search results, and neither was bypassed (no CAPTCHA solving or fingerprint
spoofing was added; see README for why). This is reproducible via
`HARVESTER_LIVE=true go test ./tests/live/... -run GoogleDirect -v`
(`tests/live/google_direct_test.go`).

**Recommended path for real volume:** the provider fetch path. It's fully
built and unit-tested against a realistic fixture and a local `httptest`
server; the one thing not yet verified is a real vendor's exact field names
(`tests/live/provider_test.go` is ready to confirm this the moment a trial
key exists).

## What sustained daily volume have you handled, particularly for Google, and over what period?

**Honestly: none yet, in production.** This is a new build, not an existing
operation with a track record. What exists instead:

- **Offline orchestration benchmark** (measures this codebase's own
  queue/worker/proxy/rate-limit/parse pipeline, mock fetcher, zero network):
  see [README "Load testing"](README.md#load-testing) for the concurrency
  sweep table with real measured numbers, currently up to ~15,000 req/s at
  concurrency 800 — well past the ~116 req/s that 10M/day requires.
- **Soak test**: a sustained run tracking memory, goroutine count, and
  throughput consistency over time — see the latest results in
  [README "Load testing"](README.md#load-testing) for duration actually run
  and outcome.
- **Production history**: none to report. Once this runs against real
  traffic (via the provider path), this section gets updated with actual
  dates, actual daily volume, and actual success rates — not before.

## Could you deploy and maintain the solution on your servers, rather than Apify or an external hosted API?

Yes, concretely: [`deploy/`](deploy/) contains a working Docker Compose
stack (harvester + Redis + Prometheus + Grafana) and a systemd unit for
bare-metal/VM deployment, both running entirely on infrastructure you
control. `docker compose -f deploy/docker-compose.yml up -d` is the whole
deployment. See [`deploy/README.md`](deploy/README.md).

The only external dependency, if you choose `mode: provider`, is API calls
to the SERP data vendor you pick — the same shape as calling any other paid
API from your own backend, not a hosted scraping platform sitting in front
of your data.

## What would your deployment timeline and indicative setup/monthly support costs be?

Rough shape, to be refined once scope (query mix, target locales, required
provider) is agreed:

- **Week 1**: wire the pipeline to your chosen provider and target
  markets/locales, deploy to your infrastructure, first data flowing.
- **Week 2**: calibrate for your actual query mix, scale workers/Redis
  consumers if a single host isn't enough, get the Grafana dashboard and
  alerting tuned to your thresholds.
- **Ongoing**: selector/schema maintenance as the provider or Google's
  markup changes, monitoring, support.

Setup and monthly costs mostly track the provider's request pricing at your
target volume, plus engineering time — I don't have real numbers to quote
until we know your query mix and provider choice, and I'd rather say that
than guess.
