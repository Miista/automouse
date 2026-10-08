// Package scheduler runs the MAM spend automation on a timer and exposes
// start/pause/run-now controls. Pausing is a real state transition — the
// background ticker keeps polling every tick, but never fires the
// automation while paused, so "stop" always does what a user expects.
package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/miista/automouse/internal/mamclient"
	"github.com/miista/automouse/internal/settings"
	"github.com/miista/automouse/internal/store"
)

const pollInterval = 5 * time.Second

// pointsRefreshInterval controls a separate, independent ticker that keeps
// the dashboard's points balance current even while the scheduler itself
// is disabled or paused — a user watching the balance shouldn't see a
// stale number just because automated spending is turned off.
const pointsRefreshInterval = 15 * time.Minute

// Scheduler owns the background run loop.
type Scheduler struct {
	store *store.Store
	log   zerolog.Logger

	mu      sync.Mutex
	running bool // true while an automation run is actively executing

	// lastPoints is the most recently observed points balance. Kept in
	// memory only, deliberately not persisted — a restart just means it's
	// unknown until the next successful run.
	lastPoints int
	havePoints bool
}

// New creates a scheduler bound to st.
func New(st *store.Store, log zerolog.Logger) *Scheduler {
	return &Scheduler{store: st, log: log.With().Str("component", "scheduler").Logger()}
}

// Start begins the background polling loop. Call once at startup.
func (s *Scheduler) Start() {
	go s.loop()
	go s.pointsRefreshLoop()
	go s.accrualLoop()
	s.RefreshPoints()
}

// pointsRefreshLoop keeps the dashboard's points balance current on a
// fixed interval, regardless of whether the scheduler is enabled or
// paused — unlike the main loop() ticker, this never checks SchedulerOn.
func (s *Scheduler) pointsRefreshLoop() {
	ticker := time.NewTicker(pointsRefreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		if s.rateLimited() {
			continue
		}
		s.RefreshPoints()
	}
}

// accrualLoop takes one balance sample per hour while inside the nightly
// measurement window, and nothing at all outside it. Checking every ten
// minutes rather than scheduling precisely keeps it robust across restarts
// and clock changes; the lastSample guard is what limits it to one sample
// per hour.
func (s *Scheduler) accrualLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	var lastSample time.Time
	for range ticker.C {
		now := time.Now()
		if !inAccrualWindow(now) {
			continue
		}
		if !lastSample.IsZero() && now.Sub(lastSample) < time.Hour {
			continue
		}
		lastSample = now
		s.sampleAccrual()
	}
}

func (s *Scheduler) loop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		due := false
		s.store.View(func(st store.State) {
			if st.RateLimitedUntil != nil && time.Now().Before(*st.RateLimitedUntil) {
				return
			}
			due = st.SchedulerOn && !st.Paused && st.NextRunTime != nil && !time.Now().Before(*st.NextRunTime)
		})
		if !due {
			continue
		}
		s.mu.Lock()
		alreadyRunning := s.running
		if !alreadyRunning {
			s.running = true
		}
		s.mu.Unlock()
		if alreadyRunning {
			continue
		}
		go s.runAndReschedule(false)
	}
}

// asRateLimit is errors.As specialised to *mamclient.RateLimitError, so the
// several call sites that only care "was this a rate limit?" read as one
// line.
func asRateLimit(err error, target **mamclient.RateLimitError) bool {
	return errors.As(err, target)
}

// rateLimited reports whether a MAM rate-limit backoff is currently in
// effect.
func (s *Scheduler) rateLimited() bool {
	limited := false
	s.store.View(func(st store.State) {
		limited = st.RateLimitedUntil != nil && time.Now().Before(*st.RateLimitedUntil)
	})
	return limited
}

// noteRateLimit records a backoff window and pushes the next run out past
// it. Returns the time runs resume. Both values are persisted: the window so
// a restart can't bypass it, and NextRunTime so the dashboard shows the real
// next run rather than one that will be skipped.
func (s *Scheduler) noteRateLimit(err *mamclient.RateLimitError) time.Time {
	until := time.Now().Add(err.RetryAfter)
	_ = s.store.Update(func(st *store.State) {
		st.RateLimitedUntil = &until
		if st.NextRunTime == nil || st.NextRunTime.Before(until) {
			next := until
			st.NextRunTime = &next
		}
	})
	s.log.Warn().
		Dur("retry_after", err.RetryAfter).
		Time("resumes_at", until).
		Int("status", err.StatusCode).
		Msg("rate limited by MAM, backing off")
	return until
}

