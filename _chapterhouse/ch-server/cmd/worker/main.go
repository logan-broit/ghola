// Package main implements the chapterhouse background worker.
//
// The worker drains the co-activation queue every WORKER_TICK_SECONDS and
// folds each pair into semantic.associations as a strengthened Hebbian
// link. This is the offline half of Pipeline B: ingest enqueues pairs
// (cmd/api), the worker consolidates them on a tick.
//
// Future scheduled jobs (Ebbinghaus decay sweeps, contradiction scanning)
// plug in alongside the existing tick loop.
//
// Configured via environment variables:
//
//	DATABASE_URL          required — Postgres connection string
//	WORKER_TICK_SECONDS   optional — default 30
//	WORKER_BATCH_SIZE     optional — default 100
//	LOG_LEVEL             optional — "debug" enables debug logs (default info)
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/envcfg"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

func main() {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	tickSec := envcfg.Int("WORKER_TICK_SECONDS", 30)
	batchSize := envcfg.Int("WORKER_BATCH_SIZE", 100)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		logger.Error("connect db", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		logger.Error("ping db", slog.String("error", err.Error()))
		os.Exit(1)
	}

	repo := repository.New(pool)

	logger.Info("worker started",
		slog.Int("tick_seconds", tickSec),
		slog.Int("batch_size", batchSize),
	)
	ticker := time.NewTicker(time.Duration(tickSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("worker stopping")
			return
		case <-ticker.C:
			n, err := consolidation.DrainAndStrengthen(ctx, repo, batchSize)
			if err != nil {
				// Continue ticking — transient DB errors recover on
				// the next tick; the queue is durable so nothing is
				// lost while we wait.
				logger.Error("drain failed", slog.String("error", err.Error()))
				continue
			}
			if n > 0 {
				logger.Info("drained co-activations", slog.Int("count", n))
			}
		}
	}
}

