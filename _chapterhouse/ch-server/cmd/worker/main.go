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
//
// Nightly consolidation (disabled unless CONSOLIDATE_WORKSPACES is set):
//
//	CONSOLIDATE_WORKSPACES  csv of workspace UUIDs; empty => disabled (kill-switch)
//	CONSOLIDATE_HOUR        local hour (0-23) to run; default 2
//	MENTAT_URL              required for consolidation — clustering service
//	CONSOLIDATE_LLM_URL     OpenAI-compatible chat URL; empty => skip labels+digest
//	CONSOLIDATE_LLM_MODEL   chat model id; default "local-model"
//	CONSOLIDATE_LLM_API_KEY bearer token for the chat endpoint (optional)
//	EMBEDDING_URL           OpenAI-compatible embeddings URL; empty => skip digest
//	EMBEDDING_MODEL         embedding model id; default "text-embedding-3-small"
//	EMBEDDING_API_KEY       bearer token for the embeddings endpoint (optional)
//	EMBEDDING_DIM           embedding dimensionality; default 1024
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"
	"github.com/thinkwright/chapterhouse/ch-server/internal/envcfg"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/semantic"
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

	// Nightly consolidation. Disabled unless CONSOLIDATE_WORKSPACES names at
	// least one workspace — the empty-list kill-switch keeps the worker a
	// pure co-activation drainer until an operator opts a workspace in. When
	// enabled it runs alongside the DrainAndStrengthen tick below in the same
	// process; the two are independent.
	startConsolidation(ctx, repo, logger)

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

// startConsolidation reads the CONSOLIDATE_* env, and — when at least one
// workspace is configured and MENTAT_URL is set — constructs the pipeline
// Deps once and launches the nightly loop goroutine. Empty workspace list
// (or missing mentat) logs a reason and returns without starting anything.
func startConsolidation(ctx context.Context, repo *repository.Repository, logger *slog.Logger) {
	workspaces := consolidation.ParseWorkspaces(envcfg.String("CONSOLIDATE_WORKSPACES", ""))
	if len(workspaces) == 0 {
		logger.Info("consolidation disabled (CONSOLIDATE_WORKSPACES empty)")
		return
	}
	mentatURL := envcfg.String("MENTAT_URL", "")
	if mentatURL == "" {
		logger.Warn("consolidation configured but MENTAT_URL unset; disabling",
			slog.Int("workspaces", len(workspaces)))
		return
	}

	mentatClient := mentat.NewClient(mentatURL, nil)

	// LLM (labels + digest) is optional: NewLLMClient returns nil when the
	// URL is empty, and the pipeline skips label/digest on a nil client.
	llm := consolidation.NewLLMClient(
		envcfg.String("CONSOLIDATE_LLM_URL", ""),
		envcfg.String("CONSOLIDATE_LLM_MODEL", "local-model"),
		envcfg.String("CONSOLIDATE_LLM_API_KEY", ""),
	)

	// Embedder (digest text -> vector) is optional too. Keep it a nil
	// interface when unset so the pipeline's `Embedder == nil` guard holds
	// (a typed-nil concrete value would read as non-nil).
	var embedder consolidation.Embedder
	if embURL := envcfg.String("EMBEDDING_URL", ""); embURL != "" {
		embedder = embedding.NewOpenAIProvider(embedding.Config{
			URL:        embURL,
			Model:      envcfg.String("EMBEDDING_MODEL", "text-embedding-3-small"),
			Dimensions: envcfg.Int("EMBEDDING_DIM", 1024),
		}, envcfg.String("EMBEDDING_API_KEY", ""))
	}

	deps := consolidation.Deps{
		Repo:     repo,
		Mentat:   mentatClient,
		Pooler:   semantic.NewWriter(repo, mentatClient),
		LLM:      llm,
		Embedder: embedder,
		Logger:   logger,
	}
	hour := envcfg.Int("CONSOLIDATE_HOUR", 2)

	logger.Info("consolidation enabled",
		slog.Int("workspaces", len(workspaces)),
		slog.Int("hour", hour),
		slog.Bool("llm", llm != nil),
		slog.Bool("embedder", embedder != nil),
	)
	go runNightlyConsolidation(ctx, deps, workspaces, hour, logger)
}

// runNightlyConsolidation sleeps until the next CONSOLIDATE_HOUR:00, then
// runs RunWorkspace for each workspace, and repeats. Recomputing the delay
// each cycle pins execution to hour:00 local (self-correcting across run
// duration + DST) rather than drifting on a fixed 24h ticker. Per-workspace
// errors (e.g. mentat-down) are logged and do not abort the batch or the
// loop — the next night retries.
func runNightlyConsolidation(ctx context.Context, d consolidation.Deps, workspaces []uuid.UUID, hour int, logger *slog.Logger) {
	for {
		delay := consolidation.NextRunDelay(time.Now(), hour)
		logger.Info("consolidation scheduled",
			slog.Duration("next_run_in", delay),
			slog.Int("workspaces", len(workspaces)),
		)
		select {
		case <-ctx.Done():
			logger.Info("consolidation loop stopping")
			return
		case <-time.After(delay):
		}
		for _, ws := range workspaces {
			if err := consolidation.RunWorkspace(ctx, d, ws); err != nil {
				logger.Error("consolidation run failed",
					slog.String("workspace_id", ws.String()),
					slog.String("error", err.Error()),
				)
			}
		}
	}
}

