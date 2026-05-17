// Package importlogs is the multi-source bootstrap tool's shared core:
// the Adapter interface, the NormalizedSession shape every adapter
// produces, and idempotent chapterhouse ingestion. Per-source adapters
// (jsonl-family, augment, codex-cli, hermes, cline, opencode) live
// under internal/importlogs/adapters/.
package importlogs

import (
	"crypto/sha256"
	"encoding/hex"
	"iter"
	"time"

	"github.com/google/uuid"
)

// namespaceOID is the RFC 4122 X.500 namespace UUID. Used as the
// SHA-1 namespace for DeriveSessionID so the same (tool, raw bytes)
// always maps to the same UUID across machines and runs.
var namespaceOID = uuid.MustParse("6ba7b812-9dad-11d1-80b4-00c04fd430c8")

// DeriveSessionID is a pure function of (tool, raw bytes) so --resume
// can cheaply detect "already imported."
func DeriveSessionID(sourceTool string, rawBytes []byte) uuid.UUID {
	h := sha256.Sum256(rawBytes)
	return uuid.NewSHA1(namespaceOID, []byte(sourceTool+"|"+hex.EncodeToString(h[:])))
}

// SessionFile points at one on-disk session log an Adapter has
// recognized as belonging to it.
//
// LineNum is an optional 1-indexed cursor into Path for adapters
// whose on-disk format packs multiple logical sessions into a single
// JSONL file (e.g. the github bundle, where one line == one issue
// thread). Adapters that yield one SessionFile per file (jsonlfamily,
// the default) leave it zero; Parse implementations that pack
// multiple sessions per file MUST honor LineNum to identify which
// record in Path to decode. The ingestor never reads it — Path is
// still the canonical log key for resume + dedupe.
type SessionFile struct {
	Path    string
	RawSize int64
	LineNum int
}

// NormalizedEvent is the source-agnostic per-turn shape every Adapter
// emits. The Type set matches chapterhouse's accepted values
// (user|assistant|tool_result|system).
//
// Tags and Entities are the top-level lift: when populated, the
// ingestor copies them onto core.Event.Tags / core.Event.Entities,
// which chapterhouse persists into episodic.events.tags / .entities
// (text[] columns with gin indexes — episodic_events_tags_gin /
// _entities_gin). That's what enables WHERE tags @> ARRAY['era:v15']
// at recall time without a full table scan. Adapters that pack tags
// into Metadata for round-trip fidelity (e.g. github bundles) should
// ALSO populate Tags here so the gin-index path is live; reading tags
// out of raw_event.metadata.tags works but loses the index.
type NormalizedEvent struct {
	// ID, when non-empty, is used verbatim as the chapterhouse event
	// primary key. Adapters that derive stable IDs from upstream data
	// (e.g. github bundle's uuid5(NS_EVENT, "vercel/next.js/issue/93146"))
	// should set this so reruns are idempotent at the row level AND so
	// downstream consumers (e.g. the seeding-eval harness) can recompute
	// the same id Python-side and match ground-truth in chapterhouse.
	// Adapters that have no upstream id (jsonl-family, codex-cli, etc.)
	// can leave it empty; the ingestor falls back to
	// deriveEventID(session, index) so re-ingest still upserts.
	ID        string
	Type      string
	Text      string
	Timestamp time.Time
	Tags      []string
	Entities  []string
	Metadata  map[string]string
}

// NormalizedSession is the source-agnostic per-session shape every
// Adapter emits. The ingestor maps these to chapterhouse's
// repository.EpisodicSession + EpisodicEvent rows.
type NormalizedSession struct {
	SourceTool, SourceMachine string
	SessionID                 uuid.UUID
	UserID                    uuid.UUID
	StartedAt                 time.Time
	EndedAt                   *time.Time
	Cwd, GitBranch            *string
	AgentKind                 string
	Events                    []NormalizedEvent
}

// Adapter is the contract per-source bootstrap adapters implement.
// Walk discovers session files under root; Parse reads one file and
// produces a NormalizedSession ready for ingestion.
type Adapter interface {
	Name() string
	Walk(root string) iter.Seq[SessionFile]
	Parse(sf SessionFile) (*NormalizedSession, error)
}
