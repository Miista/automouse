// Package store persists automouse's full application state to a single
// JSON file under a data directory, mirroring the original Python edition's
// /app/data volume mount. All writes go through Save, which rewrites the
// whole file with 0600 permissions since it may contain a secret (the MAM
// session cookie, if entered via the UI rather than env var).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Upload/wedge purchase strategies. These are mutually exclusive — exactly
// one is active at a time, replacing what used to be three independent,
// combinable flags (buy_upload_credit / fl_only / alternate_fl_upload).
const (
	StrategyNone       = "none"
	StrategyUploadOnly = "upload_only"
	StrategyWedgeOnly  = "wedge_only"
	StrategyAlternate  = "alternate"
)

// Settings holds every user-configurable value. MamID is the MAM session
// cookie; it is a persisted secret and is masked whenever rendered to the
// API (see internal/api).
type Settings struct {
	BuyVIP                bool   `json:"buy_vip"`
	UploadStrategy        string `json:"upload_strategy"`         // upload_only | wedge_only | alternate
	AlternateNextPurchase string `json:"alternate_next_purchase"` // freeleech_wedge | upload_credit — only meaningful when UploadStrategy == alternate
	PointsBuffer          int    `json:"points_buffer"`
	MaxUploadGBPerRun     int    `json:"max_upload_gb_per_run"`
	NextRunDelayMinutes   int    `json:"next_run_delay_minutes"`
	MamID                 string `json:"mam_id"`
}

// DefaultSettings mirrors the Python edition's dataclass defaults.
func DefaultSettings() Settings {
	return Settings{
		BuyVIP:                true,
		UploadStrategy:        StrategyUploadOnly,
		AlternateNextPurchase: "freeleech_wedge",
		PointsBuffer:          10000,
		MaxUploadGBPerRun:     150,
		NextRunDelayMinutes:   60,
	}
}

// Totals tracks cumulative spend across all runs.
type Totals struct {
	CumulativeUploadGB             int `json:"cumulative_upload_gb"`
	CumulativePointsSpent          int `json:"cumulative_points_spent"`
	CumulativeFreeleechWedges      int `json:"cumulative_freeleech_wedges"`
	CumulativeFreeleechPointsSpent int `json:"cumulative_freeleech_points_spent"`
	CumulativeVIPPurchases         int `json:"cumulative_vip_purchases"`
}

// HistoryEntry records the outcome of one scheduled or manual run.
type HistoryEntry struct {
	CreatedAt       time.Time `json:"created_at"`
	StartedAt       time.Time `json:"started_at"`
	Result          string    `json:"result"`
	PointsBefore    int       `json:"points_before"`
	PointsAfter     int       `json:"points_after"`
	PointsSpent     int       `json:"points_spent"`
	UploadGB        int       `json:"upload_gb"`
	FreeleechWedges int       `json:"freeleech_wedges"`
	VIPPurchased    bool      `json:"vip_purchased"`
	DryRun          bool      `json:"dry_run"`
}

// Admin holds the single admin account (Sonarr/Radarr first-launch pattern).
type Admin struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// Auth holds session-auth state that must survive a process restart:
// the HMAC key used to sign session tokens, and the set of currently valid
// sessions (id -> expiry). Persisting this means a container
// restart/rebuild no longer logs the user out, unlike keeping it in memory
// only.
type Auth struct {
	HMACKey  []byte           `json:"hmac_key,omitempty"`
	Sessions map[string]int64 `json:"sessions,omitempty"` // sessionID -> expiry (unix seconds)
}

// AccrualSample is one observation of the bonus-point balance, taken during
// the nightly measurement window.
type AccrualSample struct {
	At     time.Time `json:"at"`
	Points int       `json:"points"`
}

// Accrual holds the measured bonus-point accrual rate and the samples it was
// derived from. MAM reports the rate on its bonus page, but that page is not
// reachable with the JSON API credential, so it is measured instead: samples
// are taken hourly between 01:00 and 04:00 local time, when the user is
// unlikely to be spending points on the site and anything AutoMouse spends is
// known. Points are consumed by purchases we do not make, so a window
// containing a decrease is discarded rather than fitted.
type Accrual struct {
	PointsPerHour float64         `json:"points_per_hour,omitempty"`
	MeasuredAt    *time.Time      `json:"measured_at,omitempty"`
	Samples       []AccrualSample `json:"samples,omitempty"`
}

// State is the full persisted document.
type State struct {
	Admin       *Admin   `json:"admin,omitempty"`
	Auth        Auth     `json:"auth"`
	Settings    Settings `json:"settings"`
	Totals      Totals   `json:"totals"`
	SchedulerOn bool     `json:"scheduler_enabled"`
	// RateLimitedUntil is set when MAM rate-limits us. While it is in the
	// future no run fires, and it is persisted so a restart cannot be used
	// (accidentally or otherwise) to bypass the backoff.
	RateLimitedUntil *time.Time `json:"rate_limited_until,omitempty"`
	Paused           bool       `json:"paused"`
	NextRunTime      *time.Time `json:"next_run_time,omitempty"`
	// NextRunReason explains why the next run is scheduled when it is —
	// "Waiting for enough points to buy anything" rather than an unexplained
	// gap. Empty means the ordinary configured cadence.
	NextRunReason string `json:"next_run_reason,omitempty"`
	// Accrual is the measured points-per-hour rate used to skip runs that
	// could not possibly afford anything.
	Accrual Accrual        `json:"accrual"`
	History []HistoryEntry `json:"history"`
}

const maxHistory = 300

// Store guards State with a mutex and persists it to dataDir/config.json.
type Store struct {
	mu      sync.RWMutex
	path    string
	dataDir string
	state   State
}

// Open loads state from dataDir/config.json, creating the directory and a
// fresh default document if none exists yet.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	s := &Store{
		path:    filepath.Join(dataDir, "config.json"),
		dataDir: dataDir,
		state: State{
			Settings: DefaultSettings(),
		},
	}
	if _, err := os.Stat(s.path); err == nil {
		raw, err := os.ReadFile(s.path)
		if err != nil {
			return nil, fmt.Errorf("reading state file: %w", err)
		}
		if err := json.Unmarshal(raw, &s.state); err != nil {
			return nil, fmt.Errorf("parsing state file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat state file: %w", err)
	} else {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// DataDir returns the directory state is persisted under.
func (s *Store) DataDir() string {
	return s.dataDir
}

// View runs fn with a read lock held over a copy of the current state.
func (s *Store) View(fn func(State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.state)
}

// Update runs fn with a write lock held, letting it mutate state in place,
// then persists the result to disk.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	if len(s.state.History) > maxHistory {
		s.state.History = s.state.History[len(s.state.History)-maxHistory:]
	}
	return s.saveLocked()
}

// saveLocked writes state to disk. Caller must hold s.mu.
func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("renaming state file: %w", err)
	}
	return nil
}
