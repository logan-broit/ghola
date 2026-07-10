package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// MnemeHit is one row returned by the v0.3 cosine read path. The
// pre-v0.3 columns (concept/content/memory_type/...) are gone, so the
// hit shape is just enough metadata for the caller to fetch member
// events from episodic if it wants the underlying text.
type MnemeHit struct {
	ID    uuid.UUID
	Score float64
	Level int
}

// QueryMnemesByEmbedding runs HNSW cosine over semantic.mnemes and
// returns the top `limit` active hits for the given workspace.
//
// Cosine similarity is computed as `1 - (embedding <=> $2)` because
// pgvector's `<=>` operator is cosine distance, not similarity. The
// HNSW index `mnemes_embedding_hnsw` (cosine_ops) drives the ORDER BY
// directly.
func (r *Repository) QueryMnemesByEmbedding(
	ctx context.Context, workspaceID uuid.UUID, emb []float32, limit int,
) ([]MnemeHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if len(emb) == 0 {
		return nil, fmt.Errorf("query mnemes: empty embedding")
	}

	rows, err := r.pool.Query(ctx, `
		SELECT id,
		       1 - (embedding <=> ($2::text)::vector) AS score,
		       level
		FROM semantic.mnemes
		WHERE workspace_id = $1 AND state = 'active'
		ORDER BY embedding <=> ($2::text)::vector
		LIMIT $3`, workspaceID, vectorLiteralFloat32(emb), limit)
	if err != nil {
		return nil, fmt.Errorf("query mnemes: %w", err)
	}
	defer rows.Close()

	var out []MnemeHit
	for rows.Next() {
		var h MnemeHit
		if err := rows.Scan(&h.ID, &h.Score, &h.Level); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------
// Consolidation write path — Go overlap-reinforcement port (mnemes.py).
// ---------------------------------------------------------------------

// Level1Mneme is the read shape the consolidation matcher needs: id +
// member_ids + confidence for the overlap-reinforcement decision.
type Level1Mneme struct {
	ID         uuid.UUID
	MemberIDs  []uuid.UUID
	Confidence float64
}

// WorkspaceLevel1Mnemes returns all active level-1 mnemes for a
// workspace. The consolidation matcher scans these for the largest
// member_ids overlap with each new cluster.
func (r *Repository) WorkspaceLevel1Mnemes(ctx context.Context, workspaceID uuid.UUID) ([]Level1Mneme, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, member_ids, confidence
		FROM semantic.mnemes
		WHERE workspace_id = $1 AND level = 1 AND state = 'active'`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace level1 mnemes: %w", err)
	}
	defer rows.Close()
	var out []Level1Mneme
	for rows.Next() {
		var m Level1Mneme
		if err := rows.Scan(&m.ID, &m.MemberIDs, &m.Confidence); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InsertMneme inserts a fresh level-1 mneme at confidence 0.5 (mirrors
// the Python cold-insert path). Returns the new id.
func (r *Repository) InsertMneme(ctx context.Context, workspaceID uuid.UUID, emb []float32, members []uuid.UUID) (uuid.UUID, error) {
	if len(emb) == 0 {
		return uuid.Nil, fmt.Errorf("insert mneme: empty embedding")
	}
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, level, embedding, member_ids, confidence)
		VALUES ($1, 1, ($2::text)::vector, $3, 0.5)
		RETURNING id`,
		workspaceID, vectorLiteralFloat32(emb), members).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert mneme: %w", err)
	}
	return id, nil
}

