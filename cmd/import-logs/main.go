// Command import-logs is the multi-source bootstrap tool for
// chapterhouse. It walks per-tool log roots (claude-code, openclaw,
// augment, codex-cli, hermes, cline, opencode), normalizes them
// through registered Adapters, and POSTs each session through
// /v1/episodic/ingest. Re-runs are idempotent: chapterhouse upserts
// by id and --resume skips sessions whose derived id already lives
// in the resume-state file.
//
// Usage:
//
//	import-logs --workspace=<uuid> --user=<uuid> \
//	    --source=jsonl-family:/path/to/.claude/projects \
//	    [--source=...] [--dry-run] [--resume=true] [--batch-size=32]
//
// Configuration env vars (read on startup, overridable via flags):
//
//	CHAPTERHOUSE_URL          chapterhouse API base   (http://localhost:8080)
//	CHAPTERHOUSE_API_KEY      per-user Bearer key
//	EMBEDDING_URL             guild embedder base URL (e.g. http://localhost:8082)
//	EMBEDDING_MODEL           embedder model name     (default qwen3-embedding)
//
// When EMBEDDING_URL is set, every event is embedded before ingest so
// chapterhouse persists episodic.events.embedding instead of NULL —
// without it the 5-tier hybrid recall pipeline silently degrades to
// FTS-only on the imported corpus. EMBEDDING_URL=""  preserves prior
// behavior (NULL embedding) for callers without a guild reachable.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/logan-broit/ghola/internal/envcfg"
	"github.com/logan-broit/ghola/internal/importlogs"
	"github.com/logan-broit/ghola/internal/importlogs/adapters/github"
	"github.com/logan-broit/ghola/internal/importlogs/adapters/jsonlfamily"
	"github.com/logan-broit/ghola/pkg/embedding"
)

// adapters is the registry of per-source adapters known to this
// build. Direct literal so the registry is grep-able and there are
// no init-order surprises; new adapters land here as they're built.
var adapters = map[string]importlogs.Adapter{
	"github":       github.New(),
	"jsonl-family": jsonlfamily.New(),
}

type sourceList []importlogs.Source

func (s *sourceList) String() string {
	parts := make([]string, len(*s))
	for i, src := range *s {
		parts[i] = src.Kind + ":" + src.Path
	}
	return strings.Join(parts, ",")
}

func (s *sourceList) Set(v string) error {
	kind, path, ok := strings.Cut(v, ":")
	if !ok || kind == "" || path == "" {
		return fmt.Errorf("expected kind:path, got %q", v)
	}
	*s = append(*s, importlogs.Source{Kind: kind, Path: path})
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "import-logs: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		sources         sourceList
		workspaceFlag   string
		userFlag        string
		dryRun          bool
		resume          bool
		batchSize       int
		chapterhouseURL string
		resumeState     string
	)

	flag.Var(&sources, "source", "kind:path adapter+root (repeatable)")
	flag.StringVar(&workspaceFlag, "workspace", "", "workspace UUID (required)")
	flag.StringVar(&userFlag, "user", "", "user UUID (required)")
	flag.BoolVar(&dryRun, "dry-run", false, "parse + count only, no writes")
	flag.BoolVar(&resume, "resume", true, "skip sessions already imported")
	flag.IntVar(&batchSize, "batch-size", 32, "events per ingest call")
	flag.StringVar(&chapterhouseURL, "chapterhouse-url",
		envcfg.String("CHAPTERHOUSE_URL", "http://localhost:8080"),
		"chapterhouse API base")
	flag.StringVar(&resumeState, "resume-state", defaultResumeState(),
		"path to the imported-session UUID list")
	flag.Parse()

	workspace, err := uuid.Parse(workspaceFlag)
	if err != nil {
		return fmt.Errorf("--workspace must be a UUID: %w", err)
	}
	user, err := uuid.Parse(userFlag)
	if err != nil {
		return fmt.Errorf("--user must be a UUID: %w", err)
	}
	if len(sources) == 0 {
		return errors.New("at least one --source=kind:path is required")
	}

	apiKey := os.Getenv("CHAPTERHOUSE_API_KEY")
	if !dryRun && apiKey == "" {
		return errors.New("CHAPTERHOUSE_API_KEY is required (or pass --dry-run)")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Optional embedder. When EMBEDDING_URL is set we wire a real Guild
	// client so events are embedded before ingest; chapterhouse persists
	// the vector verbatim into episodic.events.embedding. When unset we
	// pass nil and the ingestor falls through to embedding-omitted JSON
	// — preserving the prior CLI contract for stack-less dry-runs.
	embedderURL := os.Getenv("EMBEDDING_URL")
	embedderModel := envcfg.String("EMBEDDING_MODEL", "qwen3-embedding")
	var embedder importlogs.Embedder
	if !dryRun && embedderURL != "" {
		// Timeout 15s + Retries 3 preserve the former internal/embedding.New
		// hard-coded values, keeping the swap to pkg/embedding behavior-neutral.
		embedder = embedding.New(embedding.Config{
			BaseURL: embedderURL,
			Model:   embedderModel,
			Timeout: 15 * time.Second,
			Retries: 3,
		})
		logger.Info("embedder enabled",
			"url", embedderURL, "model", embedderModel)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := importlogs.Config{
		Adapters:        adapters,
		Sources:         sources,
		User:            user,
		Workspace:       workspace,
		DryRun:          dryRun,
		Resume:          resume,
		BatchSize:       batchSize,
		ResumeStatePath: resumeState,
		ChapterhouseURL: chapterhouseURL,
		ChapterhouseKey: apiKey,
		Embedder:        embedder,
		Logger:          logger,
		Output:          os.Stdout,
	}
	_, err = importlogs.Run(ctx, cfg)
	return err
}

// defaultResumeState picks ~/.ghola/import-logs/imported.txt, falling
// back to the cwd if HOME is unreadable.
func defaultResumeState() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "imported.txt"
	}
	return filepath.Join(home, ".ghola", "import-logs", "imported.txt")
}
