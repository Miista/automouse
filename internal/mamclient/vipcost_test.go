package mamclient

import "testing"

func TestEstimateVIPCost(t *testing.T) {
	cases := []struct {
		name      string
		remaining float64
		want      int
	}{
		{"floor of 7 days at the renew threshold", 83, 1250},
		{"empty tops up to the 90 day cap", 0, 16071},
		{"mid top-up", 62, 5000},
	}
	for _, tc := range cases {
		if got := EstimateVIPCost(tc.remaining); got != tc.want {
			t.Errorf("%s: EstimateVIPCost(%v) = %d, want %d", tc.name, tc.remaining, got, tc.want)
		}
	}
}
