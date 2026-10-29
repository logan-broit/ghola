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

	"github.com/logan-broit/ghola/chapterhouse/db"
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

	return applyAll(ctx, pool, migrationsFS, "migrations", "", dim)
}

// ApplyAdminMigrations applies the legacy admin schema (chapterhouse/db/
// migrations) that API-key auth and ch-admin read. Names are recorded in
// _migrations with an "admin/" prefix. A database where the schema was
// applied by hand (api_keys exists, nothing recorded) is baselined: every
// file is marked applied without running it.
func ApplyAdminMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if err := ensureTracking(ctx, pool); err != nil {
		return err
	}
	var manual bool
	if err := pool.QueryRow(ctx, `
		SELECT to_regclass('public.api_keys') IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM _migrations WHERE applied LIKE 'admin/%')
	`).Scan(&manual); err != nil {
		return fmt.Errorf("check admin schema: %w", err)
	}
	if manual {
		names, err := sqlNames(db.Migrations, "migrations")
		if err != nil {
			return err
		}
		for _, name := range names {
			if _, err := pool.Exec(ctx,
				`INSERT INTO _migrations (applied) VALUES ($1) ON CONFLICT DO NOTHING`,
				"admin/"+name,
			); err != nil {
				return fmt.Errorf("baseline %s: %w", name, err)
			}
		}
		return nil
	}
	return applyAll(ctx, pool, db.Migrations, "migrations", "admin/", 0)
}

func applyAll(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir, prefix string, dim int) error {
	names, err := sqlNames(fsys, dir)
	if err != nil {
		return err
	}
	// Bootstrap the tracking table outside the per-migration
	// transaction so the CREATE TABLE survives retries.
	if err := ensureTracking(ctx, pool); err != nil {
		return err
	}
	for _, name := range names {
		if err := applyOne(ctx, pool, fsys, dir, prefix, name, dim); err != nil {
			return fmt.Errorf("migration %s%s: %w", prefix, name, err)
		}
	}
	return nil
}

func sqlNames(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func ensureTracking(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _migrations (
			applied text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create _migrations table: %w", err)
	}
	return nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir, prefix, name string, dim int) error {
	raw, err := fs.ReadFile(fsys, dir+"/"+name)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	sql := string(raw)
	if dim > 0 {
		sql = strings.ReplaceAll(sql, "${EMBEDDING_DIM}", strconv.Itoa(dim))
	}
	name = prefix + name

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
