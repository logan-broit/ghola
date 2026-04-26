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
type SessionFile struct {
	Path    string
	RawSize int64
}

// NormalizedEvent is the source-agnostic per-turn shape every Adapter
// emits. The Type set matches chapterhouse's accepted values
// (user|assistant|tool_result|system).
type NormalizedEvent struct {
	Type      string
	Text      string
	Timestamp time.Time
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
