package mamclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimitErrorFrom429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(Secret("x"))
	c.baseURL = srv.URL
	_, err := c.UserID()

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v (%T), want *RateLimitError", err, err)
	}
	if rl.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", rl.StatusCode)
	}
	if rl.RetryAfter != defaultRetryAfter {
		t.Errorf("RetryAfter = %s, want the default %s", rl.RetryAfter, defaultRetryAfter)
	}
}

func TestRateLimitHonoursRetryAfterSeconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(Secret("x"))
	c.baseURL = srv.URL
	_, err := c.UserID()

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if rl.RetryAfter != 2*time.Minute {
		t.Errorf("RetryAfter = %s, want 2m", rl.RetryAfter)
	}
}

// TestRateLimitHonoursRetryAfterDate covers the other form RFC 9110 allows:
// an HTTP date rather than a delay in seconds.
func TestRateLimitHonoursRetryAfterDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(10*time.Minute).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(Secret("x"))
	c.baseURL = srv.URL
	_, err := c.UserID()

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	// Allow slack for the second-granularity of HTTP dates and transit time.
	if rl.RetryAfter < 9*time.Minute || rl.RetryAfter > 10*time.Minute {
		t.Errorf("RetryAfter = %s, want ~10m", rl.RetryAfter)
	}
}

// TestServiceUnavailableOnlyRateLimitsWithRetryAfter pins that a bare 503 is
// an ordinary outage, not a rate limit — only one carrying Retry-After is
// treated as a limiter telling us to wait.
func TestServiceUnavailableOnlyRateLimitsWithRetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		wantLimit  bool
	}{
		{"bare 503 is an outage", "", false},
		{"503 with Retry-After is a limit", "60", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer srv.Close()

			c := New(Secret("x"))
			c.baseURL = srv.URL
			_, err := c.UserID()

			var rl *RateLimitError
			if got := errors.As(err, &rl); got != tc.wantLimit {
				t.Errorf("treated as rate limit = %v, want %v (err: %v)", got, tc.wantLimit, err)
			}
		})
	}
}

// TestOtherErrorsAreNotRateLimits guards against over-matching: a 403 from a
// bad cookie must stay an ordinary error, or the scheduler would back off
// for half an hour over a problem backing off cannot fix.
func TestOtherErrorsAreNotRateLimits(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusInternalServerError, http.StatusBadGateway} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		c := New(Secret("x"))
		c.baseURL = srv.URL
		_, err := c.UserID()
		srv.Close()

		var rl *RateLimitError
		if errors.As(err, &rl) {
			t.Errorf("http %d was treated as a rate limit", status)
		}
		if err == nil {
			t.Errorf("http %d produced no error", status)
		}
	}
}

func TestParseRetryAfterFallsBackOnJunk(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"absent", ""},
		{"not a number or date", "soon"},
		{"zero", "0"},
		{"negative", "-30"},
		{"a date in the past", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			if got := parseRetryAfter(h); got != defaultRetryAfter {
				t.Errorf("parseRetryAfter(%q) = %s, want the default %s", tc.value, got, defaultRetryAfter)
			}
		})
	}
}

func TestRateLimitErrorMessage(t *testing.T) {
	err := &RateLimitError{RetryAfter: 90 * time.Second, StatusCode: 429}
	const want = "mam rate limited (http 429), retry after 1m30s"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}
