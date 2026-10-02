package scheduler

import (
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/miista/automouse/internal/mamclient"
	"github.com/miista/automouse/internal/store"
)

func newTestScheduler(t *testing.T) (*Scheduler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	return New(st, zerolog.Nop()), st
}

func TestNoteRateLimitSetsWindowAndPushesNextRun(t *testing.T) {
	s, st := newTestScheduler(t)

	// Arm a run for one minute from now, well inside the backoff we're
	// about to record.
	soon := time.Now().Add(time.Minute)
	if err := st.Update(func(state *store.State) {
		state.SchedulerOn = true
		state.NextRunTime = &soon
	}); err != nil {
		t.Fatal(err)
	}

	until := s.noteRateLimit(&mamclient.RateLimitError{RetryAfter: time.Hour, StatusCode: 429})

	var got store.State
	st.View(func(state store.State) { got = state })

	if got.RateLimitedUntil == nil {
		t.Fatal("RateLimitedUntil not set")
	}
	if !got.RateLimitedUntil.Equal(until) {
		t.Errorf("RateLimitedUntil = %v, want %v", got.RateLimitedUntil, until)
	}
	if got.NextRunTime.Before(until) {
		t.Errorf("next run %v is inside the backoff window ending %v", got.NextRunTime, until)
	}
}

// TestNoteRateLimitKeepsLaterNextRun guards against pulling a run *forward*:
// if the next run was already scheduled past the backoff, it stays put.
func TestNoteRateLimitKeepsLaterNextRun(t *testing.T) {
	s, st := newTestScheduler(t)

	far := time.Now().Add(6 * time.Hour)
	if err := st.Update(func(state *store.State) {
		state.SchedulerOn = true
		state.NextRunTime = &far
	}); err != nil {
		t.Fatal(err)
	}

	s.noteRateLimit(&mamclient.RateLimitError{RetryAfter: time.Minute, StatusCode: 429})

	var got store.State
	st.View(func(state store.State) { got = state })
	if !got.NextRunTime.Equal(far) {
		t.Errorf("next run moved to %v, want it left at %v", got.NextRunTime, far)
	}
}

func TestRateLimitedReportsActiveWindow(t *testing.T) {
	s, st := newTestScheduler(t)

	if s.rateLimited() {
		t.Error("rate limited with no window set")
	}

	future := time.Now().Add(time.Hour)
	if err := st.Update(func(state *store.State) { state.RateLimitedUntil = &future }); err != nil {
		t.Fatal(err)
	}
	if !s.rateLimited() {
		t.Error("not rate limited despite an active window")
	}

	past := time.Now().Add(-time.Hour)
	if err := st.Update(func(state *store.State) { state.RateLimitedUntil = &past }); err != nil {
		t.Fatal(err)
	}
	if s.rateLimited() {
		t.Error("still rate limited after the window expired")
	}
}

func TestClearRateLimitOnlyDropsExpiredWindows(t *testing.T) {
	s, st := newTestScheduler(t)

	future := time.Now().Add(time.Hour)
	if err := st.Update(func(state *store.State) { state.RateLimitedUntil = &future }); err != nil {
		t.Fatal(err)
	}
	s.clearRateLimit()

	var got store.State
	st.View(func(state store.State) { got = state })
	if got.RateLimitedUntil == nil {
		t.Error("an active window was cleared")
	}

	past := time.Now().Add(-time.Minute)
	if err := st.Update(func(state *store.State) { state.RateLimitedUntil = &past }); err != nil {
		t.Fatal(err)
	}
	s.clearRateLimit()

	st.View(func(state store.State) { got = state })
	if got.RateLimitedUntil != nil {
		t.Errorf("expired window not cleared: %v", got.RateLimitedUntil)
	}
}

// TestRunNowRefusesWhileRateLimited is the behaviour that matters most: a
// manual trigger is the likeliest way for someone to keep hitting a limit
// they've already hit.
func TestRunNowRefusesWhileRateLimited(t *testing.T) {
	s, st := newTestScheduler(t)

	future := time.Now().Add(time.Hour)
	if err := st.Update(func(state *store.State) { state.RateLimitedUntil = &future }); err != nil {
		t.Fatal(err)
	}

	started, reason := s.RunNow(false)
	if started {
		t.Error("RunNow started a run during a rate-limit backoff")
	}
	if reason == "" {
		t.Error("RunNow gave no reason for refusing")
	}
}

// TestRateLimitSurvivesRestart pins that the window is persisted: restarting
// the container must not be a way to bypass a backoff.
func TestRateLimitSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, zerolog.Nop())
	until := s.noteRateLimit(&mamclient.RateLimitError{RetryAfter: time.Hour, StatusCode: 429})

	// Reopen from the same directory, as a restarted process would.
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2 := New(reopened, zerolog.Nop())

	if !s2.rateLimited() {
		t.Fatal("backoff did not survive a restart")
	}
	var got store.State
	reopened.View(func(state store.State) { got = state })
	if got.RateLimitedUntil == nil || !got.RateLimitedUntil.Equal(until) {
		t.Errorf("RateLimitedUntil = %v after restart, want %v", got.RateLimitedUntil, until)
	}
}
