# Live tests

Integration tests that make real network requests. They are never run by
`go test ./...` in normal use or CI — every test here checks
`HARVESTER_LIVE=true` first and skips otherwise, so this package can't
accidentally fire against a real target.

## Running

```bash
# Direct-to-Google (no key needed, documents the current blocked state):
HARVESTER_LIVE=true go test ./tests/live/... -run GoogleDirect -v

# Real provider smoke test (needs a real key):
HARVESTER_LIVE=true SERPAPI_KEY=... go test ./tests/live/... -run Provider -v
```

## What each one proves

- **`TestGoogleDirect_CurrentlyBlocked`** — reproduces the finding in
  README "Live mode": a plain HTTP fetch against Google doesn't currently
  return usable results. It's written to keep that finding honest over
  time — if Google's behavior changes and real results start coming back,
  this test logs that loudly rather than silently passing either way.
- **`TestProvider_RealKeySmokeTest`** — the one test in this repo that
  can't be verified without a real API key. It confirms the JSON field
  mapping in `internal/parser/json_provider.go` (built from public docs)
  actually matches what a real provider sends back. Skips cleanly with no
  key configured.
