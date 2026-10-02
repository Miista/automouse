package scheduler

import (
	"math"
	"testing"
	"time"

	"github.com/miista/automouse/internal/mamclient"
	"github.com/miista/automouse/internal/store"
)

// at builds a sample n hours after a fixed base, so tests read as a
// timeline rather than a pile of timestamps.
func at(hours float64, points int) store.AccrualSample {
	base := time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC)
	return store.AccrualSample{
		At:     base.Add(time.Duration(hours * float64(time.Hour))),
		Points: points,
	}
}

func TestRateFrom(t *testing.T) {
	tests := []struct {
		name     string
		samples  []store.AccrualSample
		wantRate float64
		wantOK   bool
	}{
		{
			// The live figure from MAM's own bonus page: 118.9/hour.
			name:     "three hourly samples at ~118.9/hr",
			samples:  []store.AccrualSample{at(0, 4088), at(1, 4207), at(2, 4326)},
			wantRate: 119,
			wantOK:   true,
		},
		{
			name:     "spans first to last, not just the final pair",
			samples:  []store.AccrualSample{at(0, 1000), at(1, 1100), at(2, 1200), at(3, 1300)},
			wantRate: 100,
			wantOK:   true,
		},
		{
			name:    "too few samples",
			samples: []store.AccrualSample{at(0, 1000), at(1, 1100)},
			wantOK:  false,
		},
		{
			name:    "no samples",
			samples: nil,
			wantOK:  false,
		},
		{
			// A decrease means points were spent during the window — on the
			// MAM site, or by one of our own runs — so the window says
			// nothing about accrual and the previous rate must stand.
			name:    "balance went down (spent during the window)",
			samples: []store.AccrualSample{at(0, 4088), at(1, 4207), at(2, 3100)},
			wantOK:  false,
		},
		{
			name:    "balance flat across the window",
			samples: []store.AccrualSample{at(0, 5000), at(1, 5000), at(2, 5000)},
			wantOK:  false,
		},
		{
			// Guards against a divide-by-zero if samples ever land on the
			// same instant.
			name:    "zero elapsed time",
			samples: []store.AccrualSample{at(0, 1000), at(0, 1100), at(0, 1200)},
			wantOK:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rateFrom(tc.samples)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (rate %v)", ok, tc.wantOK, got)
			}
			if ok && math.Abs(got-tc.wantRate) > 0.5 {
				t.Errorf("rate = %.2f, want ~%.2f", got, tc.wantRate)
			}
		})
	}
}

func TestInAccrualWindow(t *testing.T) {
	tests := []struct {
		hour int
		want bool
	}{
		{0, false}, // midnight: before the window
		{1, true},  // 01:00: window opens
		{2, true},
		{3, true},
		{4, false}, // 04:00: window closed
		{12, false},
		{23, false},
	}
	for _, tc := range tests {
		ts := time.Date(2026, 10, 2, tc.hour, 30, 0, 0, time.UTC)
		if got := inAccrualWindow(ts); got != tc.want {
			t.Errorf("inAccrualWindow(%02d:30) = %v, want %v", tc.hour, got, tc.want)
		}
	}
}

func TestSameNight(t *testing.T) {
	a := time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC)
	if !sameNight(a, a.Add(2*time.Hour)) {
		t.Error("01:00 and 03:00 on the same date should be the same night")
	}
	if sameNight(a, a.Add(24*time.Hour)) {
		t.Error("samples a day apart should not be the same night")
	}
}

func TestCheapestThreshold(t *testing.T) {
	const buffer = 10000
	upload := mamclient.MinUploadGB*mamclient.PointsPerGB + buffer // 25,000 + buffer
	wedge := mamclient.FLWedgeCost + buffer                        // 50,000 + buffer

	tests := []struct {
		strategy string
		want     int
		wantOK   bool
	}{
		{store.StrategyUploadOnly, upload, true},
		{store.StrategyWedgeOnly, wedge, true},
		// Either could be next, so the cheaper one is where runs stop being
		// guaranteed no-ops.
		{store.StrategyAlternate, upload, true},
		{store.StrategyNone, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.strategy, func(t *testing.T) {
			got, ok := cheapestThreshold(store.Settings{
				UploadStrategy: tc.strategy,
				PointsBuffer:   buffer,
			})
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("threshold = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCheapestThresholdIncludesBuffer(t *testing.T) {
	withBuffer, _ := cheapestThreshold(store.Settings{UploadStrategy: store.StrategyUploadOnly, PointsBuffer: 10000})
	without, _ := cheapestThreshold(store.Settings{UploadStrategy: store.StrategyUploadOnly, PointsBuffer: 0})
	if withBuffer-without != 10000 {
		t.Errorf("buffer not applied: %d vs %d", withBuffer, without)
	}
}

func TestProjectNextUsefulRun(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	t.Run("projects the gap at the measured rate", func(t *testing.T) {
		// 1,190 short at 119/hour is 10 hours.
		eta, ok := projectNextUsefulRun(10000, 11190, 119, now)
		if !ok {
			t.Fatal("no projection made")
		}
		if d := eta.Sub(now); math.Abs(d.Hours()-10) > 0.1 {
			t.Errorf("eta is %v away, want ~10h", d)
		}
	})

	t.Run("threshold already met", func(t *testing.T) {
		if _, ok := projectNextUsefulRun(40000, 35000, 119, now); ok {
			t.Error("projected a wait despite the threshold being met")
		}
	})

	t.Run("exactly at the threshold", func(t *testing.T) {
		if _, ok := projectNextUsefulRun(35000, 35000, 119, now); ok {
			t.Error("projected a wait when the balance exactly meets the threshold")
		}
	})

	t.Run("no rate measured yet", func(t *testing.T) {
		if _, ok := projectNextUsefulRun(1000, 35000, 0, now); ok {
			t.Error("projected without a rate; should fall back to the normal cadence")
		}
	})

	t.Run("negative rate", func(t *testing.T) {
		if _, ok := projectNextUsefulRun(1000, 35000, -5, now); ok {
			t.Error("projected from a negative rate")
		}
	})

	t.Run("capped so a bad estimate cannot strand the scheduler", func(t *testing.T) {
		// 1 point/hour against a 50,000 gap is ~5.7 years.
		eta, ok := projectNextUsefulRun(0, 50000, 1, now)
		if !ok {
			t.Fatal("no projection made")
		}
		if want := now.Add(maxSkipAhead); !eta.Equal(want) {
			t.Errorf("eta = %v, want it capped at %v", eta, want)
		}
	})
}

// TestProjectionReasonIsReadable pins that the dashboard gets a sentence a
// user can act on, not a bare timestamp.
func TestProjectionReasonIsReadable(t *testing.T) {
	got := projectionReason(4088, 35000, 118.9)
	const want = "Waiting for points: 30,912 short of the 35,000 needed for the next purchase, earning about 119/hour."
	if got != want {
		t.Errorf("reason = %q\nwant      %q", got, want)
	}
}

func TestHumanInt(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{30912, "30,912"},
		{1234567, "1,234,567"},
	}
	for _, tc := range tests {
		if got := humanInt(tc.in); got != tc.want {
			t.Errorf("humanInt(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
