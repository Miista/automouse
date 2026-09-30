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
	addr := os.Getenv("AUTOMOUSE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8765"
	}
	staticDir := os.Getenv("AUTOMOUSE_STATIC_DIR")
	if staticDir == "" {
		staticDir = "web/static"
	}

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
	mux.Handle("/", http.FileServer(http.Dir(staticDir)))

	log.Info().Str("addr", addr).Str("data_dir", dataDir).Str("static_dir", staticDir).Msg("starting automouse")

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("server stopped")
	}
}

func newLogger() zerolog.Logger {
	level := zerolog.InfoLevel
	if lvl, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL")); err == nil {
		level = lvl
	}
	return zerolog.New(os.Stdout).
		Level(level).
		With().
		Timestamp().
		Logger()
}
