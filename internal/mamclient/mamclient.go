// Package mamclient talks to MyAnonamouse's JSON endpoints to check account
// status and spend bonus points.
package mamclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	baseURL     = "https://www.myanonamouse.net"
	userAgent   = "automouse-go"
	PointsPerGB = 500
	// MinUploadGB is MAM's own minimum for automated upload-credit
	// purchases, confirmed directly against the live API: amounts below
	// this are rejected with "Automated spenders are limited to buying at
	// least 50 GB of upload at a time, due to log spam". This is a MAM-side
	// floor, not a design choice — verified 2026-09-29 with amount=1
	// (rejected) and amount=51 (passed validation, only failed on
	// insufficient balance), confirming it's a >= floor, not "must be a
	// multiple of 50".
	MinUploadGB = 50
	FLWedgeCost = 50000
	// VIPRenewDays is the remaining-VIP threshold below which a renewal
	// purchase is worth making. `bonusBuy.php?spendtype=VIP&duration=max`
	// doesn't add a fixed number of days — it tops VIP up to a 90-day cap,
	// with a guaranteed minimum grant of 7 days per purchase (confirmed
	// directly against MAM's account owner, 2026-09-30). Renewing any
	// earlier than 90-7=83 days remaining would only buy a partial top-up
	// you're not yet eligible for the full benefit of, since the cap blocks
	// going past 90 — so 83 is the latest point at which a renewal is
	// guaranteed to be a full, non-wasteful 7+ day top-up.
	VIPRenewDays = 83
)

// Client makes authenticated requests to MAM using the mam_id session
// cookie. The cookie is held as a Secret so it can never be accidentally
// logged in full.
type Client struct {
	cookie Secret
	http   *http.Client
	// baseURL is the MAM origin all requests are made against. It is a
	// field rather than a constant purely so tests can point the client at
	// an httptest server serving recorded responses; production code never
	// sets it and it defaults to baseURL.
	baseURL string
}

// New creates a client authenticated with the given mam_id cookie value.
func New(mamID Secret) *Client {
	return &Client{
		cookie:  mamID,
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: baseURL,
	}
}

func (c *Client) get(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json,text/plain,*/*")
	req.Header.Set("Cookie", "mam_id="+url.QueryEscape(c.cookie.Reveal()))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// 429 is the standard rate-limit status; 503 with a Retry-After is the
	// other shape a limiter commonly takes. Both are reported as a typed
	// RateLimitError so the scheduler can back off rather than retrying on
	// its normal cadence, which is what turns a brief limit into a long one.
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") != "") {
		return nil, &RateLimitError{
			RetryAfter: parseRetryAfter(resp.Header),
			StatusCode: resp.StatusCode,
		}
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mam http %d", resp.StatusCode)
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("mam returned non-JSON response")
	}
	// bonusBuy.php (and possibly other endpoints) report a *logical*
	// failure with HTTP 200 and a body like {"success":false,"error":"..."}
	// — confirmed directly against the live API (e.g. rejecting a below-
	// floor upload amount). A non-4xx/5xx status alone does not mean the
	// request did what it asked; only treat this as an error when
	// "success" is explicitly present and false, since plain data-fetching
	// endpoints don't include this field at all.
	if success, ok := data["success"].(bool); ok && !success {
		if errMsg, ok := data["error"].(string); ok && errMsg != "" {
			return data, fmt.Errorf("mam rejected request: %s", errMsg)
		}
		return data, fmt.Errorf("mam rejected request")
	}
	return data, nil
}

// UserID returns the account's uid, validating that the session cookie is
// still valid.
func (c *Client) UserID() (string, error) {
	data, err := c.get("/jsonLoad.php?snatch_summary")
	if err != nil {
		return "", err
	}
	uid, _ := data["uid"].(string)
	if uid == "" {
		if n, ok := data["uid"].(float64); ok {
			uid = strconv.Itoa(int(n))
		}
	}
	return uid, nil
}

// SeedBonus returns the current bonus point balance for uid.
func (c *Client) SeedBonus(uid string) (int, error) {
	data, err := c.get("/jsonLoad.php?uid=" + url.QueryEscape(uid))
	if err != nil {
		return 0, err
	}
	return asInt(data["seedbonus"]), nil
}

// VIPExpiry returns the account's current VIP expiry time.
func (c *Client) VIPExpiry() (time.Time, error) {
	data, err := c.get("/jsonLoad.php")
	if err != nil {
		return time.Time{}, err
	}
	raw, _ := data["vip_until"].(string)
	return parseMamDate(raw), nil
}

// BuyVIP purchases the maximum available VIP renewal.
func (c *Client) BuyVIP() error {
	ts := timestampMS()
	_, err := c.get(fmt.Sprintf("/json/bonusBuy.php/?spendtype=VIP&duration=max&_=%s", ts))
	return err
}

// BuyFreeleechWedge purchases one freeleech wedge using points.
func (c *Client) BuyFreeleechWedge() error {
	ts := timestampMS()
	_, err := c.get(fmt.Sprintf("/json/bonusBuy.php/?spendtype=wedges&source=points&_=%s", ts))
	return err
}

// BuyUploadCredit purchases gb GiB of upload credit.
func (c *Client) BuyUploadCredit(gb int) error {
	_, err := c.get(fmt.Sprintf("/json/bonusBuy.php/?spendtype=upload&amount=%d", gb))
	return err
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	default:
		return 0
	}
}

func timestampMS() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

var mamDateLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02",
	"Jan 2, 2006 3:04 PM",
	time.RFC3339,
}

func parseMamDate(value string) time.Time {
	if value == "" {
		return time.Unix(0, 0)
	}
	for _, layout := range mamDateLayouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	return time.Unix(0, 0)
}
