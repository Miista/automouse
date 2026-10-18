// Package api implements the HTTP handlers for automouse: settings (with
// env-override reporting) and scheduler control. Sign-in is not handled here:
// the authy gate wraps the whole mux in main.
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/miista/automouse/internal/mamclient"
	"github.com/miista/automouse/internal/scheduler"
	"github.com/miista/automouse/internal/settings"
	"github.com/miista/automouse/internal/store"
)

// Server wires the store and scheduler into HTTP handlers.
type Server struct {
	store     *store.Store
	scheduler *scheduler.Scheduler
	log       zerolog.Logger
}

// New builds a Server.
func New(st *store.Store, sc *scheduler.Scheduler, log zerolog.Logger) *Server {
	return &Server{
		store:     st,
		scheduler: sc,
		log:       log.With().Str("component", "api").Logger(),
	}
}

// Routes registers all handlers on mux. None of them check who is calling:
// the authy gate in front of the mux has already decided, on every route
// except /healthz.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/start", s.csrfGuard(s.handleStart))
	mux.HandleFunc("/api/pause", s.csrfGuard(s.handlePause))
	mux.HandleFunc("/api/run", s.csrfGuard(s.handleRun))
	mux.HandleFunc("/api/dry-run", s.csrfGuard(s.handleDryRun))
}

// csrfGuard rejects state-changing requests whose Origin header doesn't
// match the request Host, as defense-in-depth alongside the sign-in gate.
func (s *Server) csrfGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if u, err := url.Parse(origin); err == nil && u.Host != r.Host {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "Cross-origin request rejected."})
				return
			}
		}
		next(w, r)
	}
}

// handleHealthz reports app-level liveness for container healthchecks. It
// is intentionally unauthenticated (a healthcheck has no session) and
// returns no sensitive data.
//
// This deliberately does NOT attempt to detect a stale/orphaned network
// namespace (e.g. a VPN interface disappearing after the VPN container
// sharing this one's network namespace gets recreated). That check has to
// run from outside this
// process — a `docker exec` healthcheck that inspects the container's
// current namespace from Docker's perspective — because a check done by
// this process only ever sees the namespace it's currently in; it can't
// tell "I used to be attached to a working tunnel and now I'm not" from the
// inside. Namespace health belongs in the deployment's own HEALTHCHECK,
// not here.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var st store.State
	s.store.View(func(state store.State) { st = state })
	resolved := settings.Resolve(st.Settings)

	currentPoints, havePoints := s.scheduler.PointsStatus()

	writeJSON(w, http.StatusOK, map[string]any{
		"settings":           publicSettings(resolved),
		"totals":             st.Totals,
		"history":            reversed(st.History),
		"scheduler_enabled":  st.SchedulerOn,
		"paused":             st.Paused,
		"running":            s.scheduler.IsRunning(),
		"next_run_time":      st.NextRunTime,
		"current_points":     currentPoints,
		"have_points":        havePoints,
		"rate_limited_until": rateLimitedUntil(st),
		"next_run_reason":    st.NextRunReason,
		"points_per_hour":    st.Accrual.PointsPerHour,
	})
}

