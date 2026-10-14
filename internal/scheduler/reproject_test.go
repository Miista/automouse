package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/miista/automouse/internal/mamclient"
	"github.com/miista/automouse/internal/store"
)

// armProjected puts the store in the state runAndReschedule leaves behind
// after a points projection: scheduler on, next run at next, upload-only so
// the threshold is 25,000 + buffer.
func armProjected(t *testing.T, st *store.Store, next time.Time, projected bool) {
	t.Helper()
	if err := st.Update(func(state *store.State) {
		state.SchedulerOn = true
		state.Settings.UploadStrategy = store.StrategyUploadOnly
		state.Settings.PointsBuffer = 1000
		state.Accrual.PointsPerHour = 100
		state.NextRunTime = &next
		state.NextRunReason = "stale"
		state.NextRunProjected = projected
	}); err != nil {
		t.Fatal(err)
	}
}

func viewState(st *store.Store) (got store.State) {
	st.View(func(state store.State) { got = state })
	return got
}

func TestReproject(t *testing.T) {
	threshold := mamclient.MinUploadGB*mamclient.PointsPerGB + 1000

	t.Run("rewrites the shortfall from the fresh balance", func(t *testing.T) {
		s, st := newTestScheduler(t)
		far := time.Now().Add(maxSkipAhead)
		armProjected(t, st, far, true)

		s.reproject(threshold - 500) // 5h away at 100/hour

		got := viewState(st)
		if !strings.Contains(got.NextRunReason, "500 short") {
			t.Errorf("reason = %q, want it to quote the fresh 500 shortfall", got.NextRunReason)
		}
		if d := time.Until(*got.NextRunTime); d < 4*time.Hour || d > 6*time.Hour {
			t.Errorf("next run in %v, want it pulled in to ~5h", d)
		}
		if !got.NextRunProjected {
			t.Error("still a projection, but flag was cleared")
		}
	})

	t.Run("never pushes a capped run later", func(t *testing.T) {
		s, st := newTestScheduler(t)
		soon := time.Now().Add(time.Hour)
		armProjected(t, st, soon, true)

		s.reproject(0) // far beyond the cap at 100/hour

		got := viewState(st)
		if !got.NextRunTime.Equal(soon) {
			t.Errorf("next run moved to %v, want it left at %v", got.NextRunTime, soon)
		}
		if !strings.Contains(got.NextRunReason, "checking again") {
			t.Errorf("reason = %q, want it to say the run is an interim check", got.NextRunReason)
		}
	})

	t.Run("runs now once the threshold is met", func(t *testing.T) {
		s, st := newTestScheduler(t)
		armProjected(t, st, time.Now().Add(maxSkipAhead), true)

		s.reproject(threshold)

		got := viewState(st)
		if time.Now().Before(*got.NextRunTime) {
			t.Errorf("next run %v is still in the future", got.NextRunTime)
		}
		if got.NextRunReason != "" || got.NextRunProjected {
			t.Errorf("reason %q / projected %v not cleared", got.NextRunReason, got.NextRunProjected)
		}
	})

	t.Run("leaves a cadence run alone", func(t *testing.T) {
		s, st := newTestScheduler(t)
		far := time.Now().Add(maxSkipAhead)
		armProjected(t, st, far, false)

		s.reproject(threshold)

		got := viewState(st)
		if !got.NextRunTime.Equal(far) || got.NextRunReason != "stale" {
			t.Errorf("non-projected run was touched: %v %q", got.NextRunTime, got.NextRunReason)
		}
	})
}
