package importlogs

// Ingestor streams adapter output into chapterhouse via the existing
// /v1/episodic/ingest endpoint, which is an idempotent upsert by event
// id. Resume state is a client-side flat file of derived UUIDs at
// ResumeStatePath because chapterhouse exposes no GET-by-session
// endpoint; re-running without that file is correct (the server
// upsert dedupes), just slower.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/logan-broit/ghola/internal/chapterhouse"
	"github.com/logan-broit/ghola/internal/core"
)

// Embedder is the minimal contract this package needs from a vector
// service: text in, []float32 out. It is structurally identical to
// core.Embedder; redeclaring it here keeps internal/importlogs free of
// an internal/core import (the package doesn't otherwise need core types
// — it builds them, doesn't consume them). *embedding.Guild satisfies
// this signature directly, so cmd/import-logs can pass a real Guild
// client without an adapter wrapper.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Config governs one import run. User is required by the chapterhouse
// server (every event carries it). Workspace stamps every ingested
// session into a chapterhouse workspace — the server's validateIngest
// rejects sessions without one (400 "session.workspace_id is required"),
// and recall is workspace-scoped at every tier, so an unset workspace
// would silently make imports invisible to every reader.
type Config struct {
	Adapters        map[string]Adapter
	Sources         []Source
	User            uuid.UUID
	Workspace       uuid.UUID
	DryRun          bool
	Resume          bool
	BatchSize       int
	ResumeStatePath string
	ChapterhouseURL string
	ChapterhouseKey string
	Logger          *slog.Logger
	// Embedder, when non-nil, embeds each event's text before ingest so
	// chapterhouse persists episodic.events.embedding instead of NULL.
	// Without this the 5-tier hybrid recall pipeline degrades to FTS-only
	// on the imported corpus (the bug surfaced by seeding-eval: bulk-import
	// produced 266/266 NULL-embedding events, while the local-service flow
	// — which embeds in the encoding worker — populates 199,708/199,995).
	// Optional: nil leaves embedding unset, preserving prior behavior so
	// dry-run + tests without a guild reachable still pass. Calls are
	// sequential per event today; batching is a future optimization.
	Embedder Embedder
	// Output receives the final summary line. nil defaults to os.Stdout;
	// tests and library callers can pass io.Discard or a buffer.
	Output io.Writer
}

// Source binds an adapter name to a root directory to walk.
type Source struct {
	Kind string
	Path string
}

// Summary is the final tally surfaced to the operator.
type Summary struct {
	Imported int
	Skipped  int
	Failed   int
}

func (s Summary) Total() int { return s.Imported + s.Skipped + s.Failed }

// Run executes the import. Errors on a single session do not abort
// the run; the offender is logged and counted as failed.
func Run(ctx context.Context, cfg Config) (Summary, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Output == nil {
		cfg.Output = os.Stdout
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 32
	}

	imported, err := loadImported(cfg.ResumeStatePath, cfg.Resume)
	if err != nil {
		return Summary{}, fmt.Errorf("load resume state: %w", err)
	}

	var client *chapterhouse.Client
	if !cfg.DryRun {
		client = chapterhouse.New(cfg.ChapterhouseURL, cfg.ChapterhouseKey)
	}

	var sum Summary
	for _, src := range cfg.Sources {
		adapter, ok := cfg.Adapters[src.Kind]
		if !ok {
			return sum, fmt.Errorf("no adapter registered for source kind %q", src.Kind)
		}
		for sf := range adapter.Walk(src.Path) {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			ns, err := adapter.Parse(sf)
			if err != nil {
				cfg.Logger.Error("parse failed",
					"adapter", adapter.Name(),
					"path", sf.Path,
					"err", err.Error())
				sum.Failed++
				continue
			}
			if cfg.Resume {
				if _, seen := imported[ns.SessionID]; seen {
					sum.Skipped++
					continue
				}
			}
			if cfg.DryRun {
				sum.Imported++
				continue
			}
			if err := ingestSession(ctx, client, cfg, ns); err != nil {
				cfg.Logger.Error("ingest failed",
					"adapter", adapter.Name(),
					"path", sf.Path,
					"session_id", ns.SessionID.String(),
					"err", err.Error())
				sum.Failed++
				continue
			}
			if err := appendImported(cfg.ResumeStatePath, ns.SessionID); err != nil {
				cfg.Logger.Warn("could not persist resume state",
					"path", cfg.ResumeStatePath,
					"err", err.Error())
			}
			imported[ns.SessionID] = struct{}{}
			sum.Imported++
		}
	}

	fmt.Fprintf(cfg.Output, "imported=%d skipped=%d failed=%d total=%d\n",
		sum.Imported, sum.Skipped, sum.Failed, sum.Total())
	return sum, nil
}