// rateLimitedUntil reports an active MAM rate-limit backoff, or nil if none
// is in effect. An expired window reads as nil rather than a past timestamp,
// so the UI never has to reason about whether it has lapsed.
func rateLimitedUntil(st store.State) *time.Time {
	if st.RateLimitedUntil == nil || !time.Now().Before(*st.RateLimitedUntil) {
		return nil
	}
	return st.RateLimitedUntil
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var st store.State
		s.store.View(func(state store.State) { st = state })
		writeJSON(w, http.StatusOK, publicSettings(settings.Resolve(st.Settings)))
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var incoming map[string]any
	if err := readJSON(r, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	_, mamIDChanged := incoming["mam_id"]

	err := s.store.Update(func(st *store.State) {
		applySettingsPatch(&st.Settings, incoming)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if mamIDChanged {
		s.scheduler.RefreshPoints()
	}
	var st store.State
	s.store.View(func(state store.State) { st = state })
	writeJSON(w, http.StatusOK, publicSettings(settings.Resolve(st.Settings)))
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := s.scheduler.StartSchedule(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.scheduler.Pause(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FLOnlyOverride bool `json:"fl_only_override"`
	}
	_ = readJSON(r, &req)
	started, reason := s.scheduler.RunNow(req.FLOnlyOverride)
	resp := map[string]any{"started": started}
	if reason != "" {
		resp["reason"] = reason
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	started, reason := s.scheduler.RunDryNow()
	resp := map[string]any{"started": started}
	if reason != "" {
		resp["reason"] = reason
	}
	writeJSON(w, http.StatusOK, resp)
}

// applySettingsPatch applies whitelisted, validated fields from incoming
// onto st. Fields that are currently env-managed are silently ignored so a
// client can't override an operator-pinned value via the API.
func applySettingsPatch(st *store.Settings, incoming map[string]any) {
	resolved := settings.Resolve(*st)

	setBool := func(key string, dst *bool) {
		if resolved.IsManaged(key) {
			return
		}
		if v, ok := incoming[key].(bool); ok {
			*dst = v
		}
	}
	setInt := func(key string, dst *int, min, max int) {
		if resolved.IsManaged(key) {
			return
		}
		if v, ok := incoming[key].(float64); ok {
			n := int(v)
			if n < min {
				n = min
			}
			if max > 0 && n > max {
				n = max
			}
			*dst = n
		}
	}
	setString := func(key string, dst *string) {
		if resolved.IsManaged(key) {
			return
		}
		if v, ok := incoming[key].(string); ok {
			*dst = strings.TrimSpace(v)
		}
	}

	setBool("buy_vip", &st.BuyVIP)
	setString("upload_strategy", &st.UploadStrategy)
	validStrategies := map[string]bool{
		store.StrategyNone:       true,
		store.StrategyUploadOnly: true,
		store.StrategyWedgeOnly:  true,
		store.StrategyAlternate:  true,
	}
	if !validStrategies[st.UploadStrategy] {
		st.UploadStrategy = store.StrategyUploadOnly
	}
	// alternate_next_purchase is internal rotation state the scheduler
	// manages between runs — not user-settable directly.
	setInt("points_buffer", &st.PointsBuffer, 0, 25000)
	setInt("max_upload_gb_per_run", &st.MaxUploadGBPerRun, mamclient.MinUploadGB, 0)
	setInt("next_run_delay_minutes", &st.NextRunDelayMinutes, settings.MinRunDelayMinutes, 0)
	setString("mam_id", &st.MamID)
}

// publicSettings renders resolved settings for API responses: env-managed
// fields are flagged, and the MAM session cookie is masked once saved.
func publicSettings(resolved settings.Resolved) map[string]any {
	s := resolved.Settings
	fields := map[string]any{
		"buy_vip":                 s.BuyVIP,
		"upload_strategy":         s.UploadStrategy,
		"alternate_next_purchase": s.AlternateNextPurchase,
		"points_buffer":           s.PointsBuffer,
		"max_upload_gb_per_run":   s.MaxUploadGBPerRun,
		"next_run_delay_minutes":  s.NextRunDelayMinutes,
		"mam_id":                  settingsMaskMamID(resolved),
	}
	envManaged := map[string]string{}
	for k, v := range resolved.Managed {
		envManaged[k] = v
	}
	return map[string]any{
		"values":      fields,
		"env_managed": envManaged,
		"mam_id_set":  s.MamID != "",
	}
}

// settingsMaskMamID masks the MAM session cookie regardless of whether it
// came from the environment or persisted config — the raw value is never
// echoed back over the API once saved.
func settingsMaskMamID(resolved settings.Resolved) string {
	return settings.MaskSecret(resolved.Settings.MamID)
}

func reversed(entries []store.HistoryEntry) []store.HistoryEntry {
	out := make([]store.HistoryEntry, len(entries))
	for i, e := range entries {
		out[len(entries)-1-i] = e
	}
	return out
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
