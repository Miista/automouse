// Package scheduler runs the MAM spend automation on a timer and exposes
// start/pause/run-now controls. Pausing is a real state transition — the
// background ticker keeps polling every tick, but never fires the
// automation while paused, so "stop" always does what a user expects.
package scheduler

import (
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/miista/mam-spender/internal/mamclient"
	"github.com/miista/mam-spender/internal/settings"
	"github.com/miista/mam-spender/internal/store"
)

const pollInterval = 5 * time.Second

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
	s.RefreshPoints()
}

func (s *Scheduler) loop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		due := false
		s.store.View(func(st store.State) {
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

// RunNow triggers an immediate out-of-band run, unless one is already
// executing.
func (s *Scheduler) RunNow(flOnlyOverride bool) bool {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false
	}
	s.running = true
	s.mu.Unlock()
	go s.runAndReschedule(flOnlyOverride)
	return true
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
			return
		}
		points, err := client.SeedBonus(uid)
		if err != nil {
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

	s.runOnce(flOnlyOverride)

	_ = s.store.Update(func(st *store.State) {
		if st.SchedulerOn && !st.Paused {
			delay := time.Duration(st.Settings.NextRunDelayMinutes) * time.Minute
			next := time.Now().Add(delay)
			st.NextRunTime = &next
		}
	})
}

func (s *Scheduler) runOnce(flOnlyOverride bool) {
	startedAt := time.Now()
	entry := store.HistoryEntry{StartedAt: startedAt, CreatedAt: startedAt, Result: "Completed"}

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
	if err != nil || uid == "" {
		entry.Result = "Session invalid. Check Mam Session_ID."
		s.log.Warn().Err(err).Msg(entry.Result)
		s.appendHistory(entry)
		return
	}

	points, err := client.SeedBonus(uid)
	if err != nil {
		entry.Result = "Failed to retrieve bonus points."
		s.log.Warn().Err(err).Msg(entry.Result)
		s.appendHistory(entry)
		return
	}
	s.recordPoints(points)
	initialPoints := points
	entry.PointsBefore = points

	vipPurchased := false
	if cfg.BuyVIP {
		if expiry, err := client.VIPExpiry(); err == nil {
			remaining := time.Until(expiry)
			if remaining.Hours()/24 <= mamclient.VIPRenewDays {
				if err := client.BuyVIP(); err == nil {
					if newPoints, err := client.SeedBonus(uid); err == nil && newPoints < points {
						vipPurchased = true
						points = newPoints
					}
				} else {
					s.log.Warn().Err(err).Msg("VIP purchase request failed")
				}
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
	if shouldBuyWedge && points >= mamclient.FLWedgeCost+cfg.PointsBuffer {
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
		if gb >= mamclient.MinUploadGB {
			cost := gb * mamclient.PointsPerGB
			if err := client.BuyUploadCredit(gb); err != nil {
				s.log.Warn().Err(err).Msg("upload credit purchase failed")
			} else {
				points -= cost
				uploadGB = gb
				if cfg.UploadStrategy == store.StrategyAlternate {
					s.setAlternateTarget("freeleech_wedge")
				}
			}
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

	s.updateTotals(uploadGB, pointsSpent, wedgesPurchased, vipPurchased)
	s.appendHistory(entry)
	s.log.Info().
		Int("points_spent", pointsSpent).
		Int("upload_gb", uploadGB).
		Int("wedges", wedgesPurchased).
		Bool("vip", vipPurchased).
		Msg("automation run complete")
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
