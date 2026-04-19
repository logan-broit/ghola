package mneme

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const nearDuplicateThreshold = 0.92

// Mneme represents a single memory unit from ghola.mnemes.
type Mneme struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Concept     string
	Content     string
	Confidence  float64
	AccessCount int32
	LastAccess  time.Time
	CreatedAt   time.Time
	State       string
	MemoryType  string
	Scope       string
	Tier        string
	Tags        []string
	SessionID   *uuid.UUID
	ExpiresAt   *time.Time
}

// NearDuplicate is returned alongside a stored mneme when a similar one exists.
type NearDuplicate struct {
	ID         uuid.UUID
	Content    string
	Similarity float64
}

// RecallResult represents a single result from ghola.recall().
type RecallResult struct {
	MnemeID      uuid.UUID
	Score        float64
	ContentMatch float64
	Activation   float64
	HebbianBoost float64
	Confidence   float64
	Concept      string
	Content      string
	Scope        string // set during merge (personal or org)
}

// Session represents an aggregated session from memory records.
type Session struct {
	SessionID     uuid.UUID
	MemoryCount   int64
	FirstActivity time.Time
	LastActivity  time.Time
}

// Store provides pg_ghola storage operations.
type Store struct {
	pool     *pgxpool.Pool
	embedder embedding.Provider
	logger   *slog.Logger
}

// NewStore creates a new pg_ghola store.
func NewStore(pool *pgxpool.Pool, embedder embedding.Provider, logger *slog.Logger) *Store {
	return &Store{pool: pool, embedder: embedder, logger: logger}
}

// workspaces returns the workspace IDs to query: [userID, orgID].
// Both are always included so queries see personal + org-scoped mnemes.
func workspaces(userID, orgID uuid.UUID) []uuid.UUID {
	return []uuid.UUID{userID, orgID}
}

// workspaceForScope returns the workspace_id to use for a given scope.
func workspaceForScope(userID, orgID uuid.UUID, scope string) uuid.UUID {
	if scope == "org" {
		return orgID
	}
	return userID
}

// sanitizeConcept generates a short concept key from a fact string.
func sanitizeConcept(s string) string {
	runes := []rune(s)
	if len(runes) > 50 {
		s = string(runes[:50])
	}
	var result strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			result.WriteRune(c)
		} else if c == ' ' {
			result.WriteRune('_')
		}
	}
	return strings.ToLower(result.String())
}

func scanMneme(row pgx.Row) (Mneme, error) {
	var m Mneme
	var sessionID *uuid.UUID
	var expiresAt *time.Time
	err := row.Scan(
		&m.ID, &m.WorkspaceID, &m.Concept, &m.Content,
		&m.Confidence, &m.AccessCount, &m.LastAccess, &m.CreatedAt,
		&m.State, &m.MemoryType, &m.Scope, &m.Tier, &m.Tags,
		&sessionID, &expiresAt,
	)
	m.SessionID = sessionID
	m.ExpiresAt = expiresAt
	return m, err
}

func scanMnemes(rows pgx.Rows) ([]Mneme, error) {
	var result []Mneme
	for rows.Next() {
		var m Mneme
		var sessionID *uuid.UUID
		var expiresAt *time.Time
		if err := rows.Scan(
			&m.ID, &m.WorkspaceID, &m.Concept, &m.Content,
			&m.Confidence, &m.AccessCount, &m.LastAccess, &m.CreatedAt,
			&m.State, &m.MemoryType, &m.Scope, &m.Tier, &m.Tags,
			&sessionID, &expiresAt,
		); err != nil {
			return nil, err
		}
		m.SessionID = sessionID
		m.ExpiresAt = expiresAt
		result = append(result, m)
	}
	return result, rows.Err()
}