// ingestSession converts one NormalizedSession into core.Session +
// []core.Event and POSTs it in batches. The chapterhouse server is
// the source of truth for upsert; re-POSTing the same id is safe.
func ingestSession(ctx context.Context, client *chapterhouse.Client, cfg Config, ns *NormalizedSession) error {
	sess := core.Session{
		ID:          ns.SessionID.String(),
		UserID:      cfg.User.String(),
		WorkspaceID: cfg.Workspace.String(),
		StartedAt:   ns.StartedAt,
		EndedAt:     ns.EndedAt,
		Cwd:         ns.Cwd,
		GitBranch:   ns.GitBranch,
	}
	if ns.AgentKind != "" {
		ak := ns.AgentKind
		sess.AgentKind = &ak
	}
	if ns.SourceMachine != "" {
		sm := ns.SourceMachine
		sess.SourceDevice = &sm
	}

	events := make([]core.Event, 0, len(ns.Events))
	for i, ev := range ns.Events {
		text := ev.Text
		raw, err := json.Marshal(map[string]any{
			"source_tool": ns.SourceTool,
			"metadata":    ev.Metadata,
		})
		if err != nil {
			return fmt.Errorf("marshal raw_event: %w", err)
		}
		// Honor adapter-supplied IDs verbatim so adapters that can derive
		// stable upstream-keyed UUIDs (e.g. github bundle's
		// uuid5(NS_EVENT, "vercel/next.js/issue/93146")) round-trip into
		// chapterhouse — that's what lets the seeding-eval harness
		// recompute the same id Python-side and match ground truth.
		// Empty falls back to (session, ordinal) so jsonl-family adapters
		// (no upstream id) still upsert idempotently on re-run.
		eventID := ev.ID
		if eventID == "" {
			eventID = deriveEventID(ns.SessionID, i).String()
		}
		// Embed at ingest time so chapterhouse persists a populated
		// episodic.events.embedding. Without this the bulk-import path
		// stores NULL and the 5-tier hybrid recall pipeline degrades to
		// FTS-only on the imported corpus. Sequential calls per event for
		// now — at ~50ms guild latency per event a 50-event session adds
		// ~2.5s, acceptable for a one-shot bootstrap. Batching against
		// vLLM/TEI is the obvious follow-up if this becomes a hot path.
		var emb []float32
		if cfg.Embedder != nil && text != "" {
			emb, err = cfg.Embedder.Embed(ctx, text)
			if err != nil {
				return fmt.Errorf("embed event %d (id=%s): %w", i, eventID, err)
			}
		}
		events = append(events, core.Event{
			ID:        eventID,
			SessionID: sess.ID,
			UserID:    sess.UserID,
			Type:      ev.Type,
			Text:      &text,
			CreatedAt: ev.Timestamp,
			RawEvent:  raw,
			Embedding: emb,
			// Lift Tags/Entities to top-level so chapterhouse's gin-indexed
			// episodic.events.tags / .entities columns are populated — this
			// is what makes WHERE tags @> ARRAY[...] index-served at recall
			// time. Adapters that pack the same lists into Metadata for
			// round-trip fidelity keep doing so; the lift is additive.
			Tags:     ev.Tags,
			Entities: ev.Entities,
		})
	}

	if len(events) == 0 {
		_, _, err := client.IngestEpisodic(ctx, sess, nil)
		return err
	}
	for start := 0; start < len(events); start += cfg.BatchSize {
		end := start + cfg.BatchSize
		if end > len(events) {
			end = len(events)
		}
		if _, _, err := client.IngestEpisodic(ctx, sess, events[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// deriveEventID gives each event a stable UUID derived from
// (session, ordinal). Re-running an import lands on the same ids,
// so chapterhouse's upsert path counts them as Updated rather than
// inserting duplicates.
func deriveEventID(sessionID uuid.UUID, ordinal int) uuid.UUID {
	return uuid.NewSHA1(sessionID, []byte(fmt.Sprintf("event:%d", ordinal)))
}

// loadImported reads the newline-separated UUID file at path. A
// missing file is not an error.
func loadImported(path string, resume bool) (map[uuid.UUID]struct{}, error) {
	out := make(map[uuid.UUID]struct{})
	if !resume || path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		id, err := uuid.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("parse %q in %s: %w", line, path, err)
		}
		out[id] = struct{}{}
	}
	return out, sc.Err()
}

// appendImported atomically appends one UUID to the resume file. Empty
// path is a no-op (lets dry-run + tests skip the side effect).
func appendImported(path string, id uuid.UUID) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, id.String())
	return err
}

