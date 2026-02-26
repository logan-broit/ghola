package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/config"
	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/vector"

	"github.com/google/uuid"
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

	logger.Info("starting Qdrant reindex")

	ctx := context.Background()

	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN())
	if err != nil {
		return fmt.Errorf("failed to parse database config: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer pool.Close()

	logger.Info("connected to database")

	repo := repository.New(pool)
	queries := repo.Queries()

	var embedder embedding.Provider
	embeddingCfg := embedding.Config{
		URL:         cfg.Embedding.URL,
		Model:       cfg.Embedding.Model,
		Dimensions:  cfg.Embedding.Dimensions,
		Concurrency: cfg.Embedding.Concurrency,
	}
	embedder = embedding.NewOpenAIProvider(embeddingCfg, cfg.Embedding.APIKey)
	logger.Info("initialized embedding provider",
		slog.String("url", cfg.Embedding.URL),
		slog.String("model", cfg.Embedding.Model),
	)

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
	logger.Info("connected to Qdrant",
		slog.String("host", cfg.Qdrant.Host),
		slog.String("collection", cfg.Qdrant.Collection),
	)

	// Clear all existing Qdrant points to purge stale data from previous
	// indexing runs that used non-deterministic point IDs.
	logger.Info("recreating Qdrant collection to purge stale points")
	if err := vectorDB.RecreateCollection(ctx); err != nil {
		return fmt.Errorf("failed to recreate collection: %w", err)
	}
	logger.Info("collection recreated successfully")

	blocks, err := queries.GetAllCurrentMemoryBlocksWithOrg(ctx)
	if err != nil {
		return fmt.Errorf("failed to get memory blocks: %w", err)
	}

	logger.Info("found memory blocks to reindex", slog.Int("count", len(blocks)))

	success := 0
	failed := 0
	for i, block := range blocks {
		value := ""
		if block.Value.Valid {
			value = block.Value.String
		}

		if value == "" {
			logger.Warn("skipping block with empty value",
				slog.Int64("block_id", block.ID),
				slog.String("name", block.Name),
			)
			continue
		}

		embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		vec, err := embedder.Embed(embedCtx, value)
		cancel()

		if err != nil {
			logger.Error("failed to generate embedding",
				slog.Int64("block_id", block.ID),
				slog.String("error", err.Error()),
			)
			failed++
			continue
		}

		sessionID := ""
		if block.SessionID.Valid {
			sessionID = uuid.UUID(block.SessionID.Bytes).String()
		}

		point := vector.Point{
			ID:         vector.MemoryPointID(block.UserID, block.Name),
			UserID:     block.UserID,
			OrgID:      block.OrgID,
			BlockID:    block.ID,
			Text:       value,
			Scope:      block.Scope,
			MemoryType: block.MemoryType,
			Tags:       block.Tags,
			SessionID:  sessionID,
			Vector:     vec,
		}

		if err := vectorDB.Upsert(ctx, point); err != nil {
			logger.Error("failed to upsert vector",
				slog.Int64("block_id", block.ID),
				slog.String("error", err.Error()),
			)
			failed++
			continue
		}

		success++
		if (i+1)%10 == 0 || i == len(blocks)-1 {
			logger.Info("progress",
				slog.Int("processed", i+1),
				slog.Int("total", len(blocks)),
				slog.Int("success", success),
				slog.Int("failed", failed),
			)
		}
	}

	logger.Info("reindex complete",
		slog.Int("total", len(blocks)),
		slog.Int("success", success),
		slog.Int("failed", failed),
	)

	return nil
}