// ReinforceMneme refreshes an existing mneme's embedding + members,
// bumps last_reinforced_at, and nudges confidence up (cap 0.99).
// Mirrors the Python overlap-reinforcement UPDATE.
func (r *Repository) ReinforceMneme(ctx context.Context, id uuid.UUID, emb []float32, members []uuid.UUID) error {
	if len(emb) == 0 {
		return fmt.Errorf("reinforce mneme: empty embedding")
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE semantic.mnemes
		SET embedding = ($2::text)::vector,
		    member_ids = $3,
		    last_reinforced_at = now(),
		    confidence = LEAST(0.99, confidence + 0.05)
		WHERE id = $1`,
		id, vectorLiteralFloat32(emb), members)
	if err != nil {
		return fmt.Errorf("reinforce mneme: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------
// L1 write path (PR1.7) — feeds the semantic.Writer + Reconciler.
// ---------------------------------------------------------------------

// ClosedSession identifies a closed session that the reconciler still
// needs to pool into an L1 vector. WorkspaceID is the chapterhouse
// workspace identifier; since episodic.sessions stores user_id (and
// chapterhouse maps user_id == personal workspace), we surface it
// under the workspace name to match mentat's PoolRequest contract.
type ClosedSession struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
}

// ClosedSessionsMissingL1 returns up to `limit` closed sessions whose
// l1_embedding column is still NULL, oldest-first by ended_at. The
// reconciler walks this set on every tick.
func (r *Repository) ClosedSessionsMissingL1(ctx context.Context, limit int) ([]ClosedSession, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, user_id
		FROM episodic.sessions
		WHERE ended_at IS NOT NULL AND l1_embedding IS NULL
		ORDER BY ended_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("closed sessions missing l1: %w", err)
	}
	defer rows.Close()

	var out []ClosedSession
	for rows.Next() {
		var s ClosedSession
		if err := rows.Scan(&s.ID, &s.WorkspaceID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SessionEventRow is the per-event shape session-close consumers
// need: mentat pools the embeddings, the l1_chunk_text builder reads
// the role-prefixed text. Type follows episodic.events.type (one of
// user/assistant/tool_result/system); Text is the raw event text the
// agent recorded; Embedding is the per-event vector already populated
// by the ingest path.
type SessionEventRow struct {
	Type      string
	Text      string
	Embedding []float32
}

// SessionEvents returns every embedded event in a session, ordered the
// way mentat expects (created_at ASC, id ASC). Rows whose embedding is
// NULL are filtered out — they would carry no signal for pooling and
// mentat rejects empty-vector inputs anyway.
//
// Note on ordering: the spec calls for `ts ASC, id ASC`, but
// episodic.events stores the canonical timestamp as `created_at`
// (see migrations/001_episodic.sql). created_at is the equivalent
// column.
func (r *Repository) SessionEvents(ctx context.Context, sessionID uuid.UUID) ([]SessionEventRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT type, coalesce(text, ''), embedding::text
		FROM episodic.events
		WHERE session_id = $1 AND embedding IS NOT NULL
		ORDER BY created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session events: %w", err)
	}
	defer rows.Close()

	var out []SessionEventRow
	for rows.Next() {
		var row SessionEventRow
		var lit string
		if err := rows.Scan(&row.Type, &row.Text, &lit); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		emb, err := parseVectorLiteral(lit)
		if err != nil {
			return nil, fmt.Errorf("parse vector: %w", err)
		}
		row.Embedding = emb
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateSessionL1 writes both the pooled L1 vector and the
// role-prefixed concatenated chunk text into episodic.sessions.
// Idempotent: re-running with fresh pool/concat results overwrites
// the previous values, which is what the reconciler wants when a
// session gains additional events post-close.
//
// chunkText may be empty (e.g., legacy callers that only persist the
// embedding) — the column is nullable. Both writes happen in one
// statement so a partial failure can't leave the row inconsistent.
func (r *Repository) UpdateSessionL1(
	ctx context.Context, sessionID uuid.UUID, emb []float32, chunkText string,
) error {
	if len(emb) == 0 {
		return fmt.Errorf("update l1: empty embedding")
	}
	var ct any
	if chunkText != "" {
		ct = chunkText
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE episodic.sessions
		    SET l1_embedding  = ($1::text)::vector,
		        l1_chunk_text = $3
		  WHERE id = $2`,
		vectorLiteralFloat32(emb), sessionID, ct)
	if err != nil {
		return fmt.Errorf("update l1: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------
// Consolidation read path — clustering + selection-first enrichment.
// ---------------------------------------------------------------------

// SessionL1 is one session's clustering input: its id + pooled L1 vector.
type SessionL1 struct {
	SessionID uuid.UUID
	Embedding []float32
}

// WorkspaceSessionL1s returns every closed session in the workspace with
// a populated l1_embedding, as parallel (id, vector) rows for clustering.
// Workspace is scoped via user_id (chapterhouse maps user_id==workspace
// in the single-tenant dev deployment; see ClosedSessionsMissingL1).
func (r *Repository) WorkspaceSessionL1s(ctx context.Context, workspaceID uuid.UUID) ([]SessionL1, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, l1_embedding::text
		FROM episodic.sessions
		WHERE user_id = $1 AND l1_embedding IS NOT NULL
		ORDER BY id ASC`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace session l1s: %w", err)
	}
	defer rows.Close()
	var out []SessionL1
	for rows.Next() {
		var s SessionL1
		var lit string
		if err := rows.Scan(&s.SessionID, &lit); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		emb, err := parseVectorLiteral(lit)
		if err != nil {
			return nil, fmt.Errorf("parse vector: %w", err)
		}
		s.Embedding = emb
		out = append(out, s)
	}
	return out, rows.Err()
}

// EnrichEvent is one event's material for selection-first mnemes.
type EnrichEvent struct {
	ID        uuid.UUID
	Text      string
	Embedding []float32
	CreatedAt time.Time
	Tags      []string
	Entities  []string
}

// SessionEnrichmentEvents returns a session's embedded, text-bearing
// events (created_at ASC, id ASC) with their tags/entities for
// selection-first mneme enrichment. Embedding-null rows are excluded
// (no centrality signal), and only 'user'/'assistant' events qualify —
// 'tool_result'/'system' rows are filtered out so a mneme's user-facing
// representative excerpt is never raw tool output. episodic.events.tags/
// entities are both `text[] NOT NULL DEFAULT '{}'` (see
// migrations/001_episodic.sql), so this always returns a (possibly empty)
// slice rather than nil.
func (r *Repository) SessionEnrichmentEvents(ctx context.Context, sessionID uuid.UUID) ([]EnrichEvent, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, coalesce(text, ''), embedding::text, created_at,
		       tags, entities
		FROM episodic.events
		WHERE session_id = $1 AND embedding IS NOT NULL
		  AND type IN ('user', 'assistant')
		ORDER BY created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session enrichment events: %w", err)
	}
	defer rows.Close()
	var out []EnrichEvent
	for rows.Next() {
		var e EnrichEvent
		var lit string
		if err := rows.Scan(&e.ID, &e.Text, &lit, &e.CreatedAt, &e.Tags, &e.Entities); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		emb, err := parseVectorLiteral(lit)
		if err != nil {
			return nil, fmt.Errorf("parse vector: %w", err)
		}
		e.Embedding = emb
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateMnemeEnrichment writes the selection-first content columns onto
// an existing mneme. label is optional (nil leaves the column
// unchanged so the LLM label step can run independently). reps/meta are
// pre-marshalled JSON bytes; tags/entities are Postgres text[].
func (r *Repository) UpdateMnemeEnrichment(
	ctx context.Context, id uuid.UUID, label *string,
	representatives []byte, tags, entities []string,
	spanStart, spanEnd time.Time, meta []byte,
) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE semantic.mnemes
		SET label           = COALESCE($2, label),
		    representatives = $3::jsonb,
		    tags            = $4,
		    entities        = $5,
		    span_start      = $6,
		    span_end        = $7,
		    meta            = $8::jsonb
		WHERE id = $1`,
		id, label, representatives, tags, entities, spanStart, spanEnd, meta)
	if err != nil {
		return fmt.Errorf("update mneme enrichment: %w", err)
	}
	return nil
}
