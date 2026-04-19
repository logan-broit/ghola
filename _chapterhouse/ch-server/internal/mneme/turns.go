package mneme

// RememberWithTurns -- multi-granularity ingest.
//
// Stores a session as a parent mneme plus one sub_mneme per turn. The parent
// mneme carries all cognitive state (confidence, access_count, tier, etc.)
// and is the unit of cognitive processing (Hebbian associations, contradiction
// detection, consolidation). Sub_mnemes are retrieval primitives: each turn
// gets its own embedding for fine-grained semantic/FTS matching.
//
// Each turn is encoded in ISOLATION via the embedding provider's batched
// native sentence-level encoding. One HTTP round-trip covers all turns in
// a session (subject to the provider's MaxBatch; we chunk above 32).
//
// The parent embedding covers the full session_text for cognitive operations
// (Hebbian association matching, near-duplicate detection). Primary retrieval
// uses sub_mneme embeddings via pg_ghola's recall_with_submnemes path (see
// pg_ghola/src/recall.rs, recall_inner rewritten in commit 80e120f).
//
// Design refs:
//   - pg_ghola/docs/plans/2026-04-16-multi-granularity-encoding-design.md
//   - pg_ghola/docs/plans/2026-04-16-multi-granularity-encoding-implementation.md
//   - pg_ghola/docs/plans/2026-04-17-encoding-eval-harness-design.md

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// maxEmbedBatch chunks turn-embedding calls into groups of at most this size.
// Matches the default Config.MaxBatch in the embedding package (32). Sessions
// with <= 32 turns (the common case for LongMemEval-S: median 12, max 32)
// encode in a single HTTP round-trip.
const maxEmbedBatch = 32

// allowedTurnRoles mirrors the CHECK constraint on ghola.sub_mnemes.role.
// Kept in sync with pg_ghola/src/schema.rs create_sub_mnemes_table.
var allowedTurnRoles = map[string]bool{
	"user":      true,
	"assistant": true,
	"system":    true,
	"tool":      true,
}

// TurnInput is the per-turn payload for RememberWithTurns. The caller (MCP
// handler) is responsible for parsing the turn structure out of the incoming
// request and producing these values. Concatenating Content for every turn
// in order MUST equal sessionText; the char offsets index into sessionText.
type TurnInput struct {
	Role      string
	Content   string
	CharStart int
	CharEnd   int
}

