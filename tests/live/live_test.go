// Package live holds integration tests that make real network requests.
// They never run as part of `go test ./...` in normal CI — every test here
// skips immediately unless HARVESTER_LIVE=true is set, so CI stays fully
// offline and these can't accidentally fire against a real target.
package live

import (
	"os"
	"testing"
)

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("HARVESTER_LIVE") != "true" {
		t.Skip("set HARVESTER_LIVE=true to run live tests against real endpoints (see tests/live/README.md)")
	}
}