// clearRateLimit drops an expired backoff so state doesn't carry a stale
// timestamp around.
func (s *Scheduler) clearRateLimit() {
	_ = s.store.Update(func(st *store.State) {
		if st.RateLimitedUntil != nil && !time.Now().Before(*st.RateLimitedUntil) {
			st.RateLimitedUntil = nil
		}
	})
}

// StartSchedule enables the scheduler and arms the next run.
func (s *Scheduler) StartSchedule() error {
	return s.store.Update(func(st *store.State) {
		delay := time.Duration(st.Settings.NextRunDelayMinutes) * time.Minute
		next := time.Now().Add(delay)
		st.SchedulerOn = true
		st.Paused = false
		st.NextRunTime = &next
	})
}

// Pause disables the scheduler. In-flight runs finish, but no new run will
// be triggered until StartSchedule is called again.
func (s *Scheduler) Pause() error {
	return s.store.Update(func(st *store.State) {
		st.Paused = true
		st.SchedulerOn = false
	})
}

// RunNow triggers an immediate out-of-band run. It refuses while a run is
// already executing, and while a MAM rate-limit backoff is in effect —
// a manual trigger is exactly the thing most likely to be used repeatedly
// against a limit, which is what prolongs it. reason is empty on success and
// otherwise explains the refusal for display.
func (s *Scheduler) RunNow(flOnlyOverride bool) (started bool, reason string) {
	var until time.Time
	limited := false
	s.store.View(func(st store.State) {
		if st.RateLimitedUntil != nil && time.Now().Before(*st.RateLimitedUntil) {
			limited, until = true, *st.RateLimitedUntil
		}
	})
	if limited {
		return false, fmt.Sprintf("Rate limited by MAM. Runs resume %s.", until.Format("15:04 on 2 Jan"))
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false, "A run is already in progress."
	}
	s.running = true
	s.mu.Unlock()
	go s.runAndReschedule(flOnlyOverride)
	return true, ""
}

// RunDryNow triggers an immediate, one-off dry run: it fetches the real
// balance and reports what would be purchased, but never calls a buy
// endpoint. This is a manual, on-demand action only — the scheduler itself
// never does a dry run on its own, and a dry run never reschedules the next
// real run or updates cumulative totals.
func (s *Scheduler) RunDryNow() (started bool, reason string) {
	var until time.Time
	limited := false
	s.store.View(func(st store.State) {
		if st.RateLimitedUntil != nil && time.Now().Before(*st.RateLimitedUntil) {
			limited, until = true, *st.RateLimitedUntil
		}
	})
	if limited {
		return false, fmt.Sprintf("Rate limited by MAM. Runs resume %s.", until.Format("15:04 on 2 Jan"))
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false, "A run is already in progress."
	}
	s.running = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		s.runOnce(false, true)
	}()
	return true, ""
}

// IsRunning reports whether an automation pass is currently executing.
func (s *Scheduler) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// recordPoints stores the most recently observed points balance.
func (s *Scheduler) recordPoints(points int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPoints = points
	s.havePoints = true
}

// PointsStatus reports the most recently observed points balance. ok is
// false until at least one successful run has completed.
func (s *Scheduler) PointsStatus() (currentPoints int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPoints, s.havePoints
}

// RefreshPoints does a single read-only balance check against MAM — no
// purchases, no history entry, no totals update. It exists so the current
// balance can be shown promptly at two points where nothing has actually
// run yet: on process startup, and right after a new Mam Session_ID is
// saved. It runs in its own goroutine and silently no-ops on any error
// (invalid/missing cookie, network failure) since it's a convenience
// refresh, not something that should surface as a failed "run".
func (s *Scheduler) RefreshPoints() {
	go func() {
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
			// Errors here are otherwise deliberately silent (this is a
			// convenience refresh, not a run), but a rate limit must still
			// be recorded or the backoff never starts and this keeps
			// firing into the limit.
			var rl *mamclient.RateLimitError
			if errors.As(err, &rl) {
				s.noteRateLimit(rl)
			}
			return
		}
		points, err := client.SeedBonus(uid)
		if err != nil {
			var rl *mamclient.RateLimitError
			if errors.As(err, &rl) {
				s.noteRateLimit(rl)
			}
			return
		}
		s.recordPoints(points)
	}()
}

