package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/config"

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
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	logger.Info("starting Chapterhouse init")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Verify PostgreSQL connectivity
	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN())
	if err != nil {
		return fmt.Errorf("failed to parse database config: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}
	logger.Info("database connectivity verified")

	// Verify pg_recall extension is available
	var extVersion string
	err = pool.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname = 'pg_recall'").Scan(&extVersion)
	if err != nil {
		return fmt.Errorf("pg_recall extension not found — ensure the custom CNPG image includes pg_recall: %w", err)
	}
	logger.Info("pg_recall extension verified", slog.String("version", extVersion))

	logger.Info("init complete")
	return nil
}
