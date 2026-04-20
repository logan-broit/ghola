package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Mneme mirrors semantic.mnemes + the OpenAPI Mneme DTO.
type Mneme struct {
	ID                  uuid.UUID   `json:"id"`
	WorkspaceID         uuid.UUID   `json:"workspace_id"`
	Concept             string      `json:"concept"`
	Content             string      `json:"content"`
	Confidence          float64     `json:"confidence"`
	AccessCount         int         `json:"access_count"`
	LastAccess          time.Time   `json:"last_access"`
	CreatedAt           time.Time   `json:"created_at"`
	State               string      `json:"state"`
	MemoryType          string      `json:"memory_type"`
	Tags                []string    `json:"tags"`
	Entities            []string    `json:"entities"`
	SourceEpisodicIDs   []uuid.UUID `json:"source_episodic_ids"`
	ContributorUserIDs  []uuid.UUID `json:"contributor_user_ids"`
}

// SemanticMnemeHit is one row returned by semantic.recall(...).
type SemanticMnemeHit struct {
	MnemeID      uuid.UUID
	Score        float64
	ContentMatch float64
	Activation   float64
	HebbianBoost float64
	Confidence   float64
	Concept      string
	Content      string
}

// SemanticQueryParams controls a /v1/semantic/query call.
type SemanticQueryParams struct {
	WorkspaceID    uuid.UUID
	QueryText      string
	QueryEmbedding []float64
	Limit          int
	MinConfidence  float64
}

// SemanticRecall calls the ghola extension's semantic.recall(...) and
// projects the returned composite rows onto []SemanticMnemeHit.
func (r *Repository) SemanticRecall(ctx context.Context, p SemanticQueryParams) ([]SemanticMnemeHit, error) {
	if p.Limit <= 0 {
		p.Limit = 10
	}

	var embLit string
	if len(p.QueryEmbedding) > 0 {
		parts := make([]string, len(p.QueryEmbedding))
		for i, x := range p.QueryEmbedding {
			parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
		}
		embLit = "[" + strings.Join(parts, ",") + "]"
	}

	rows, err := r.pool.Query(ctx, `
		SELECT mneme_id, score, content_match, activation, hebbian_boost,
		       confidence, concept, content
		FROM semantic.recall($1, $2, ($3::text)::vector, $4, $5)
	`, p.WorkspaceID, p.QueryText, embLit, p.Limit, p.MinConfidence)
	if err != nil {
		return nil, fmt.Errorf("semantic.recall: %w", err)
	}
	defer rows.Close()

	var hits []SemanticMnemeHit
	for rows.Next() {
		var h SemanticMnemeHit
		if err := rows.Scan(&h.MnemeID, &h.Score, &h.ContentMatch, &h.Activation,
			&h.HebbianBoost, &h.Confidence, &h.Concept, &h.Content); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// SemanticUpdateConfidence wraps semantic.update_confidence(...).
func (r *Repository) SemanticUpdateConfidence(
	ctx context.Context, mnemeID uuid.UUID, evidence float64,
) (float64, error) {
	var newConf float64
	err := r.pool.QueryRow(ctx,
		`SELECT semantic.update_confidence($1, $2)`,
		mnemeID, evidence,
	).Scan(&newConf)
	if err != nil {
		return 0, fmt.Errorf("update_confidence: %w", err)
	}
	return newConf, nil
}

// ---------------------------------------------------------------------
// List (paginated)
// ---------------------------------------------------------------------

// MnemeFilters narrows a /v1/semantic/list query.
type MnemeFilters struct {
	MemoryType *string  `json:"memory_type,omitempty"`
	State      *string  `json:"state,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Entities   []string `json:"entities,omitempty"`
}

// SemanticListParams is the full input to SemanticList.
type SemanticListParams struct {
	WorkspaceID uuid.UUID
	Limit       int
	Cursor      *string
	Filters     MnemeFilters
}

type listCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"i"`
}

func encodeCursor(c listCursor) string {
	b, _ := json.Marshal(c)
	return base64.URLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*listCursor, error) {
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode cursor: %w", err)
	}
	var c listCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("unmarshal cursor: %w", err)
	}
	return &c, nil
}

// SemanticList paginates semantic.mnemes by (created_at, id) DESC.
// Returns the page + an optional next_cursor (nil when exhausted).
func (r *Repository) SemanticList(
	ctx context.Context, p SemanticListParams,
) ([]Mneme, *string, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}

	q := strings.Builder{}
	args := []any{p.WorkspaceID, p.Limit + 1}
	q.WriteString(`
		SELECT id, workspace_id, concept, content, confidence, access_count,
		       last_access, created_at, state, memory_type, tags, entities,
		       source_episodic_ids, contributor_user_ids
		FROM semantic.mnemes
		WHERE workspace_id = $1`)

	next := 3
	if p.Filters.MemoryType != nil {
		fmt.Fprintf(&q, " AND memory_type = $%d", next)
		args = append(args, *p.Filters.MemoryType)
		next++
	}
	if p.Filters.State != nil {
		fmt.Fprintf(&q, " AND state = $%d", next)
		args = append(args, *p.Filters.State)
		next++
	}
	if len(p.Filters.Tags) > 0 {
		fmt.Fprintf(&q, " AND tags @> $%d::text[]", next)
		args = append(args, p.Filters.Tags)
		next++
	}
	if len(p.Filters.Entities) > 0 {
		fmt.Fprintf(&q, " AND entities && $%d::text[]", next)
		args = append(args, p.Filters.Entities)
		next++
	}
	if p.Cursor != nil && *p.Cursor != "" {
		c, err := decodeCursor(*p.Cursor)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(&q, " AND (created_at, id) < ($%d, $%d)", next, next+1)
		args = append(args, c.CreatedAt, c.ID)
	}

	q.WriteString(" ORDER BY created_at DESC, id DESC LIMIT $2")

	rows, err := r.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()

	var mnemes []Mneme
	for rows.Next() {
		var m Mneme
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.Concept, &m.Content,
			&m.Confidence, &m.AccessCount, &m.LastAccess, &m.CreatedAt,
			&m.State, &m.MemoryType, &m.Tags, &m.Entities,
			&m.SourceEpisodicIDs, &m.ContributorUserIDs); err != nil {
			return nil, nil, fmt.Errorf("scan: %w", err)
		}
		mnemes = append(mnemes, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var nextCursor *string
	if len(mnemes) > p.Limit {
		last := mnemes[p.Limit-1]
		mnemes = mnemes[:p.Limit]
		c := encodeCursor(listCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		nextCursor = &c
	}
	return mnemes, nextCursor, nil
}