func (s *Scheduler) runAndReschedule(flOnlyOverride bool) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	s.runOnce(flOnlyOverride, false)

	_ = s.store.Update(func(st *store.State) {
		if !st.SchedulerOn || st.Paused {
			return
		}
		now := time.Now()
		delay := time.Duration(st.Settings.NextRunDelayMinutes) * time.Minute
		next := now.Add(delay)
		reason := ""

		// Skip ahead when the balance cannot reach the cheapest purchase by
		// the next scheduled run: every run before then is a guaranteed
		// no-op costing MAM requests to discover nothing. The balance is
		// re-read on each run, so a projection made optimistic by site
		// spending simply gets recomputed from the lower balance next time.
		cfg := settings.Resolve(st.Settings).Settings
		if threshold, ok := cheapestThreshold(cfg); ok {
			if balance, have := s.PointsStatus(); have {
				if eta, ok := projectNextUsefulRun(balance, threshold, st.Accrual.PointsPerHour, now); ok && eta.After(next) {
					next = eta
					reason = projectionReason(balance, threshold, st.Accrual.PointsPerHour, eta)
				}
			}
		}

		// Never schedule back inside an active backoff window: runOnce may
		// have just set one, and the normal delay is typically shorter than
		// the retry-after.
		if st.RateLimitedUntil != nil && next.Before(*st.RateLimitedUntil) {
			next = *st.RateLimitedUntil
			reason = "Backing off after MyAnonamouse rate limited us."
		}
		st.NextRunTime = &next
		st.NextRunReason = reason
	})
}

