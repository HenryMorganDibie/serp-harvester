package fetcher

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// RateLimitError indicates the target responded with a rate-limit signal
// (HTTP 429) and, if the response specified one, how long to wait before
// retrying. Callers (see worker.Pool) can check for this via errors.As and
// honor RetryAfter instead of blind exponential backoff — the point of a
// provider integration respecting the provider's own rate-limit signaling
// rather than guessing.
type RateLimitError struct {
	StatusCode int
	RetryAfter time.Duration // zero if the response didn't specify one
	Body       string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("fetcher: rate limited (status %d, retry after %s)", e.StatusCode, e.RetryAfter)
	}
	return fmt.Sprintf("fetcher: rate limited (status %d, no retry-after given)", e.StatusCode)
}

// parseRetryAfter parses a standard Retry-After header value, which per
// RFC 9110 §10.2.3 is either an integer number of seconds or an HTTP-date.
// Returns 0 (with ok=false) if header is empty or unparseable.
func parseRetryAfter(header string) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0, false
		}
		return d, true
	}
	return 0, false
}
