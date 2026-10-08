package scheduler

import "testing"

func TestDryRunSummary(t *testing.T) {
	cases := []struct {
		name    string
		vip     bool
		gb, fl  int
		skipped []string
		want    string
	}{
		{"buys", true, 50, 0, nil, "Would buy VIP renewal, 50 GiB upload credit."},
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