func (s *Scheduler) runOnce(flOnlyOverride, dryRun bool) {
	startedAt := time.Now()
	entry := store.HistoryEntry{StartedAt: startedAt, CreatedAt: startedAt, Result: "Completed", DryRun: dryRun}

	s.clearRateLimit()

	var resolved settings.Resolved
	s.store.View(func(st store.State) {
		resolved = settings.Resolve(st.Settings)
	})
	cfg := resolved.Settings

	if cfg.MamID == "" {
		entry.Result = "No Mam Session_ID configured."
		s.log.Warn().Msg(entry.Result)
		s.appendHistory(entry)
		return
	}

	client := mamclient.New(mamclient.Secret(cfg.MamID))

	uid, err := client.UserID()
	if err != nil {
		// A rate limit is not a bad cookie, and saying so would send the
		// user off to re-check a session that is perfectly fine.
		var rl *mamclient.RateLimitError
		if errors.As(err, &rl) {
			until := s.noteRateLimit(rl)
			entry.Result = fmt.Sprintf("Rate limited by MAM. Runs resume %s.", until.Format("15:04 on 2 Jan"))
			s.appendHistory(entry)
			return
		}
		entry.Result = "Session invalid. Check Mam Session_ID."
		s.log.Warn().Err(err).Msg(entry.Result)
		s.appendHistory(entry)
		return
	}
	if uid == "" {
		entry.Result = "Session invalid. Check Mam Session_ID."
		s.log.Warn().Msg(entry.Result)
		s.appendHistory(entry)
		return
	}

	points, err := client.SeedBonus(uid)
	if err != nil {
		var rl *mamclient.RateLimitError
		if errors.As(err, &rl) {
			until := s.noteRateLimit(rl)
			entry.Result = fmt.Sprintf("Rate limited by MAM. Runs resume %s.", until.Format("15:04 on 2 Jan"))
			s.appendHistory(entry)
			return
		}
		entry.Result = "Failed to retrieve bonus points."
		s.log.Warn().Err(err).Msg(entry.Result)
		s.appendHistory(entry)
		return
	}
	s.recordPoints(points)
	initialPoints := points
	entry.PointsBefore = points

	// skipped collects why an enabled purchase didn't happen, so a dry run
	// that buys nothing can say why instead of leaving the user to redo
	// the buffer arithmetic by hand.
	var skipped []string

	vipPurchased := false
	if cfg.BuyVIP {
		if expiry, err := client.VIPExpiry(); err == nil {
			remaining := time.Until(expiry)
			if remaining.Hours()/24 > mamclient.VIPRenewDays {
				skipped = append(skipped, fmt.Sprintf("VIP not due (%d days left)", int(remaining.Hours()/24)))
			} else if dryRun {
				vipPurchased = true
			} else if err := client.BuyVIP(); err == nil {
				if newPoints, err := client.SeedBonus(uid); err == nil && newPoints < points {
					vipPurchased = true
					points = newPoints
				}
			} else {
				s.log.Warn().Err(err).Msg("VIP purchase request failed")
			}
		} else {
			s.log.Warn().Err(err).Msg("failed to check VIP expiry")
		}
	}

	// UploadStrategy is a mutually-exclusive choice between three purchase
	// modes; flOnlyOverride (from a manual "FL-only" run trigger) forces
	// wedge-buying for this run regardless of the configured strategy.
	targetsWedge := cfg.UploadStrategy == store.StrategyAlternate && cfg.AlternateNextPurchase == "freeleech_wedge"
	shouldBuyWedge := cfg.UploadStrategy == store.StrategyWedgeOnly || flOnlyOverride || targetsWedge
	shouldBuyUpload := !shouldBuyWedge && (cfg.UploadStrategy == store.StrategyUploadOnly || cfg.UploadStrategy == store.StrategyAlternate)

	wedgesPurchased := 0
	if shouldBuyWedge && points < mamclient.FLWedgeCost+cfg.PointsBuffer {
		skipped = append(skipped, fmt.Sprintf("freeleech wedge needs %d points (%d + %d buffer), have %d",
			mamclient.FLWedgeCost+cfg.PointsBuffer, mamclient.FLWedgeCost, cfg.PointsBuffer, points))
	}
	if shouldBuyWedge && points >= mamclient.FLWedgeCost+cfg.PointsBuffer {
		if dryRun {
			wedgesPurchased = 1
			points -= mamclient.FLWedgeCost
		} else {
			before := points
			if err := client.BuyFreeleechWedge(); err != nil {
				s.log.Warn().Err(err).Msg("freeleech wedge purchase failed")
			} else if newPoints, err := client.SeedBonus(uid); err == nil && before-newPoints >= mamclient.FLWedgeCost {
				wedgesPurchased = 1
				points = newPoints
				if cfg.UploadStrategy == store.StrategyAlternate {
					s.setAlternateTarget("upload_credit")
				}
			}
		}
	}

	uploadGB := 0
	if shouldBuyUpload {
		available := points - cfg.PointsBuffer
		if available < 0 {
			available = 0
		}
		affordableGB := available / mamclient.PointsPerGB
		gb := affordableGB
		if cfg.MaxUploadGBPerRun > 0 && gb > cfg.MaxUploadGBPerRun {
			gb = cfg.MaxUploadGBPerRun
		}
		// MAM rejects automated upload-credit purchases below 50 GiB
		// ("Automated spenders are limited to buying at least 50 GB of
		// upload at a time, due to log spam") — confirmed directly against
		// the live API. This floor is MAM's, not a design choice of ours,
		// so it's enforced here regardless of the configured cap.
		if gb < mamclient.MinUploadGB {
			if cfg.MaxUploadGBPerRun > 0 && cfg.MaxUploadGBPerRun < mamclient.MinUploadGB {
				skipped = append(skipped, fmt.Sprintf("upload cap of %d GiB per run is below MAM's %d GiB minimum",
					cfg.MaxUploadGBPerRun, mamclient.MinUploadGB))
			} else {
				skipped = append(skipped, fmt.Sprintf("upload credit: %d GiB affordable after %d point buffer, below MAM's %d GiB minimum (needs %d points)",
					affordableGB, cfg.PointsBuffer, mamclient.MinUploadGB, mamclient.MinUploadGB*mamclient.PointsPerGB+cfg.PointsBuffer))
			}
		}
		if gb >= mamclient.MinUploadGB {
			if dryRun {
				uploadGB = gb
				points -= gb * mamclient.PointsPerGB
			} else {
				before := points
				if err := client.BuyUploadCredit(gb); err != nil {
					s.log.Warn().Err(err).Msg("upload credit purchase failed")
				} else if newPoints, err := client.SeedBonus(uid); err != nil {
					// The purchase request itself succeeded, but we can't
					// confirm the outcome — MAM is the source of truth for the
					// balance, not our own arithmetic, so an unconfirmed
					// purchase must not be recorded as GB bought or points
					// spent. Flag it loudly: this is the one scenario where
					// money may have moved with no local record of it.
					s.log.Error().Err(err).Msg("upload credit purchase sent, but balance re-check failed afterward — unable to confirm outcome, points may have been spent without being recorded")
				} else if newPoints < before {
					points = newPoints
					uploadGB = gb
					if cfg.UploadStrategy == store.StrategyAlternate {
						s.setAlternateTarget("freeleech_wedge")
					}
				} else {
					s.log.Warn().Msg("upload credit purchase request succeeded but balance did not decrease — treating as failed")
				}
			}
		}
	}

	// Always finish with a fresh balance check rather than trusting our own
	// running arithmetic through the purchases above — MAM's own reported
	// balance is the only honest source of truth for what actually
	// happened this run, regardless of how each individual purchase step
	// verified (or failed to verify) itself. A dry run never called a buy
	// endpoint, so re-checking here would just overwrite our projected
	// points with the real (unchanged) balance.
	if !dryRun {
		if finalPoints, err := client.SeedBonus(uid); err != nil {
			s.log.Error().Err(err).Msg("final balance re-check failed — recorded totals for this run reflect our own bookkeeping, not a confirmed MAM balance")
		} else {
			points = finalPoints
		}
	}

	pointsSpent := initialPoints - points
	if pointsSpent < 0 {
		pointsSpent = 0
	}
	entry.PointsAfter = points
	entry.PointsSpent = pointsSpent
	entry.UploadGB = uploadGB
	entry.FreeleechWedges = wedgesPurchased
	entry.VIPPurchased = vipPurchased

	if dryRun {
		entry.Result = dryRunSummary(vipPurchased, uploadGB, wedgesPurchased, skipped)
		s.appendHistory(entry)
		s.log.Info().
			Int("points_spent", pointsSpent).
			Int("upload_gb", uploadGB).
			Int("wedges", wedgesPurchased).
			Bool("vip", vipPurchased).
			Msg("dry run complete")
		return
	}

	s.recordPoints(points)
	s.updateTotals(uploadGB, pointsSpent, wedgesPurchased, vipPurchased)
	s.appendHistory(entry)
	s.log.Info().
		Int("points_spent", pointsSpent).
		Int("upload_gb", uploadGB).
		Int("wedges", wedgesPurchased).
		Bool("vip", vipPurchased).
		Msg("automation run complete")
}

