// Command backfill-sessions segments an over-long episodic session into
// natural conversation episodes (see internal/backfill). Dry-run by
// default; --execute applies the transaction after writing a JSON
// backup. One-time, operator-run.
//
// Usage:
//
//	go run ./cmd/backfill-sessions \
//	  --dsn "$DATABASE_URL" \
//	  --session-id 019eb0dd-79c0-a45b-13d8-0d434bff47c8
//	go run ./cmd/backfill-sessions --dsn ... --session-id ... --execute
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thinkwright/chapterhouse/ch-server/internal/backfill"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "backfill-sessions: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "postgres DSN (or DATABASE_URL)")
	sessionID := flag.String("session-id", "", "session to segment (required)")
	gapHours := flag.Int("gap-hours", 4, "idle gap (hours) that starts a new segment")
	execute := flag.Bool("execute", false, "apply the plan (default: dry-run)")
	backupPath := flag.String("backup", "", "backup file (default: backfill-backup-<session-id>.json)")
	orphan := flag.String("orphan-session", "", "second open session to close (optional)")
	flag.Parse()

	if *sessionID == "" {
		return fmt.Errorf("--session-id is required")
	}
	if *dsn == "" {
		return fmt.Errorf("--dsn (or DATABASE_URL) is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	plan, err := backfill.BuildPlan(ctx, pool, *sessionID, time.Duration(*gapHours)*time.Hour)
	if err != nil {
		return err
	}
	fmt.Print(plan.String())

	if !*execute {
		fmt.Println("\n(dry-run) re-run with --execute to apply")
		return nil
	}

	path := *backupPath
	if path == "" {
		path = "backfill-backup-" + *sessionID + ".json"
	}
	if err := backfill.Execute(ctx, pool, plan, path, *orphan); err != nil {
		return err
	}
	fmt.Printf("\napplied; backup written to %s\n", path)
	return nil
}
