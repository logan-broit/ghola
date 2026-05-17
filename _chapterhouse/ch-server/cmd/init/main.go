package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/config"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	logger.Info("starting Chapterhouse init")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Verify PostgreSQL connectivity
	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN())
	if err != nil {
		return fmt.Errorf("parse database config: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	logger.Info("database connectivity verified")

	// Verify ghola extension is available (renamed from pg_ghola in v2).
	// Dev stacks running on vanilla pgvector skip this by setting
	// GHOLA_EXTENSION_OPTIONAL=true — the replay pipeline falls back
	// to a test-stub semantic schema seeded by seed.sql.
	var extVersion string
	err = pool.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname = 'ghola'").Scan(&extVersion)
	if err != nil {
		if os.Getenv("GHOLA_EXTENSION_OPTIONAL") == "true" {
			logger.Warn("ghola extension not installed; continuing because GHOLA_EXTENSION_OPTIONAL=true")
		} else {
			return fmt.Errorf("ghola extension not found — ensure the custom CNPG image includes ghola: %w", err)
		}
	} else {
		logger.Info("ghola extension verified", slog.String("version", extVersion))
	}

	// Apply episodic migrations. This is idempotent — already-applied
	// migrations are skipped via _migrations.applied. Requires
	// EMBEDDING_DIM to be set (see repository/migrate.go).
	if err := repository.ApplyMigrations(ctx, pool); err != nil {
		return fmt.Errorf("apply episodic migrations: %w", err)
	}
	logger.Info("episodic migrations applied")

	logger.Info("init complete")
	return nil
}
