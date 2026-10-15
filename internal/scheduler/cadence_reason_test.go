package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/miista/automouse/internal/store"
)

func TestCadenceReason(t *testing.T) {
	uploadOnly := store.Settings{UploadStrategy: store.StrategyUploadOnly}

	tests := []struct {
		name  string
		cfg   store.Settings
		rate  float64
		delay time.Duration
		want  string
	}{
		{"unmeasured rate explains the wait", uploadOnly, 0, time.Hour, "Earn rate not measured yet"},
		{"unmeasured rate names the interval", uploadOnly, 0, 90 * time.Minute, "every 90 minutes"},
		{"measured rate is a regular check", uploadOnly, 260, time.Hour, "Regular check every hour."},
		{"whole hours are pluralised", uploadOnly, 260, 2 * time.Hour, "every 2 hours."},
		{"no purchases enabled", store.Settings{UploadStrategy: store.StrategyNone}, 260, time.Hour, "no upload or wedge purchases"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cadenceReason(tt.cfg, tt.rate, tt.delay)
			if !strings.Contains(got, tt.want) {
				t.Errorf("cadenceReason = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestStartScheduleExplainsCadence(t *testing.T) {
	s, st := newTestScheduler(t)
	if err := st.Update(func(state *store.State) {
		state.Settings.UploadStrategy = store.StrategyUploadOnly
		state.Settings.NextRunDelayMinutes = 60
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.StartSchedule(); err != nil {
		t.Fatal(err)
	}

	got := viewState(st)
	if got.NextRunProjected {
		t.Error("Start armed a projected run, want a cadence run")
	}
	if !strings.Contains(got.NextRunReason, "Earn rate not measured yet") {
		t.Errorf("reason = %q, want the unmeasured-rate explanation", got.NextRunReason)
	}
}
