package scheduler

import (
	"time"

	"github.com/miista/automouse/internal/mamclient"
	"github.com/miista/automouse/internal/settings"
	"github.com/miista/automouse/internal/store"
)

// The nightly measurement window. MAM recalculates seedbonus hourly, so
// samples an hour apart move by a full update; spanning three hours gives a
// baseline large enough that the per-hour figure isn't dominated by a single
// update boundary.
//
// The window is at night because the balance is also moved by the user
// spending on the MAM site, which we never observe. At 03:00 they are
// unlikely to be doing so, and anything AutoMouse itself spends is known and
// excluded, so what's left is accrual.
const (
	accrualWindowStartHour = 1 // 01:00
	accrualWindowEndHour   = 4 // 04:00
)

// minAccrualSamples is how many observations are needed before a rate is
// trusted. Two give a rate; three mean a single anomalous reading can be
// seen for what it is.
const minAccrualSamples = 3

// inAccrualWindow reports whether t falls in the nightly measurement window.
func inAccrualWindow(t time.Time) bool {
	h := t.Hour()
	return h >= accrualWindowStartHour && h < accrualWindowEndHour
}

// sampleAccrual takes one balance observation during the measurement window
// and recomputes the rate once enough samples exist. It costs two MAM
// requests and makes no purchases.
//
// Samples from a previous night are discarded rather than accumulated: the
// rate depends on what is currently being seeded, which changes between
// nights, so a stale sample would drag the estimate toward a profile that no
// longer holds.
func (s *Scheduler) sampleAccrual() {
	if s.rateLimited() {
		return
	}

	var cfg store.Settings
	s.store.View(func(st store.State) {
		cfg = settings.Resolve(st.Settings).Settings
	})
	if cfg.MamID == "" {
		return
	}

	client := mamclient.New(mamclient.Secret(cfg.MamID))
	uid, err := client.UserID()
	if err != nil || uid == "" {
		var rl *mamclient.RateLimitError
		if asRateLimit(err, &rl) {
			s.noteRateLimit(rl)
		}
		return
	}
	points, err := client.SeedBonus(uid)
	if err != nil {
		var rl *mamclient.RateLimitError
		if asRateLimit(err, &rl) {
			s.noteRateLimit(rl)
		}
		return
	}
	s.recordPoints(points)

	now := time.Now()
	_ = s.store.Update(func(st *store.State) {
		samples := st.Accrual.Samples
		// Drop anything from an earlier night.
		if len(samples) > 0 && !sameNight(samples[len(samples)-1].At, now) {
			samples = nil
		}
		samples = append(samples, store.AccrualSample{At: now, Points: points})
		st.Accrual.Samples = samples

		if rate, ok := rateFrom(samples); ok {
			st.Accrual.PointsPerHour = rate
			measured := now
			st.Accrual.MeasuredAt = &measured
		}
	})

	s.log.Debug().Int("points", points).Msg("accrual sample taken")
}

// sameNight reports whether two timestamps belong to the same measurement
// window. The window never spans midnight, so a date comparison suffices.
func sameNight(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// rateFrom derives points-per-hour from the night's samples, using the first
// and last to span the widest baseline available.
//
// A decrease means points were spent during the window — by the user on the
// site, or by a run of ours — and says nothing about accrual, so no rate is
// reported and the previous one stands. That is the right call precisely
// because a spend doesn't change the rate: only the balance the projection
// starts from, which is re-read on every run anyway.
func rateFrom(samples []store.AccrualSample) (float64, bool) {
	if len(samples) < minAccrualSamples {
		return 0, false
	}
	first, last := samples[0], samples[len(samples)-1]
	gained := last.Points - first.Points
	if gained <= 0 {
		return 0, false
	}
	hours := last.At.Sub(first.At).Hours()
	if hours <= 0 {
		return 0, false
	}
	return float64(gained) / hours, true
}