// dryRunSummary describes, in prose, what a dry run determined it would
// have purchased, so the history table's Result column doesn't just say
// "Completed" for a run that bought nothing for real. When nothing would
// be bought, skipped explains why.
func dryRunSummary(vip bool, uploadGB, wedges int, skipped []string) string {
	var parts []string
	if vip {
		parts = append(parts, "VIP renewal")
	}
	if uploadGB > 0 {
		parts = append(parts, fmt.Sprintf("%d GiB upload credit", uploadGB))
	}
	if wedges > 0 {
		parts = append(parts, fmt.Sprintf("%d freeleech wedge", wedges))
	}
	if len(parts) == 0 {
		if len(skipped) == 0 {
			return "Would buy nothing this run: no purchase type is enabled."
		}
		return "Would buy nothing this run: " + strings.Join(skipped, "; ") + "."
	}
	return "Would buy " + strings.Join(parts, ", ") + "."
}

func (s *Scheduler) setAlternateTarget(next string) {
	_ = s.store.Update(func(st *store.State) {
		st.Settings.AlternateNextPurchase = next
	})
}

func (s *Scheduler) updateTotals(gb, pointsSpent, wedges int, vip bool) {
	_ = s.store.Update(func(st *store.State) {
		st.Totals.CumulativeUploadGB += gb
		st.Totals.CumulativePointsSpent += pointsSpent
		st.Totals.CumulativeFreeleechWedges += wedges
		st.Totals.CumulativeFreeleechPointsSpent += wedges * mamclient.FLWedgeCost
		if vip {
			st.Totals.CumulativeVIPPurchases++
		}
	})
}

func (s *Scheduler) appendHistory(entry store.HistoryEntry) {
	_ = s.store.Update(func(st *store.State) {
		st.History = append(st.History, entry)
	})
}
