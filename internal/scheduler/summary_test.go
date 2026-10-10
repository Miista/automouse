package scheduler

import (
	"errors"
	"testing"
)

func TestDryRunSummary(t *testing.T) {
	cases := []struct {
		name    string
		vip     bool
		gb, fl  int
		skipped []string
		want    string
	}{
		{"buys", true, 50, 0, nil, "Would buy VIP renewal (cost estimated), 50 GiB upload credit."},
		{"nothing enabled", false, 0, 0, nil, "Would buy nothing this run: no purchase type is enabled."},
		{"explains skip", false, 0, 0, []string{"a", "b"}, "Would buy nothing this run: a; b."},
		{"purchase wins over skip", false, 50, 0, []string{"VIP not due"}, "Would buy 50 GiB upload credit."},
	}
	for _, tc := range cases {
		if got := dryRunSummary(tc.vip, tc.gb, tc.fl, tc.skipped); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPurchaseError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("mam rejected request: Insufficient funds"), "Insufficient funds"},
		{errors.New("mam http 500"), "mam http 500"},
	}
	for _, tc := range cases {
		if got := purchaseError(tc.err); got != tc.want {
			t.Errorf("purchaseError(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestRunSummary(t *testing.T) {
	cases := []struct {
		name            string
		vip             bool
		gb, fl          int
		skipped, failed []string
		want            string
	}{
		{"bought", true, 50, 0, nil, nil, "Bought VIP renewal, 50 GiB upload credit."},
		{"nothing enabled", false, 0, 0, nil, nil, "Bought nothing: no purchase type is enabled."},
		{"skipped", false, 0, 0, []string{"VIP not due (80 days left)"}, nil, "Bought nothing: VIP not due (80 days left)."},
		{"vip rejected", false, 0, 0, nil, []string{"VIP renewal: not eligible"}, "Bought nothing: purchase failed — VIP renewal: not eligible."},
		{"partial", false, 50, 0, nil, []string{"VIP renewal: not eligible"}, "Bought 50 GiB upload credit. Purchase failed — VIP renewal: not eligible."},
	}
	for _, tc := range cases {
		if got := runSummary(tc.vip, tc.gb, tc.fl, tc.skipped, tc.failed); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
