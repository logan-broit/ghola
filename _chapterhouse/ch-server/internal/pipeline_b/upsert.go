package pipeline_b

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UpsertInput bundles everything stage 3 needs: the distilled mneme
// from the LLM, its embedding, the episodic source events, and the
// users whose sessions contributed.
type UpsertInput struct {
	WorkspaceID        uuid.UUID
	Mneme              Mneme
	Embedding          []float32
	SourceEpisodicIDs  []uuid.UUID
	ContributorUserIDs []uuid.UUID
}

// UpsertResult describes the outcome. Inserted=true → new row; else
// the existing row with MnemeID was strengthened.
type UpsertResult struct {
	MnemeID    uuid.UUID
	Inserted   bool
	Similarity float64
}

// DedupConfig tunes the match threshold. The Bayesian evidence used on
// strengthen is a constant — high because the HNSW match already
// passed the similarity gate.
type DedupConfig struct {
	SimilarityThreshold float64 // default 0.9
	StrengthenEvidence  float64 // default 0.9
}

// Upsert writes a semantic mneme with HNSW dedup. If an existing
// mneme in the same workspace has cosine similarity ≥ threshold, that
// row is strengthened via semantic.bayesian_update() and contributor
// arrays are unioned. Otherwise a new row is inserted.
func Upsert(ctx context.Context, pool *pgxpool.Pool, cfg DedupConfig, in UpsertInput) (UpsertResult, error) {
	if cfg.SimilarityThreshold == 0 {
		cfg.SimilarityThreshold = 0.9
	}
	if cfg.StrengthenEvidence == 0 {
		cfg.StrengthenEvidence = 0.9
	}

	embLit, err := vectorLiteral(in.Embedding)
	if err != nil {
		return UpsertResult{}, err
	}

	var (
		existingID uuid.UUID
		similarity float64
	)
	err = pool.QueryRow(ctx, `
		SELECT id, 1 - (embedding <=> ($2::text)::vector) AS sim
		  FROM semantic.mnemes
		 WHERE workspace_id = $1
		   AND state = 'active'
		 ORDER BY embedding <=> ($2::text)::vector
		 LIMIT 1
	`, in.WorkspaceID, embLit).Scan(&existingID, &similarity)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return UpsertResult{}, fmt.Errorf("nearest neighbor: %w", err)
	}

	if existingID != uuid.Nil && similarity >= cfg.SimilarityThreshold {
		if _, err := pool.Exec(ctx, `
			UPDATE semantic.mnemes
			   SET confidence          = semantic.bayesian_update(confidence, $2),
			       source_episodic_ids = array(SELECT DISTINCT unnest(source_episodic_ids || $3::uuid[])),
			       contributor_user_ids= array(SELECT DISTINCT unnest(contributor_user_ids || $4::uuid[])),
			       last_access         = now()
			 WHERE id = $1
		`, existingID, cfg.StrengthenEvidence, in.SourceEpisodicIDs, in.ContributorUserIDs); err != nil {
			return UpsertResult{}, fmt.Errorf("strengthen: %w", err)
		}
		return UpsertResult{MnemeID: existingID, Inserted: false, Similarity: similarity}, nil
	}

	entities := in.Mneme.Entities
	if entities == nil {
		entities = []string{}
	}
	sources := in.SourceEpisodicIDs
	if sources == nil {
		sources = []uuid.UUID{}
	}
	contribs := in.ContributorUserIDs
	if contribs == nil {
		contribs = []uuid.UUID{}
	}

	var newID uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes
		  (workspace_id, concept, content, embedding, memory_type,
		   entities, source_episodic_ids, contributor_user_ids)
		VALUES
		  ($1, $2, $3, ($4::text)::vector, $5, $6, $7, $8)
		RETURNING id
	`, in.WorkspaceID, in.Mneme.Concept, in.Mneme.Content, embLit,
		in.Mneme.MemoryType, entities, sources, contribs).Scan(&newID)
	if err != nil {
		return UpsertResult{}, fmt.Errorf("insert: %w", err)
	}
	return UpsertResult{MnemeID: newID, Inserted: true, Similarity: similarity}, nil
}

func vectorLiteral(v []float32) (string, error) {
	if len(v) == 0 {
		return "", fmt.Errorf("empty embedding")
	}
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(float64(x), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}
