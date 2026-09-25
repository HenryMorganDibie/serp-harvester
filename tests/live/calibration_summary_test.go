package live

import (
	"strings"
	"testing"
	"time"
)

// summarize runs without network access, so its logic is checked here
// rather than for the first time against Google.
func TestSummarize(t *testing.T) {
	sample := func(i int, outcome string, retryAfter time.Duration) calibrationSample {
		return calibrationSample{Index: i, Elapsed: time.Duration(i) * 10 * time.Second, Outcome: outcome, RetryAfter: retryAfter}
	}

	t.Run("no pushback", func(t *testing.T) {
		s := summarize("http", 10*time.Second, []calibrationSample{sample(0, "success", 0), sample(1, "success", 0)})
		if s.FirstPushback != -1 || len(s.Suggestions) != 1 || !strings.Contains(s.Suggestions[0], "no pushback in 2 requests at 0.100 req/s") {
			t.Errorf("%+v", s)
		}
	})

	t.Run("block then recovery", func(t *testing.T) {
		s := summarize("playwright", 10*time.Second, []calibrationSample{
			sample(0, "success", 0),
			sample(1, "success", 0),
			sample(2, "rate_limited", 60*time.Second),
			sample(3, "captcha", 0),
			sample(4, "captcha", 0),
			sample(5, "success", 0),
		})
		if s.FirstPushback != 2 || s.PushbackAfter != 20*time.Second {
			t.Errorf("first pushback = %d after %s", s.FirstPushback, s.PushbackAfter)
		}
		if s.RecoveredAfter != 30*time.Second || s.MaxRetryAfter != 60*time.Second {
			t.Errorf("recovered after %s, max Retry-After %s", s.RecoveredAfter, s.MaxRetryAfter)
		}
		if s.Outcomes["captcha"] != 2 || s.Outcomes["success"] != 3 {
			t.Errorf("outcomes %v", s.Outcomes)
		}
		joined := strings.Join(s.Suggestions, "\n")
		for _, want := range []string{"first pushback at request 2", "below 0.050", "proxy_ban_cooldown of about 30s", "largest Retry-After was 1m0s"} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing %q in:\n%s", want, joined)
			}
		}
	})

	t.Run("no recovery", func(t *testing.T) {
		s := summarize("http", 5*time.Second, []calibrationSample{sample(0, "interstitial", 0), sample(1, "interstitial", 0)})
		if s.RecoveredAfter != 0 || !strings.Contains(strings.Join(s.Suggestions, "\n"), "no recovery observed") {
			t.Errorf("%+v", s)
		}
	})
}
