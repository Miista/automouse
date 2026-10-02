// Package api implements the HTTP handlers for automouse: first-launch
// admin setup, login/logout, settings (with env-override reporting), and
// scheduler control.
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/miista/automouse/internal/auth"
	"github.com/miista/automouse/internal/mamclient"
	"github.com/miista/automouse/internal/scheduler"
	"github.com/miista/automouse/internal/settings"
	"github.com/miista/automouse/internal/store"
)

// Server wires the store, auth manager, and scheduler into HTTP handlers.
type Server struct {
	store     *store.Store
	auth      *auth.Manager
	scheduler *scheduler.Scheduler
	log       zerolog.Logger
	authOff   bool
}

// New builds a Server.
func New(st *store.Store, am *auth.Manager, sc *scheduler.Scheduler, log zerolog.Logger) *Server {
	return &Server{
		store:     st,
		auth:      am,
		scheduler: sc,
		log:       log.With().Str("component", "api").Logger(),
		authOff:   settings.AuthDisabled(),
	}
}

// Routes registers all handlers on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/setup", s.handleSetup)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.guarded(s.handleLogout))
	mux.HandleFunc("/api/state", s.guarded(s.handleState))
	mux.HandleFunc("/api/settings", s.guarded(s.handleSettings))
	mux.HandleFunc("/api/start", s.guarded(s.csrfGuard(s.handleStart)))
	mux.HandleFunc("/api/pause", s.guarded(s.csrfGuard(s.handlePause)))
	mux.HandleFunc("/api/run", s.guarded(s.csrfGuard(s.handleRun)))
}

// guarded enforces that, once an admin account exists (and auth isn't
// explicitly disabled), a request carries a valid session — on every
// route, including read-only state.
func (s *Server) guarded(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authOff {
			next(w, r)
			return
		}
		if !s.auth.HasAdmin() {
			writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "Admin account setup required."})
			return
		}
		if !s.auth.Authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Login required."})
			return
		}
		next(w, r)
	}
}

// csrfGuard rejects state-changing requests whose Origin header doesn't
// match the request Host, as defense-in-depth alongside session auth.
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

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]bool{"admin_exists": s.auth.HasAdmin()})
	case http.MethodPost:
		if s.auth.HasAdmin() {
			writeJSON(w, http.StatusConflict, map[string]string{"error": auth.ErrAdminExists.Error()})
			return
		}
		var req struct{ Username, Password string }
		if err := readJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.auth.CreateAdmin(req.Username, req.Password); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		token, err := s.auth.Authenticate(req.Username, req.Password)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.auth.SetSessionCookie(w, r, token)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct{ Username, Password string }
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	token, err := s.auth.Authenticate(req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid username or password."})
		return
	}
	s.auth.SetSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.ClearSessionCookie(w, r)
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
