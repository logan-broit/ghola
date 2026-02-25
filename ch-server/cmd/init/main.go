package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/config"
	"github.com/thinkwright/chapterhouse/ch-server/internal/vector"

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

	// Ensure Qdrant collection exists
	vectorDB, err := vector.NewClient(vector.Config{
		Host:       cfg.Qdrant.Host,
		GRPCPort:   cfg.Qdrant.GRPCPort,
		APIKey:     cfg.Qdrant.APIKey,
		Collection: cfg.Qdrant.Collection,
		Dimensions: cfg.Embedding.Dimensions,
	})
	if err != nil {
		return fmt.Errorf("failed to connect to Qdrant: %w", err)
	}
	defer vectorDB.Close()

	if err := vectorDB.EnsureCollection(ctx); err != nil {
		return fmt.Errorf("failed to ensure Qdrant collection: %w", err)
	}
	logger.Info("Qdrant collection ready",
		slog.String("host", cfg.Qdrant.Host),
		slog.String("collection", cfg.Qdrant.Collection),
		slog.Int("dimensions", cfg.Embedding.Dimensions),
	)

	logger.Info("init complete")
	return nil
}
