package scheduler

import (
	"fmt"
	"strconv"
	"time"

	"github.com/miista/automouse/internal/mamclient"
	"github.com/miista/automouse/internal/store"
)

// maxSkipAhead caps how far ahead a projection may push the next run. The
// accrual rate is measured, not reported by MAM, and the balance can move
// for reasons we never observe (the user spending on the site). A cap means
// a bad estimate costs one needless wake-up rather than stranding the
// scheduler for weeks.
const maxSkipAhead = 24 * time.Hour

// cheapestThreshold returns the smallest balance at which the configured
// run could actually buy something, and whether any purchase is enabled at
// all.
//
// VIP is deliberately not a candidate. The API only accepts duration="max",
// which fills to the 90-day cap, so its cost varies with how far below the
// cap the account currently sits and cannot be known ahead of time.
// Excluding it means the threshold is never overestimated: if VIP is
// enabled and affordable sooner than any purchase, the scheduler simply
// wakes at the purchase threshold and attempts VIP then, exactly as it does
// today. Being too eager costs one request; being too lazy risks VIP
// lapsing.
func cheapestThreshold(cfg store.Settings) (threshold int, ok bool) {
	switch cfg.UploadStrategy {
	case store.StrategyUploadOnly:
		return mamclient.MinUploadGB*mamclient.PointsPerGB + cfg.PointsBuffer, true
	case store.StrategyWedgeOnly:
		return mamclient.FLWedgeCost + cfg.PointsBuffer, true
	case store.StrategyAlternate:
		// Either could be next, so the cheaper of the two is the point at
		// which a run stops being a guaranteed no-op.
		upload := mamclient.MinUploadGB*mamclient.PointsPerGB + cfg.PointsBuffer
		wedge := mamclient.FLWedgeCost + cfg.PointsBuffer
		if upload < wedge {
			return upload, true
		}
		return wedge, true
	default:
		// StrategyNone: nothing is ever bought, so there is no threshold to
		// wait for and the VIP check alone decides whether a run is useful.
		return 0, false
	}
}

// projectionReason renders the skip as something a user can act on: how
// short we are, how fast that gap is closing, and when it closes. Without it
// the dashboard would show a next run hours or days out with no explanation,
// which reads like the scheduler has stalled.
//
// The ETA is spelled out rather than left to the "Next run" line above it,
// because the same sentence is written into run history, where it stands on
// its own with no surrounding context.
func projectionReason(balance, threshold int, ratePerHour float64, eta time.Time) string {
	return fmt.Sprintf(
		"Waiting for points: %s short of the %s needed for the next purchase, earning about %.0f/hour — expected %s.",
		humanInt(threshold-balance), humanInt(threshold), ratePerHour, eta.Format("15:04 on 2 Jan"),
	)
}

// humanInt formats a count with thousands separators, since the numbers
// involved run to five and six digits.
func humanInt(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// projectNextUsefulRun reports when the balance is expected to first reach
// threshold, given the measured accrual rate. ok is false when no useful
// projection can be made — no rate measured yet, a non-positive rate, or a
// threshold already met — in which case the caller should fall back to the
// configured cadence rather than skipping.
func projectNextUsefulRun(balance, threshold int, ratePerHour float64, now time.Time) (eta time.Time, ok bool) {
	if ratePerHour <= 0 {
		return time.Time{}, false
	}
	short := threshold - balance
	if short <= 0 {
		return time.Time{}, false
	}
	hours := float64(short) / ratePerHour
	eta = now.Add(time.Duration(hours * float64(time.Hour)))
	if capped := now.Add(maxSkipAhead); eta.After(capped) {
		eta = capped
	}
	return eta, true
}
