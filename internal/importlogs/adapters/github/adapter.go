// Package github implements the import-logs adapter that reads a
// GitHub-bundle JSONL (one issue thread per line, written by an
// external Python extract script) and emits NormalizedSessions into
// chapterhouse via the standard import-logs pipeline.
//
// Bundle line shape (one per JSONL line; thread_id collates issue +
// PR + commit events into one logical session):
//
//	{"thread_id": "vercel/next.js#54321",
//	 "session": {
//	   "id":         "<uuid5(NS_session, thread_id)>",
//	   "started_at": "2024-09-12T14:33:01Z",
//	   "ended_at":   "2024-09-19T08:11:44Z",
//	   "summary":    "issue: ...",
//	   "agent_kind": "github",
//	   "cwd":        "vercel/next.js",
//	   "git_branch": null
//	 },
//	 "events": [
//	   {"id":"...", "kind":"issue",  "created_at":"...", "content":"...",
//	    "tags":["era:v14","repo:...","type:issue","author:..."],
//	    "entities":["..."]},
//	   {"id":"...", "kind":"pr",     ...},
//	   {"id":"...", "kind":"commit", ...}
//	 ]}
//
// Walk granularity is per-record, not per-file: one bundle file
// holds many threads, and the import-logs pipeline upserts one
// chapterhouse session per Parse() call. The adapter therefore
// streams the file with bufio.Scanner and yields a SessionFile for
// each line, carrying the LineNum cursor; Parse re-opens the file
// and seeks (by re-scanning) to that line. This keeps memory
// proportional to one record at a time, never the whole bundle —
// real corpora will be large.
package github

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/logan-broit/ghola/internal/importlogs"
)

const (
	// sourceTool lands in NormalizedSession.SourceTool for downstream
	// queries — the chapterhouse-side "where did this row come from"
	// answer for github-extracted threads.
	sourceTool = "github"
	// adapterKey is the registry key matching --source=<kind>:<path>.
	adapterKey = "github"
)

// Adapter implements importlogs.Adapter for GitHub issue-thread bundles.
type Adapter struct {
	logger *slog.Logger
}

// New returns a fresh Adapter. The github adapter is host-agnostic:
// SourceMachine is left empty (the import host running the bootstrap
// is irrelevant to the github-bundle's provenance — the data came
// from github.com, not this machine).
func New() *Adapter {
	return &Adapter{logger: slog.Default()}
}

// Name is the registry key; matches the --source=<kind>:<path> token.
func (a *Adapter) Name() string { return adapterKey }

// Walk discovers bundle records under root. It descends into sub-
// directories so a corpora layout like /bundles/<repo>/threads.jsonl
// nests cleanly, and yields one SessionFile *per record* (per JSONL
// line) — not per file. The bundle is opened once per file and
// scanned line-by-line; we never load more than one line of payload
// at a time. SessionFile.LineNum is the 1-indexed cursor Parse uses
// to re-seek the same record.
func (a *Adapter) Walk(root string) iter.Seq[importlogs.SessionFile] {
	return func(yield func(importlogs.SessionFile) bool) {
		stop := false
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if stop {
				return filepath.SkipAll
			}
			if err != nil {
				a.logger.Debug("walk error", "path", path, "error", err.Error())
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				a.logger.Debug("stat error", "path", path, "error", err.Error())
				return nil
			}
			rawSize := info.Size()

			f, err := os.Open(path)
			if err != nil {
				a.logger.Debug("open error", "path", path, "error", err.Error())
				return nil
			}
			scanner := bufio.NewScanner(f)
			// Bundle lines can be large (a thread may contain dozens
			// of long comments). Match jsonlfamily's 16 MiB ceiling.
			scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
			lineNum := 0
			for scanner.Scan() {
				lineNum++
				if len(scanner.Bytes()) == 0 {
					continue
				}
				if !yield(importlogs.SessionFile{
					Path:    path,
					RawSize: rawSize,
					LineNum: lineNum,
				}) {
					stop = true
					break
				}
			}
			if err := scanner.Err(); err != nil {
				a.logger.Debug("scan error", "path", path, "error", err.Error())
			}
			_ = f.Close()
			return nil
		})
	}
}

// bundleRecord is one JSONL line in a github bundle. Field tags
// are the wire-format contract with the Python extract.py — must
// be exact (snake_case, omitempty where the Python writer omits).
type bundleRecord struct {
	ThreadID string        `json:"thread_id"`
	Session  bundleSession `json:"session"`
	Events   []bundleEvent `json:"events"`
}

type bundleSession struct {
	ID        string     `json:"id"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Summary   string     `json:"summary"`
	AgentKind string     `json:"agent_kind"`
	CWD       string     `json:"cwd"`
	GitBranch *string    `json:"git_branch,omitempty"`
}

type bundleEvent struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // issue | pr | commit
	CreatedAt time.Time `json:"created_at"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags"`
	Entities  []string  `json:"entities"`
}

