// Command automouse automates spending MyAnonamouse bonus points (VIP
// renewal, upload credit, freeleech wedges) on a schedule, behind a
// first-launch admin login and env-var-overridable settings.
package main

import (
	"net/http"
	"os"
	"time"

	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/miista/automouse/internal/api"
	"github.com/miista/automouse/internal/auth"
	"github.com/miista/automouse/internal/scheduler"
	"github.com/miista/automouse/internal/store"
)

func main() {
	log := newLogger()

	dataDir := os.Getenv("AUTOMOUSE_DATA_DIR")
	if dataDir == "" {
		dataDir = "/app/data"
	}
	const addr = ":8765"
	// The image sets WORKDIR / and copies the dashboard to /web/static, so
	// this relative path always resolves.
	const staticDir = "web/static"

	st, err := store.Open(dataDir)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open state store")
	}

	authManager, err := auth.NewManager(st)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize auth")
	}

	sched := scheduler.New(st, log)
	sched.Start()

	server := api.New(st, authManager, sched, log)

	mux := http.NewServeMux()
	server.Routes(mux)
	// No build step means no hashed filenames, so a browser must revalidate
	// app.js/style.css/index.html on every load rather than caching them
	// blindly — otherwise a deployed update can silently keep serving a
	// stale dashboard until the user manually clears site data.
	mux.Handle("/", noCache(http.FileServer(http.Dir(staticDir))))

	log.Info().Str("addr", addr).Str("data_dir", dataDir).Msg("starting automouse")

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("server stopped")
	}
}

// noCache forces revalidation on every static asset request instead of
// letting a browser cache app.js/style.css/index.html indefinitely — see
// the comment at its call site for why that matters here.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// newLogger builds the process-wide zerolog.Logger using ConsoleWriter —
// the same colored, human-readable format used elsewhere in this stack
// (tagbrr, reaparr, diun: "TIME | LEVEL | message key=value ..."), instead
// of raw JSON lines. Level is configurable via LOG_LEVEL.
func newLogger() zerolog.Logger {
	level := zerolog.InfoLevel
	// zerolog.ParseLevel("") returns (zerolog.NoLevel, nil) — no error —
	// so checking err == nil alone silently replaced the InfoLevel default
	// with NoLevel whenever LOG_LEVEL was unset (the common case), which
	// suppressed all leveled log output (.Info()/.Warn()/etc.) with no
	// error or other symptom — found live in packrat (2026-10-01, this
	// file's exact sibling): the running container produced zero log
	// lines despite serving requests correctly. Only apply a parsed level
	// when LOG_LEVEL was actually set to something.
	if raw := os.Getenv("LOG_LEVEL"); raw != "" {
		if lvl, err := zerolog.ParseLevel(raw); err == nil {
			level = lvl
		}
	}
	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
		// Colors forced on: docker logs / Dozzle render ANSI fine,
		// matching the rest of the stack.
	}
	return zerolog.New(writer).
		Level(level).
		With().
		Timestamp().
		Logger()
}
