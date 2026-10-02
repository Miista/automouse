package mamclient

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// defaultRetryAfter is how long to back off when MAM rate-limits us without
// saying for how long. MAM documents no limits and sends no Retry-After in
// the cases observed, so this is a deliberately conservative guess: long
// enough to stop hammering a tripped limit, short enough that a transient
// one doesn't cost a whole day of runs.
const defaultRetryAfter = 30 * time.Minute

// RateLimitError reports that MAM refused the request because we are making
// too many. It carries how long to wait before trying again, taken from the
// Retry-After header when one is present and defaultRetryAfter otherwise.
//
// This is a distinct type rather than a generic HTTP error because the
// scheduler has to treat it differently: an ordinary failure means retry on
// the normal schedule, whereas retrying into a rate limit is what prolongs
// it.
type RateLimitError struct {
	RetryAfter time.Duration
	StatusCode int
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("mam rate limited (http %d), retry after %s", e.StatusCode, e.RetryAfter)
}

// parseRetryAfter reads the Retry-After header, which RFC 9110 allows to be
// either a delay in seconds or an HTTP date. Returns defaultRetryAfter when
// the header is absent, malformed, or in the past.
func parseRetryAfter(h http.Header) time.Duration {
	raw := h.Get("Retry-After")
	if raw == "" {
		return defaultRetryAfter
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return defaultRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return defaultRetryAfter
}