// Remember stores a new mneme, embedding the content synchronously.
// If an existing mneme with the same concept exists, the new one supersedes it.
// Returns the stored mneme and an optional near-duplicate notice.
func (s *Store) Remember(ctx context.Context, userID, orgID uuid.UUID, fact, memType, scope, tier string, tags []string, sessionID *uuid.UUID) (Mneme, *NearDuplicate, error) {
	// 1. Embed synchronously
	vec, err := s.embedder.Embed(ctx, fact)
	if err != nil {
		return Mneme{}, nil, fmt.Errorf("embedding failed: %w", err)
	}

	concept := sanitizeConcept(fact)
	wsID := workspaceForScope(userID, orgID, scope)

	var expiresAt *time.Time
	if memType == "working" {
		t := time.Now().Add(7 * 24 * time.Hour)
		expiresAt = &t
	}

	// 2. Check for existing mneme with same concept → will supersede
	var oldID *uuid.UUID
	existing, err := scanMneme(s.pool.QueryRow(ctx, findByConcept, wsID, concept))
	if err == nil {
		oldID = &existing.ID
	}

	// 3. INSERT into ghola.mnemes
	vecStr := vectorToString(vec)
	insertRow := s.pool.QueryRow(ctx, insertMneme,
		wsID, concept, fact, vecStr,
		memType, scope, tier, tags, sessionID, expiresAt,
	)
	m, err := scanMneme(insertRow)
	if err != nil {
		return Mneme{}, nil, fmt.Errorf("insert failed: %w", err)
	}

	// 4. Supersede old mneme if found
	if oldID != nil {
		if _, err := s.pool.Exec(ctx, markSupersedes, m.ID, *oldID); err != nil {
			s.logger.Warn("mark_supersedes failed",
				slog.String("error", err.Error()),
				slog.String("new_id", m.ID.String()),
				slog.String("old_id", oldID.String()),
			)
		}
	}

	// 5. Near-duplicate check
	var dup *NearDuplicate
	dupRows, err := s.pool.Query(ctx, nearDuplicateCheck, vecStr, wsID, m.ID)
	if err != nil {
		s.logger.Warn("near-duplicate check failed", slog.String("error", err.Error()))
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

// Recall searches memories using ghola.recall(), querying both personal and org workspaces.
func (s *Store) Recall(ctx context.Context, userID, orgID uuid.UUID, query string, limit int, mode, memType string, tags []string, sessionID *uuid.UUID) ([]RecallResult, error) {
	vec, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embedding failed: %w", err)
	}
	vecStr := vectorToString(vec)

	// Map mode to score_weights
	var weights string
	switch mode {
	case "semantic":
		weights = "(1.0, 0.0, 0.5, 4.0)"
	case "keyword":
		weights = "(0.0, 1.0, 0.5, 4.0)"
	default: // hybrid
		weights = "(0.6, 0.4, 0.5, 4.0)"
	}

	var memTypeArg, scopeArg *string
	var tagsArg []string
	if memType != "" {
		memTypeArg = &memType
	}
	if len(tags) > 0 {
		tagsArg = tags
	}

	// Query personal workspace
	personalResults, err := s.doRecall(ctx, userID, query, vecStr, limit, weights, memTypeArg, nil, tagsArg, sessionID)
	if err != nil {
		return nil, err
	}
	for i := range personalResults {
		personalResults[i].Scope = "personal"
	}

	// Query org workspace
	orgScope := "org"
	scopeArg = &orgScope
	orgResults, err := s.doRecall(ctx, orgID, query, vecStr, limit, weights, memTypeArg, scopeArg, tagsArg, sessionID)
	if err != nil {
		s.logger.Warn("org recall failed", slog.String("error", err.Error()))
		orgResults = nil
	}
	for i := range orgResults {
		orgResults[i].Scope = "org"
	}

	// Merge by score, deduplicate by mneme_id
	all := append(personalResults, orgResults...)
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })

	seen := make(map[uuid.UUID]bool)
	var merged []RecallResult
	for _, r := range all {
		if seen[r.MnemeID] {
			continue
		}
		seen[r.MnemeID] = true
		merged = append(merged, r)
		if len(merged) >= limit {
			break
		}
	}

	return merged, nil
}