// Parse reads exactly one record from sf.Path at sf.LineNum and
// returns the corresponding NormalizedSession. It re-opens the file
// (Walk closed its handle) and re-scans to the target line so memory
// stays proportional to the largest single record, never the whole
// bundle.
func (a *Adapter) Parse(sf importlogs.SessionFile) (*importlogs.NormalizedSession, error) {
	if sf.LineNum < 1 {
		return nil, fmt.Errorf("github adapter: SessionFile.LineNum must be >=1 for bundle records (path=%s)", sf.Path)
	}
	f, err := os.Open(sf.Path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", sf.Path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var line []byte
	cur := 0
	for scanner.Scan() {
		cur++
		if cur == sf.LineNum {
			// Copy: scanner.Bytes() is reused on next Scan().
			line = append(line, scanner.Bytes()...)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", sf.Path, err)
	}
	if line == nil {
		return nil, fmt.Errorf("github adapter: %s has no record at line %d", sf.Path, sf.LineNum)
	}

	var rec bundleRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, fmt.Errorf("decode bundle record %s line %d: %w", sf.Path, sf.LineNum, err)
	}

	if rec.Session.ID == "" {
		return nil, fmt.Errorf("github adapter: %s line %d: session.id is required", sf.Path, sf.LineNum)
	}
	sessionID, err := uuid.Parse(rec.Session.ID)
	if err != nil {
		return nil, fmt.Errorf("github adapter: %s line %d: session.id %q is not a uuid: %w", sf.Path, sf.LineNum, rec.Session.ID, err)
	}
	if len(rec.Events) == 0 {
		return nil, fmt.Errorf("github adapter: %s line %d: bundle record has zero events", sf.Path, sf.LineNum)
	}

	events := make([]importlogs.NormalizedEvent, 0, len(rec.Events))
	for i, ev := range rec.Events {
		if ev.Kind == "" {
			return nil, fmt.Errorf("github adapter: %s line %d events[%d]: kind is required", sf.Path, sf.LineNum, i)
		}
		md := map[string]string{}
		if len(ev.Tags) > 0 {
			b, err := json.Marshal(ev.Tags)
			if err != nil {
				return nil, fmt.Errorf("github adapter: %s line %d events[%d]: marshal tags: %w", sf.Path, sf.LineNum, i, err)
			}
			md["tags"] = string(b)
		} else {
			md["tags"] = "[]"
		}
		if len(ev.Entities) > 0 {
			b, err := json.Marshal(ev.Entities)
			if err != nil {
				return nil, fmt.Errorf("github adapter: %s line %d events[%d]: marshal entities: %w", sf.Path, sf.LineNum, i, err)
			}
			md["entities"] = string(b)
		} else {
			md["entities"] = "[]"
		}
		if ev.ID != "" {
			md["event_id"] = ev.ID
		}
		events = append(events, importlogs.NormalizedEvent{
			// Bundle event_id is a stable UUID derived Python-side from
			// (repo, kind, num) — uuid5(NS_EVENT, "vercel/next.js/issue/93146").
			// The ingestor honors this verbatim as core.Event.ID, which is
			// what lets the seeding-eval harness recompute the same id and
			// match chapterhouse's stored ids at recall time. md["event_id"]
			// stays in place for back-compat with readers that decode it
			// out of raw_event.metadata; the lift is additive.
			ID: ev.ID,
			// Canonical chapterhouse type set is
			// {user|assistant|tool_result|system}; github bundle
			// content (issue body, PR description, commit message)
			// is externally-authored, so "user" is the closest fit.
			// Per-event kind discrimination is preserved on
			// Metadata.tags via the type:<kind> tag stamped above.
			Type:      "user",
			Text:      ev.Content,
			Timestamp: ev.CreatedAt,
			// Top-level lift: same lists also live on Metadata
			// (round-trip envelope). The ingestor copies these onto
			// core.Event.Tags / .Entities, which chapterhouse
			// persists into episodic.events.tags / .entities — gin-
			// indexed text[] columns that serve WHERE tags @>
			// ARRAY['era:v15'] without a full scan.
			Tags:     ev.Tags,
			Entities: ev.Entities,
			Metadata: md,
		})
	}

	agentKind := rec.Session.AgentKind
	if agentKind == "" {
		agentKind = sourceTool
	}

	out := &importlogs.NormalizedSession{
		SourceTool: sourceTool,
		// SourceMachine intentionally empty: github bundles have no
		// "machine of record" — they came from github.com.
		SessionID: sessionID,
		StartedAt: rec.Session.StartedAt,
		EndedAt:   rec.Session.EndedAt,
		AgentKind: agentKind,
		Events:    events,
	}
	if rec.Session.CWD != "" {
		cwd := rec.Session.CWD
		out.Cwd = &cwd
	}
	if rec.Session.GitBranch != nil && *rec.Session.GitBranch != "" {
		gb := *rec.Session.GitBranch
		out.GitBranch = &gb
	}
	return out, nil
}
