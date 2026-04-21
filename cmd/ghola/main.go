// Command ghola is the per-user local service. Binds localhost:7421
// by default and exposes the 12 canonical memory operations as
// HTTP/JSON. Agents (Claude Code via MCP in cmd/ghola-mcp, pi-mono
// via HTTP) call it to record turns and query across the three
// memory tiers.
//
// Configuration is environment-variable driven:
//
//   GHOLA_ADDR                listen address            (127.0.0.1:7421)
//   GHOLA_SESSIONS_DIR        sietch root               (~/.ghola/sessions)
//   GHOLA_LOOPBACK_ONLY       reject non-loopback       (true)
//   CHAPTERHOUSE_URL          chapterhouse API base     (http://localhost:8080)
//   CHAPTERHOUSE_API_KEY      per-user Bearer key       (required in prod)
//   MELANGE_URL               embeddings service base   (http://localhost:8082)
//   MELANGE_MODEL             embedding model name      (qwen3-embedding)
//   PIPELINE_A_INTERVAL       consolidation tick cadence (5m)
package main

import (
	"context"
	"errors"
	"fmt"
	stdhttp "net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"log/slog"

	"github.com/logan-broit/ghola/internal/chapterhouse"
	"github.com/logan-broit/ghola/internal/core"
	"github.com/logan-broit/ghola/internal/embedding"
	ghttp "github.com/logan-broit/ghola/internal/http"
	"github.com/logan-broit/ghola/internal/pipeline_a"
	"github.com/logan-broit/ghola/internal/sietch"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ghola: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	addr := envOr("GHOLA_ADDR", "127.0.0.1:7421")
	sessionsDir := envOr("GHOLA_SESSIONS_DIR", "")
	if sessionsDir == "" {
		def, err := sietch.DefaultRoot()
		if err != nil {
			return fmt.Errorf("resolve default sessions dir: %w", err)
		}
		sessionsDir = def
	}
	loopbackOnly := envBool("GHOLA_LOOPBACK_ONLY", true)

	chBase := envOr("CHAPTERHOUSE_URL", "http://localhost:8080")
	chKey := os.Getenv("CHAPTERHOUSE_API_KEY")
	if chKey == "" && envBool("CHAPTERHOUSE_REQUIRE_KEY", true) {
		return errors.New("CHAPTERHOUSE_API_KEY is required; set it or pass CHAPTERHOUSE_REQUIRE_KEY=false for local dev against an open server")
	}

	melBase := envOr("MELANGE_URL", "http://localhost:8082")
	melModel := envOr("MELANGE_MODEL", "qwen3-embedding")

	pipelineInterval, err := time.ParseDuration(envOr("PIPELINE_A_INTERVAL", "5m"))
	if err != nil {
		return fmt.Errorf("parse PIPELINE_A_INTERVAL: %w", err)
	}

	logger.Info("starting ghola local service",
		"addr", addr,
		"sessions_dir", sessionsDir,
		"chapterhouse", chBase,
		"melange", melBase,
		"pipeline_a_interval", pipelineInterval)

	store, err := sietch.Open(sessionsDir)
	if err != nil {
		return fmt.Errorf("open sietch: %w", err)
	}
	defer store.Close()

	chClient := chapterhouse.New(chBase, chKey)
	embedder := embedding.New(melBase, melModel)

	c := core.New(store, chClient, embedder)
	srv := ghttp.NewServer(c, logger)
	srv.LoopbackOnly = loopbackOnly

	// Pipeline A: continuous working -> episodic consolidation. Runs
	// alongside the HTTP server in its own goroutine; cancellation
	// flows through workerCtx on shutdown.
	worker := pipeline_a.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		pipelineInterval, logger)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	go func() {
		if err := worker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("pipeline A exited with error", "err", err.Error())
		}
	}()

	httpSrv := &stdhttp.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, stdhttp.ErrServerClosed) {
			serverErr <- err
		}
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return fmt.Errorf("server: %w", err)
	case sig := <-shutdown:
		logger.Info("shutdown signal received", "signal", sig.String())
	}

	// Stop Pipeline A before draining the HTTP server so any in-flight
	// Consolidate calls finish against a stable chapterhouse.
	workerCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Error("shutdown", "err", err.Error())
		return httpSrv.Close()
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// absPath is kept for future use (e.g. relative session dirs via a
// --sessions-dir flag); not wired yet but easier than re-adding
// `path/filepath` later.
var _ = filepath.Abs
