// Command mam-spender automates spending MyAnonamouse bonus points (VIP
// renewal, upload credit, freeleech wedges) on a schedule, behind a
// first-launch admin login and env-var-overridable settings.
package main

import (
	"net/http"
	"os"
	"time"

	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/miista/mam-spender/internal/api"
	"github.com/miista/mam-spender/internal/auth"
	"github.com/miista/mam-spender/internal/scheduler"
	"github.com/miista/mam-spender/internal/store"
)

func main() {
	log := newLogger()

	dataDir := os.Getenv("MAMSPENDER_DATA_DIR")
	if dataDir == "" {
		dataDir = "/app/data"
	}
	addr := os.Getenv("MAMSPENDER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8765"
	}
	staticDir := os.Getenv("MAMSPENDER_STATIC_DIR")
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

	log.Info().Str("addr", addr).Str("data_dir", dataDir).Str("static_dir", staticDir).Msg("starting mam-spender")

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