func (s *Store) doRecall(ctx context.Context, wsID uuid.UUID, queryText, vecStr string, limit int, weights string, memType, scope *string, tags []string, sessionID *uuid.UUID) ([]RecallResult, error) {
	rows, err := s.pool.Query(ctx, recallQuery,
		wsID, queryText, vecStr, limit, 0.0, weights,
		memType, scope, tags, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("recall query failed: %w", err)
	}
	defer rows.Close()

	var results []RecallResult
	for rows.Next() {
		var r RecallResult
		if err := rows.Scan(
			&r.MnemeID, &r.Score, &r.ContentMatch, &r.Activation,
			&r.HebbianBoost, &r.Confidence, &r.Concept, &r.Content,
		); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ConfirmRecall applies Bayesian confidence updates for recalled mnemes.
func (s *Store) ConfirmRecall(ctx context.Context, mnemeIDs []uuid.UUID) error {
	if len(mnemeIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, confirmRecall, mnemeIDs)
	return err
}

// WeightedConfirmRecall applies score-weighted Bayesian confidence updates for recalled mnemes.
// Higher recall scores produce stronger confirmation evidence.
func (s *Store) WeightedConfirmRecall(ctx context.Context, mnemeIDs []uuid.UUID, scores []float64) error {
	if len(mnemeIDs) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	for i, id := range mnemeIDs {
		var evidence float64
		switch {
		case scores[i] >= 0.8:
			evidence = 0.80
		case scores[i] >= 0.5:
			evidence = 0.65
		default: // >= 0.3 (caller filters below 0.3)
			evidence = 0.55
		}
		if _, err := tx.Exec(ctx, weightedConfirmSingle, id, evidence); err != nil {
			return fmt.Errorf("weighted confirm mneme %s: %w", id, err)
		}
	}

	return tx.Commit(ctx)
}

// FeedbackMemory applies an explicit evidence signal to a mneme's confidence.
func (s *Store) FeedbackMemory(ctx context.Context, userID, orgID uuid.UUID, mnemeID uuid.UUID, evidence float64) (float64, error) {
	ws := workspaces(userID, orgID)
	var confidence float64
	err := s.pool.QueryRow(ctx, feedbackUpdate, mnemeID, evidence, ws).Scan(&confidence)
	if err != nil {
		return 0, fmt.Errorf("feedback update failed: %w", err)
	}
	return confidence, nil
}

// Forget deletes a mneme by ID, verifying ownership via workspace membership.
func (s *Store) Forget(ctx context.Context, userID, orgID uuid.UUID, mnemeID uuid.UUID) error {
	// Try personal workspace first, then org
	for _, wsID := range workspaces(userID, orgID) {
		tag, err := s.pool.Exec(ctx, deleteMneme, mnemeID, wsID)
		if err != nil {
			return fmt.Errorf("delete failed: %w", err)
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
	}
	return fmt.Errorf("memory not found or you don't have permission to delete it")
}

// ChangeScope moves a mneme between personal and org scope.
func (s *Store) ChangeScope(ctx context.Context, userID, orgID uuid.UUID, mnemeID uuid.UUID, newScope string) error {
	// Look up the mneme first to verify it exists and find current workspace
	row := s.pool.QueryRow(ctx, getMnemeByID, mnemeID)
	m, err := scanMneme(row)
	if err != nil {
		return fmt.Errorf("memory not found")
	}

	// Verify ownership: must belong to user's personal or org workspace
	if m.WorkspaceID != userID && m.WorkspaceID != orgID {
		return fmt.Errorf("memory not found or you don't have permission to modify it")
	}

	newWsID := workspaceForScope(userID, orgID, newScope)
	currentWsID := m.WorkspaceID

	updateRow := s.pool.QueryRow(ctx, updateScope, newScope, newWsID, mnemeID, currentWsID)
	if _, err := scanMneme(updateRow); err != nil {
		return fmt.Errorf("scope update failed: %w", err)
	}
	return nil
}

// List returns mnemes matching optional filters.
func (s *Store) List(ctx context.Context, userID, orgID uuid.UUID, memType string, tags []string, sessionID *uuid.UUID, limit int) ([]Mneme, error) {
	if limit <= 0 {
		limit = 50
	}

	var memTypeArg *string
	if memType != "" {
		memTypeArg = &memType
	}
	var tagsArg []string
	if len(tags) > 0 {
		tagsArg = tags
	}

	rows, err := s.pool.Query(ctx, listMnemes,
		workspaces(userID, orgID), memTypeArg, tagsArg, sessionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMnemes(rows)
}

// Export returns all active mnemes for a user (personal + org).
func (s *Store) Export(ctx context.Context, userID, orgID uuid.UUID) ([]Mneme, error) {
	rows, err := s.pool.Query(ctx, exportMnemes, workspaces(userID, orgID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMnemes(rows)
}

// ListSessions returns session aggregations for a user's memories.
func (s *Store) ListSessions(ctx context.Context, userID, orgID uuid.UUID, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, listSessions, workspaces(userID, orgID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.SessionID, &sess.MemoryCount, &sess.FirstActivity, &sess.LastActivity); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// GetSessionMemories returns all mnemes for a given session.
func (s *Store) GetSessionMemories(ctx context.Context, userID, orgID uuid.UUID, sessionID uuid.UUID) ([]Mneme, error) {
	rows, err := s.pool.Query(ctx, getSessionMemories, workspaces(userID, orgID), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMnemes(rows)
}

// vectorToString converts a float32 slice to a PostgreSQL vector literal.
func vectorToString(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