// RememberWithTurns stores a session as a parent mneme plus one sub_mneme
// per turn, atomically. The parent's fields (concept, tags, memType, etc.)
// work identically to Remember; the new surface is the turns slice and
// the sub_mneme rows it produces.
//
// Returns the parent Mneme and an optional near-duplicate notice (detected
// against the parent embedding, same as Remember).
//
// Pre-conditions validated before any DB or embedding work:
//   - len(turns) >= 1
//   - every turn's role is one of user/assistant/system/tool
//   - sessionText[CharStart:CharEnd] == Content for every turn
//   - char offsets are non-overlapping and ordered (implicit in the above)
//
// On embedding or DB failure the transaction is rolled back. Post-commit
// housekeeping (supersede, near-duplicate scan) runs best-effort and does
// not fail the call if it errors out; those are warnings in the log.
func (s *Store) RememberWithTurns(
	ctx context.Context,
	userID, orgID uuid.UUID,
	sessionText string,
	turns []TurnInput,
	memType, scope, tier string,
	tags []string,
	sessionID *uuid.UUID,
) (Mneme, *NearDuplicate, error) {
	// 0. Validate inputs before touching embeddings or DB.
	if len(turns) == 0 {
		return Mneme{}, nil, fmt.Errorf("session must have at least one turn")
	}
	if err := validateTurnReconstruction(sessionText, turns); err != nil {
		return Mneme{}, nil, err
	}
	for i, t := range turns {
		if !allowedTurnRoles[t.Role] {
			return Mneme{}, nil, fmt.Errorf("turn %d has invalid role %q (expected user|assistant|system|tool)", i, t.Role)
		}
	}

	// 1. Embed the parent (session-level, for cognitive operations).
	parentVec, err := s.embedder.Embed(ctx, sessionText)
	if err != nil {
		return Mneme{}, nil, fmt.Errorf("embed parent: %w", err)
	}

	// 2. Embed turns in isolation via batched native encoding. For sessions
	// with <= maxEmbedBatch turns this is one HTTP round-trip.
	turnTexts := make([]string, len(turns))
	for i, t := range turns {
		turnTexts[i] = t.Content
	}
	turnVecs, err := s.embedBatchedChunked(ctx, turnTexts)
	if err != nil {
		return Mneme{}, nil, fmt.Errorf("embed turns: %w", err)
	}
	if len(turnVecs) != len(turns) {
		return Mneme{}, nil, fmt.Errorf("embed returned %d vectors for %d turns", len(turnVecs), len(turns))
	}

	// 3. Derive parent metadata (mirrors Remember).
	concept := sanitizeConcept(sessionText)
	wsID := workspaceForScope(userID, orgID, scope)
	var expiresAt *time.Time
	if memType == "working" {
		t := time.Now().Add(7 * 24 * time.Hour)
		expiresAt = &t
	}

	// 4. Look up existing concept for supersede (read-only; outside tx is fine).
	var oldID *uuid.UUID
	existing, err := scanMneme(s.pool.QueryRow(ctx, findByConcept, wsID, concept))
	if err == nil {
		oldID = &existing.ID
	}

	// 5. Atomic: parent + sub_mnemes in one transaction.
	parentVecStr := vectorToString(parentVec)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Mneme{}, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after successful commit

	parentRow := tx.QueryRow(ctx, insertMneme,
		wsID, concept, sessionText, parentVecStr,
		memType, scope, tier, tags, sessionID, expiresAt,
	)
	m, err := scanMneme(parentRow)
	if err != nil {
		return Mneme{}, nil, fmt.Errorf("insert parent mneme: %w", err)
	}

	for i, t := range turns {
		turnVecStr := vectorToString(turnVecs[i])
		if _, err := tx.Exec(ctx, insertSubMneme,
			m.ID, int16(i), t.Role, t.Content, turnVecStr, t.CharStart, t.CharEnd,
		); err != nil {
			return Mneme{}, nil, fmt.Errorf("insert sub_mneme position=%d: %w", i, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Mneme{}, nil, fmt.Errorf("commit tx: %w", err)
	}

	// 6. Best-effort post-commit: supersede old concept, near-duplicate scan.
	// Parallel to Remember; failures here are logged warnings, not errors.
	if oldID != nil {
		if _, err := s.pool.Exec(ctx, markSupersedes, m.ID, *oldID); err != nil {
			s.logger.Warn("mark_supersedes failed after RememberWithTurns",
				slog.String("error", err.Error()),
				slog.String("new_id", m.ID.String()),
				slog.String("old_id", oldID.String()),
			)
		}
	}

	var dup *NearDuplicate
	dupRows, err := s.pool.Query(ctx, nearDuplicateCheck, parentVecStr, wsID, m.ID)
	if err != nil {
		s.logger.Warn("near-duplicate check failed after RememberWithTurns",
			slog.String("error", err.Error()),
		)
	} else {
		defer dupRows.Close()
		for dupRows.Next() {
			var d NearDuplicate
			var sim float64
			if err := dupRows.Scan(&d.ID, nil, &d.Content, &sim); err != nil {
				continue
			}
			if sim >= nearDuplicateThreshold {
				d.Similarity = sim
				dup = &d
				break
			}
		}
	}

	return m, dup, nil
}

// embedBatchedChunked calls Provider.EmbedBatch in chunks of maxEmbedBatch to
// respect the provider's batch limit. For the common case (len(texts) <= 32),
// this is a single HTTP round-trip. Returns embeddings in the same order as
// the input.
func (s *Store) embedBatchedChunked(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) <= maxEmbedBatch {
		return s.embedder.EmbedBatch(ctx, texts)
	}
	result := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += maxEmbedBatch {
		end := i + maxEmbedBatch
		if end > len(texts) {
			end = len(texts)
		}
		chunk, err := s.embedder.EmbedBatch(ctx, texts[i:end])
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", i, end, err)
		}
		result = append(result, chunk...)
	}
	return result, nil
}

// validateTurnReconstruction verifies that concatenating turn.Content values
// in order reconstructs sessionText byte-for-byte, and that each turn's char
// offsets index correctly into sessionText. Fails BEFORE any DB or embedding
// work so callers don't pay for malformed input.
func validateTurnReconstruction(sessionText string, turns []TurnInput) error {
	var reconstructed strings.Builder
	reconstructed.Grow(len(sessionText))
	for i, t := range turns {
		if t.CharStart < 0 {
			return fmt.Errorf("turn %d has negative char_start %d", i, t.CharStart)
		}
		if t.CharEnd <= t.CharStart {
			return fmt.Errorf("turn %d has empty char span [%d:%d]", i, t.CharStart, t.CharEnd)
		}
		if t.CharEnd > len(sessionText) {
			return fmt.Errorf("turn %d char_end %d exceeds session length %d", i, t.CharEnd, len(sessionText))
		}
		slice := sessionText[t.CharStart:t.CharEnd]
		if slice != t.Content {
			return fmt.Errorf("turn %d content does not match session_text[%d:%d]", i, t.CharStart, t.CharEnd)
		}
		reconstructed.WriteString(t.Content)
	}
	if reconstructed.String() != sessionText {
		return fmt.Errorf("concatenation of turn contents does not equal session_text (reconstructed %d chars, expected %d)",
			reconstructed.Len(), len(sessionText))
	}
	return nil
}
