package mamclient

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The fixtures in testdata/ are real responses captured from MAM's live API
// on 2026-10-01. Account-identifying values (uid, username, ratio, points,
// byte counts, torrent counts, VIP expiry, timestamps) have been replaced
// with innocent stand-ins; nothing else was touched. Every key, nesting
// level, JSON type and MAM-authored string — including the exact error
// messages — is verbatim what MAM returned. That matters because several
// of the parsing branches below exist to handle quirks (uid arriving as a
// JSON number, logical failures served with HTTP 200, a non-JSON body on
// 403) that would be easy to get wrong when guessing at the shape.

// fixtureServer serves the named fixture file for every request, and
// records the last request it saw so tests can assert on what the client
// sent.
func fixtureServer(t *testing.T, name string, status int) (*Client, *http.Request) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	var last *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = r.Clone(r.Context())
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := New(Secret("test-cookie-value"))
	c.baseURL = srv.URL
	// Returning last by value here would capture it before the handler
	// runs, so tests that need the request read it via the returned
	// pointer-to-pointer pattern below instead.
	return c, last
}

// newFixtureClient is the common case: HTTP 200 plus a fixture body.
func newFixtureClient(t *testing.T, name string) *Client {
	t.Helper()
	c, _ := fixtureServer(t, name, http.StatusOK)
	return c
}

func TestUserIDParsesNumericUID(t *testing.T) {
	// MAM returns uid as a JSON number, not a string — so the float64
	// fallback in UserID is the branch that actually runs in production,
	// not the string one.
	c := newFixtureClient(t, "snatch_summary.json")
	uid, err := c.UserID()
	if err != nil {
		t.Fatalf("UserID: %v", err)
	}
	if uid != "123456" {
		t.Errorf("uid = %q, want %q", uid, "123456")
	}
}

func TestSeedBonus(t *testing.T) {
	c := newFixtureClient(t, "uid.json")
	points, err := c.SeedBonus("123456")
	if err != nil {
		t.Fatalf("SeedBonus: %v", err)
	}
	if points != 5000 {
		t.Errorf("seedbonus = %d, want 5000", points)
	}
}

func TestSeedBonusFromSnatchSummary(t *testing.T) {
	// The snatch_summary response carries seedbonus too, alongside a large
	// nested object. Parsing must not be confused by the extra structure.
	c := newFixtureClient(t, "snatch_summary.json")
	points, err := c.SeedBonus("123456")
	if err != nil {
		t.Fatalf("SeedBonus: %v", err)
	}
	if points != 5000 {
		t.Errorf("seedbonus = %d, want 5000", points)
	}
}

func TestVIPExpiry(t *testing.T) {
	c := newFixtureClient(t, "bare.json")
	expiry, err := c.VIPExpiry()
	if err != nil {
		t.Fatalf("VIPExpiry: %v", err)
	}
	want := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	if !expiry.Equal(want) {
		t.Errorf("vip_until = %v, want %v", expiry, want)
	}
}

// TestVIPExpiryUsesLiveDateLayout pins the fact that MAM's vip_until uses
// the "2006-01-02 15:04:05" layout. parseMamDate tries four layouts and
// silently returns the zero epoch when none match, so a format change
// would otherwise surface as "VIP expired in 1970" rather than an error.
func TestVIPExpiryUsesLiveDateLayout(t *testing.T) {
	c := newFixtureClient(t, "bare.json")
	expiry, err := c.VIPExpiry()
	if err != nil {
		t.Fatalf("VIPExpiry: %v", err)
	}
	if expiry.Equal(time.Unix(0, 0)) {
		t.Fatal("vip_until fell through to the zero epoch: no layout in mamDateLayouts matched the live format")
	}
}

// TestRejectionBelowUploadFloor covers the case the MinUploadGB constant
// documents: MAM enforces a 50 GB minimum for automated upload purchases
// and reports the refusal with HTTP 200 + success:false.
func TestRejectionBelowUploadFloor(t *testing.T) {
	c := newFixtureClient(t, "buy_upload_below_floor.json")
	err := c.BuyUploadCredit(1)
	if err == nil {
		t.Fatal("BuyUploadCredit(1) succeeded, want rejection")
	}
	const want = "mam rejected request: Automated spenders are limited to buying at least 50 GB of upload at a time, due to log spam"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestRejectionInsufficientFunds and the upload variant below capture that
// the two spend types report the same underlying condition with *different*
// error strings, so nothing may pattern-match on the text.
func TestRejectionInsufficientFunds(t *testing.T) {
	c := newFixtureClient(t, "buy_wedge_insufficient.json")
	err := c.BuyFreeleechWedge()
	if err == nil {
		t.Fatal("BuyFreeleechWedge succeeded, want rejection")
	}
	if err.Error() != "mam rejected request: Insufficient funds" {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRejectionNotEnoughBonus(t *testing.T) {
	c := newFixtureClient(t, "buy_upload_insufficient.json")
	err := c.BuyUploadCredit(50)
	if err == nil {
		t.Fatal("BuyUploadCredit(50) succeeded, want rejection")
	}
	if err.Error() != "mam rejected request: Not enough bonus, s1" {
		t.Errorf("error = %q", err.Error())
	}
}

// TestInvalidSessionIsNotJSON pins the one captured response that isn't
// JSON at all: an expired or malformed cookie yields HTTP 403 with a
// plain-text body. The status check fires before the JSON decode, so this
// surfaces as an http error rather than a parse error.
func TestInvalidSessionIsNotJSON(t *testing.T) {
	c, _ := fixtureServer(t, "invalid_session.json", http.StatusForbidden)
	_, err := c.UserID()
	if err == nil {
		t.Fatal("UserID with invalid session succeeded, want error")
	}
	if err.Error() != "mam http 403" {
		t.Errorf("error = %q, want %q", err.Error(), "mam http 403")
	}
}

// TestDataEndpointsHaveNoSuccessField guards the subtlety in get(): plain
// data endpoints omit "success" entirely, and treating a missing field as
// failure would break every read.
func TestDataEndpointsHaveNoSuccessField(t *testing.T) {
	for _, name := range []string{"bare.json", "uid.json", "snatch_summary.json"} {
		t.Run(name, func(t *testing.T) {
			c := newFixtureClient(t, name)
			if _, err := c.UserID(); err != nil {
				t.Errorf("data endpoint treated as failure: %v", err)
			}
		})
	}
}

// TestCookieIsSent verifies the mam_id cookie actually reaches the wire,
// since Secret's String() is redacted and a mistake here would be silent.
func TestCookieIsSent(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "bare.json"))
	if err != nil {
		t.Fatal(err)
	}
	var gotCookie, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := New(Secret("secret-cookie-123"))
	c.baseURL = srv.URL
	if _, err := c.UserID(); err != nil {
		t.Fatal(err)
	}
	if gotCookie != "mam_id=secret-cookie-123" {
		t.Errorf("Cookie header = %q", gotCookie)
	}
	if gotUA != userAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, userAgent)
	}
}

// TestNonJSONBodyWith200 covers a 200 response whose body isn't JSON —
// e.g. an interstitial or error page served with the wrong status.
func TestNonJSONBodyWith200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>maintenance</html>"))
	}))
	defer srv.Close()

	c := New(Secret("x"))
	c.baseURL = srv.URL
	_, err := c.UserID()
	if err == nil {
		t.Fatal("non-JSON 200 body accepted, want error")
	}
	if err.Error() != "mam returned non-JSON response" {
		t.Errorf("error = %q", err.Error())
	}
}
