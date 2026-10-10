// Command matrixrtc-recorder is a Matrix bot that joins MatrixRTC (Element Call) calls on request
// and records every participant's audio and video.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrixrtc-recorder/internal/bot"
	"github.com/ricardo-duarte-av/matrixrtc-recorder/internal/config"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	flag.Parse()

	// Colours only on a terminal: under Docker they'd show up as escape codes in `docker logs`.
	noColor := !isatty.IsTerminal(os.Stderr.Fd()) || os.Getenv("NO_COLOR") != ""
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.DateTime, NoColor: noColor}).
		With().Timestamp().Logger()
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load configuration")
	}
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		log.Fatal().Err(err).Msg("Bad log_level")
	}
	log = log.Level(level)
	log.Info().Str("version", version).Msg("Starting matrixrtc-recorder")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b, err := bot.New(ctx, cfg, log)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to start")
	}
	if err = b.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("Sync failed")
	}
	log.Info().Msg("Bye")
}
