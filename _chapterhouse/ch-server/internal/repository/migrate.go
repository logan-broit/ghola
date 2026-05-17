package repository

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ApplyMigrations runs every *.sql file in the embedded migrations
// directory in alpha order, each inside its own transaction. Applied
// names are recorded in _migrations.applied so reruns are idempotent.
//
// Before each file's contents execute, any ${EMBEDDING_DIM} token is
// replaced with the positive integer read from the EMBEDDING_DIM
// environment variable. The function fails fast if EMBEDDING_DIM is
// missing or invalid — this is the contract that keeps the substrate
// dimension-agnostic.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dim, err := readEmbeddingDim()
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// Bootstrap the tracking table outside the per-migration
	// transaction so the CREATE TABLE survives retries.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _migrations (
			applied text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create _migrations table: %w", err)
	}

	for _, name := range names {
		if err := applyOne(ctx, pool, name, dim); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, name string, dim int) error {
	raw, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	sql := strings.ReplaceAll(string(raw), "${EMBEDDING_DIM}", strconv.Itoa(dim))

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Skip if already applied. SELECT ... FOR UPDATE would add
	// contention; _migrations is tiny, plain SELECT is fine.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM _migrations WHERE applied = $1)`,
		name,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check applied: %w", err)
	}
	if exists {
		return nil
	}

	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO _migrations (applied) VALUES ($1)`,
		name,
	); err != nil {
		return fmt.Errorf("record applied: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func readEmbeddingDim() (int, error) {
	raw := os.Getenv("EMBEDDING_DIM")
	if raw == "" {
		return 0, fmt.Errorf("EMBEDDING_DIM must be set (e.g. 1024 for Qwen3)")
	}
	dim, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("EMBEDDING_DIM must be an integer, got %q: %w", raw, err)
	}
	if dim <= 0 {
		return 0, fmt.Errorf("EMBEDDING_DIM must be positive, got %d", dim)
	}
	return dim, nil
}
