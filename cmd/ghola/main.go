// Command ghola is the per-user local service. Binds localhost:7421
// by default and exposes the 12 canonical memory operations as
// HTTP/JSON. Agents (Claude Code via MCP in cmd/ghola-mcp, pi-mono
// via HTTP) call it to record turns and query across the three
// memory tiers.
//
// Configuration is environment-variable driven:
//
//	GHOLA_ADDR                listen address            (127.0.0.1:7421)
//	GHOLA_SESSIONS_DIR        sietch root               (~/.ghola/sessions)
//	GHOLA_LOOPBACK_ONLY       reject non-loopback       (true)
//	CHAPTERHOUSE_URL          chapterhouse API base     (http://localhost:8080)
//	CHAPTERHOUSE_API_KEY      per-user Bearer key       (required in prod)
//	EMBEDDING_URL             embeddings service base   (http://localhost:8082)
//	EMBEDDING_MODEL           embedding model name      (qwen3-embedding)
//	TRUTHSAYER_URL            reranker service base     (empty disables rerank)
//	RERANK_TOPK               candidate pool sent to reranker (50)
//	RERANK_WEIGHT             reranker share of fused score [0,1] (0.5; re-sweep after PR-D, PR-E)
//	GHOLA_SETTLE              P4 settle default for unset requests (channel; "off" is the kill-switch)
//	GHOLA_ACTIVATION_WEIGHT   channel-mode activation weight (0,1] (0.40)
//	RRF_K                     RRF fusion constant       (60)
//	GHOLA_TIER_TIMEOUT_MS     per-recall-tier timeout in ms (10000)
//	ENCODING_INTERVAL         sietch -> episodic tick cadence (5m)
//	GHOLA_SIETCH_RETENTION    keep drained session files this long (7d; 0 disables GC)
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
	"github.com/logan-broit/ghola/internal/encoding"
	"github.com/logan-broit/ghola/internal/envcfg"
	ghttp "github.com/logan-broit/ghola/internal/http"
	"github.com/logan-broit/ghola/internal/sietch"
	"github.com/logan-broit/ghola/internal/truthsayer"
	"github.com/logan-broit/ghola/pkg/embedding"
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

	addr := envcfg.String("GHOLA_ADDR", "127.0.0.1:7421")
	sessionsDir := envcfg.String("GHOLA_SESSIONS_DIR", "")
	if sessionsDir == "" {
		def, err := sietch.DefaultRoot()
		if err != nil {
			return fmt.Errorf("resolve default sessions dir: %w", err)
		}
		sessionsDir = def
	}
	loopbackOnly := envcfg.Bool("GHOLA_LOOPBACK_ONLY", true)

	chBase := envcfg.String("CHAPTERHOUSE_URL", "http://localhost:8080")
	chKey := os.Getenv("CHAPTERHOUSE_API_KEY")
	if chKey == "" && envcfg.Bool("CHAPTERHOUSE_REQUIRE_KEY", true) {
		return errors.New("CHAPTERHOUSE_API_KEY is required; set it or pass CHAPTERHOUSE_REQUIRE_KEY=false for local dev against an open server")
	}

	embedBase := envcfg.String("EMBEDDING_URL", "http://localhost:8082")
	embedModel := envcfg.String("EMBEDDING_MODEL", "qwen3-embedding")

	truthsayerBase := os.Getenv("TRUTHSAYER_URL")
	var tsClient *truthsayer.Client
	if truthsayerBase != "" {
		tsClient = truthsayer.New(truthsayerBase)
		healthCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := tsClient.Health(healthCtx); err != nil {
			logger.Warn("truthsayer health probe failed at startup; rerank will fall back to RRF-only when wired",
				"url", truthsayerBase, "error", err.Error())
		} else {
			logger.Info("truthsayer client ready", "url", truthsayerBase)
		}
		cancel()
	}
	encodingInterval, err := time.ParseDuration(envcfg.String("ENCODING_INTERVAL", "5m"))
	if err != nil {
		return fmt.Errorf("parse ENCODING_INTERVAL: %w", err)
	}

	logger.Info("starting ghola local service",
		"addr", addr,
		"sessions_dir", sessionsDir,
		"chapterhouse", chBase,
		"guild", embedBase,
		"truthsayer", truthsayerBase,
		"encoding_interval", encodingInterval)

	store, err := sietch.Open(sessionsDir)
	if err != nil {
		return fmt.Errorf("open sietch: %w", err)
	}
	defer store.Close()

	chClient := chapterhouse.New(chBase, chKey)
	// Timeout 15s + Retries 3 preserve the former internal/embedding.New
	// hard-coded values, keeping the swap to pkg/embedding behavior-neutral.
	embedder := embedding.New(embedding.Config{
		BaseURL: embedBase,
		Model:   embedModel,
		Timeout: 15 * time.Second,
		Retries: 3,
	})

	c := core.New(store, chClient, embedder)
	c.Truthsayer = tsClient
	if v := os.Getenv("RRF_K"); v != "" {
		k, err := strconv.Atoi(v)
		if err != nil || k <= 0 {
			return fmt.Errorf("parse RRF_K: must be positive integer, got %q", v)
		}
		c.RRFK = k
	}
	if v := os.Getenv("RERANK_TOPK"); v != "" {
		k, err := strconv.Atoi(v)
		if err != nil || k <= 0 {
			return fmt.Errorf("parse RERANK_TOPK: must be positive integer, got %q", v)
		}
		c.RerankTopK = k
	}
	if v := os.Getenv("RERANK_WEIGHT"); v != "" {
		w, err := strconv.ParseFloat(v, 64)
		if err != nil || w < 0 || w > 1 {
			return fmt.Errorf("parse RERANK_WEIGHT: must be float in [0,1], got %q", v)
		}
		c.RerankWeight = w
	}
	// Settle default for unset recall requests. "channel" (the New()
	// default) is on-by-default; "off" is the deployment kill-switch —
	// GHOLA_SETTLE=off restores the pre-P4 pipeline for every request that
	// doesn't explicitly opt in, the rollback path if the on-by-default
	// flip regresses in production. Explicit per-request Settle values
	// still override it either way.
	if v := os.Getenv("GHOLA_SETTLE"); v != "" {
		switch v {
		case "off", "expand", "channel":
			c.Settle = v
		default:
			return fmt.Errorf("parse GHOLA_SETTLE: must be one of \"off\", \"expand\", \"channel\", got %q", v)
		}
	}
	if v := os.Getenv("GHOLA_ACTIVATION_WEIGHT"); v != "" {
		w, err := strconv.ParseFloat(v, 64)
		if err != nil || w <= 0 || w > 1 {
			return fmt.Errorf("parse GHOLA_ACTIVATION_WEIGHT: must be float in (0,1], got %q", v)
		}
		c.ActivationWeight = w
	}
	if v := os.Getenv("GHOLA_TIER_TIMEOUT_MS"); v != "" {
		msVal, err := strconv.Atoi(v)
		if err != nil || msVal <= 0 {
			return fmt.Errorf("parse GHOLA_TIER_TIMEOUT_MS: must be positive integer (milliseconds), got %q", v)
		}
		c.TierTimeout = time.Duration(msVal) * time.Millisecond
	}
	// Retention window for drained session files. Unset keeps the
	// New() default (7d); an explicit 0 disables GC (GCSession
	// short-circuits on SietchRetention <= 0).
	c.SietchRetention = envcfg.Duration("GHOLA_SIETCH_RETENTION", c.SietchRetention)
	srv := ghttp.NewServer(c, logger)
	srv.LoopbackOnly = loopbackOnly
	if defaultUser := os.Getenv("AUTH_DEFAULT_USER"); defaultUser != "" {
		if err := srv.SetDefaultUserID(defaultUser); err != nil {
			return fmt.Errorf("AUTH_DEFAULT_USER: %w", err)
		}
		logger.Info("user_id fallback configured", "default_user", defaultUser)
	}

	// Encoding: continuous working -> episodic trace creation. Runs
	// alongside the HTTP server in its own goroutine; cancellation
	// flows through workerCtx on shutdown.
	worker := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		encodingInterval, logger)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	go func() {
		if err := worker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("encoding worker exited with error", "error", err.Error())
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

	// Stop encoding before draining the HTTP server so any in-flight
	// Consolidate calls finish against a stable chapterhouse.
	workerCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Error("shutdown", "error", err.Error())
		return httpSrv.Close()
	}
	return nil
}

// absPath is kept for future use (e.g. relative session dirs via a
// --sessions-dir flag); not wired yet but easier than re-adding
// `path/filepath` later.
var _ = filepath.Abs
