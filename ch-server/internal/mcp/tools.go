package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"
	"github.com/thinkwright/chapterhouse/ch-server/internal/secrets"
	"github.com/thinkwright/chapterhouse/ch-server/internal/vector"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// nearDuplicateThreshold is the cosine similarity threshold above which a
// memory is considered a near-duplicate during store. The memory is still
// stored — the notice lets the agent decide whether to forget the old one.
const nearDuplicateThreshold = 0.92

// Enum validation sets, shared across handlers.
var (
	validMemoryTypes = map[string]bool{
		"factual":      true,
		"experiential": true,
		"working":      true,
	}
	validScopes = map[string]bool{
		"personal": true,
		"org":      true,
	}
)

// sessionToPgUUID converts an auth.Context SessionID to a pgtype.UUID.
// Returns an invalid pgtype.UUID when the session is uuid.Nil (stateless transport).
func sessionToPgUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// sessionToString returns the session UUID as a string, or "" for uuid.Nil.
func sessionToString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// parseSessionIDArg validates an optional session_id argument string.
// Returns the parsed UUID string and any validation error.
func parseSessionIDArg(args map[string]any) (string, error) {
	raw, _ := args["session_id"].(string)
	if raw == "" {
		return "", nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid session_id: must be a valid UUID")
	}
	return parsed.String(), nil
}

// truncateText shortens a string to maxLen characters, appending "..." if truncated.
func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (s *Server) handleRemember(authCtx *auth.Context, args map[string]any) CallToolResult {
	fact, _ := args["fact"].(string)
	if fact == "" {
		return toolError("fact is required")
	}

	if findings := s.scanner.Scan(fact); len(findings) > 0 {
		s.logger.Warn("secret detected in memory payload",
			"user", authCtx.UserID.String(),
			"findings_count", len(findings),
		)
		return toolError(secrets.FormatError(findings))
	}

	memoryType, _ := args["memory_type"].(string)
	if memoryType == "" {
		memoryType = "factual"
	}
	if !validMemoryTypes[memoryType] {
		return toolError("memory_type must be 'factual', 'experiential', or 'working'")
	}

	scope, _ := args["scope"].(string)
	if scope == "" {
		scope = "personal"
	}
	if !validScopes[scope] {
		return toolError("scope must be 'personal' or 'org'")
	}

	var expiresAt pgtype.Timestamptz
	if memoryType == "working" {
		expiresAt = pgtype.Timestamptz{
			Time:  time.Now().Add(7 * 24 * time.Hour),
			Valid: true,
		}
	}

	name := sanitizeName(fact)

	var tagStrs []string
	if tags, ok := args["tags"].([]any); ok && len(tags) > 0 {
		for _, t := range tags {
			if ts, ok := t.(string); ok {
				tagStrs = append(tagStrs, strings.ToLower(strings.TrimSpace(ts)))
			}
		}
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	nextVersion, err := s.queries.GetNextMemoryBlockVersion(ctx, sqlc.GetNextMemoryBlockVersionParams{
		UserID: authCtx.UserID,
		Name:   name,
	})
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	block, err := s.queries.CreateMemoryBlock(ctx, sqlc.CreateMemoryBlockParams{
		UserID:     authCtx.UserID,
		Name:       name,
		Tier:       "index",
		Value:      pgtype.Text{String: fact, Valid: true},
		MemoryType: memoryType,
		Scope:      scope,
		ExpiresAt:  expiresAt,
		Version:    nextVersion,
		SortOrder:  0,
		Tags:       tagStrs,
		SessionID:  sessionToPgUUID(authCtx.SessionID),
	})
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	var nearDuplicateNotice string
	if s.embedder != nil && s.vectorDB != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		vec, err := s.embedder.Embed(embedCtx, fact)
		cancel()

		if err != nil {
			s.logger.Warn("failed to generate embedding",
				"error", err.Error(),
				"block_id", block.ID,
			)
		} else {
			// Check for near-duplicates before upserting
			similar, searchErr := s.vectorDB.Search(ctx, authCtx.UserID, authCtx.OrgID, vec, 3, nil)
			if searchErr != nil {
				s.logger.Warn("near-duplicate check failed",
					"error", searchErr.Error(),
					"block_id", block.ID,
				)
			} else {
				for _, r := range similar {
					if r.Score >= nearDuplicateThreshold && r.BlockID != block.ID {
						nearDuplicateNotice = fmt.Sprintf(
							"\n\nNote: Similar memory exists (id=%d, similarity=%.0f%%): %s",
							r.BlockID, r.Score*100, truncateText(r.Text, 120),
						)
						break
					}
				}
			}

			// Upsert vector in background
			point := vector.Point{
				ID:         vector.MemoryPointID(authCtx.UserID, name),
				UserID:     authCtx.UserID,
				OrgID:      authCtx.OrgID,
				BlockID:    block.ID,
				Text:       fact,
				Scope:      scope,
				MemoryType: memoryType,
				Tags:       tagStrs,
				SessionID:  sessionToString(authCtx.SessionID),
				Vector:     vec,
			}
			s.goBackground(func() {
				upsertCtx, upsertCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer upsertCancel()
				if err := s.vectorDB.Upsert(upsertCtx, point); err != nil {
					s.logger.Warn("failed to store embedding",
						"error", err.Error(),
						"block_id", block.ID,
					)
				}
			})
		}
	}

	return toolResult(fmt.Sprintf("Remembered (id=%d): %s%s", block.ID, fact, nearDuplicateNotice))
}

// rankedResult holds a single search result with its source for RRF fusion.
type rankedResult struct {
	blockID int64
	scope   string
	text    string
	source  string // "semantic" or "keyword"
}

func (s *Server) handleRecall(authCtx *auth.Context, args map[string]any) CallToolResult {
	query, _ := args["query"].(string)
	if query == "" {
		return toolError("query is required")
	}

	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "hybrid"
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	memoryType, _ := args["memory_type"].(string)

	// Parse optional session_id filter
	sessionFilter, err := parseSessionIDArg(args)
	if err != nil {
		return toolError(err.Error())
	}

	// Parse tag filters
	var tagFilter []string
	if tags, ok := args["tags"].([]any); ok {
		for _, t := range tags {
			if ts, ok := t.(string); ok {
				tagFilter = append(tagFilter, strings.ToLower(strings.TrimSpace(ts)))
			}
		}
	}

	var semanticResults []rankedResult
	var keywordResults []rankedResult

	// Semantic search via vector DB
	if (mode == "semantic" || mode == "hybrid") && s.embedder != nil && s.vectorDB != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// Build vector search filter from memory_type, tags, and session_id
		var searchFilter *vector.SearchFilter
		if memoryType != "" || len(tagFilter) > 0 || sessionFilter != "" {
			searchFilter = &vector.SearchFilter{
				MemoryType: memoryType,
				Tags:       tagFilter,
				SessionID:  sessionFilter,
			}
		}

		queryVec, err := s.embedder.Embed(embedCtx, query)
		if err != nil {
			s.logger.Warn("failed to generate query embedding",
				"error", err.Error(),
			)
		} else {
			results, err := s.vectorDB.Search(embedCtx, authCtx.UserID, authCtx.OrgID, queryVec, uint64(limit), searchFilter)
			if err != nil {
				s.logger.Warn("vector search failed",
					"error", err.Error(),
				)
			} else {
				for _, r := range results {
					semanticResults = append(semanticResults, rankedResult{
						blockID: r.BlockID,
						scope:   r.Scope,
						text:    r.Text,
						source:  "semantic",
					})
				}
			}
		}
	}

	// Keyword search via PostgreSQL ILIKE + full-text search
	if mode == "keyword" || mode == "hybrid" {
		searchLimit := int32(limit)

		var blocks []sqlc.CurrentMemoryBlock
		var err error

		hasType := memoryType != ""
		hasTags := len(tagFilter) > 0

		switch {
		case hasType && hasTags:
			blocks, err = s.queries.SearchAccessibleMemoryBlocksByTypeAndTags(ctx, sqlc.SearchAccessibleMemoryBlocksByTypeAndTagsParams{
				UserID:      authCtx.UserID,
				Query:       pgtype.Text{String: query, Valid: true},
				MemoryType:  memoryType,
				FilterTags:  tagFilter,
				SearchLimit: searchLimit,
			})
		case hasType:
			blocks, err = s.queries.SearchAccessibleMemoryBlocksByType(ctx, sqlc.SearchAccessibleMemoryBlocksByTypeParams{
				UserID:      authCtx.UserID,
				Query:       pgtype.Text{String: query, Valid: true},
				MemoryType:  memoryType,
				SearchLimit: searchLimit,
			})
		case hasTags:
			blocks, err = s.queries.SearchAccessibleMemoryBlocksByTags(ctx, sqlc.SearchAccessibleMemoryBlocksByTagsParams{
				UserID:      authCtx.UserID,
				Query:       pgtype.Text{String: query, Valid: true},
				FilterTags:  tagFilter,
				SearchLimit: searchLimit,
			})
		default:
			blocks, err = s.queries.SearchAccessibleMemoryBlocks(ctx, sqlc.SearchAccessibleMemoryBlocksParams{
				UserID:      authCtx.UserID,
				Query:       pgtype.Text{String: query, Valid: true},
				SearchLimit: searchLimit,
			})
		}

		if err != nil {
			return toolError(fmt.Sprintf("Error: %v", err))
		}

		for _, block := range blocks {
			// App-side session_id filter for keyword results
			if sessionFilter != "" {
				if !block.SessionID.Valid || uuid.UUID(block.SessionID.Bytes).String() != sessionFilter {
					continue
				}
			}

			value := ""
			if block.Value.Valid {
				value = block.Value.String
			}
			keywordResults = append(keywordResults, rankedResult{
				blockID: block.ID,
				scope:   block.Scope,
				text:    value,
				source:  "keyword",
			})
		}
	}

	// Fuse results using Reciprocal Rank Fusion (RRF) when in hybrid mode
	var matches []string
	var recalledBlockIDs []int64
	if mode == "hybrid" && len(semanticResults) > 0 && len(keywordResults) > 0 {
		matches, recalledBlockIDs = fuseRRF(semanticResults, keywordResults, limit)
	} else {
		// Single-source mode or one source returned nothing
		for _, r := range semanticResults {
			matches = append(matches, fmt.Sprintf("[%d] [%s] (semantic) %s", r.blockID, r.scope, r.text))
			recalledBlockIDs = append(recalledBlockIDs, r.blockID)
		}
		for _, r := range keywordResults {
			matches = append(matches, fmt.Sprintf("[%d] [%s] (keyword) %s", r.blockID, r.scope, r.text))
			recalledBlockIDs = append(recalledBlockIDs, r.blockID)
		}
	}

	if len(matches) == 0 {
		return toolResult("No matching memories found")
	}

	if len(matches) > limit {
		matches = matches[:limit]
		recalledBlockIDs = recalledBlockIDs[:limit]
	}

	// Track recall hits asynchronously
	if len(recalledBlockIDs) > 0 {
		ids := recalledBlockIDs
		userID := authCtx.UserID
		s.goBackground(func() {
			trackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.queries.IncrementRecallCount(trackCtx, sqlc.IncrementRecallCountParams{
				BlockIds: ids,
				UserID:   userID,
			}); err != nil {
				s.logger.Warn("failed to increment recall count",
					"error", err.Error(),
				)
			}
		})
	}

	return toolResult(strings.Join(matches, "\n"))
}

// fuseRRF merges two ranked result lists using Reciprocal Rank Fusion.
// RRF score = sum of 1/(k + rank) for each list the result appears in.
// k=60 is the standard constant from the original RRF paper.
func fuseRRF(semantic, keyword []rankedResult, limit int) ([]string, []int64) {
	const k = 60.0

	type fusedEntry struct {
		blockID int64
		scope   string
		text    string
		score   float64
		sources string
	}

	entries := make(map[int64]*fusedEntry)

	for rank, r := range semantic {
		entries[r.blockID] = &fusedEntry{
			blockID: r.blockID,
			scope:   r.scope,
			text:    r.text,
			score:   1.0 / (k + float64(rank+1)),
			sources: "semantic",
		}
	}

	for rank, r := range keyword {
		if e, exists := entries[r.blockID]; exists {
			e.score += 1.0 / (k + float64(rank+1))
			e.sources = "hybrid"
		} else {
			entries[r.blockID] = &fusedEntry{
				blockID: r.blockID,
				scope:   r.scope,
				text:    r.text,
				score:   1.0 / (k + float64(rank+1)),
				sources: "keyword",
			}
		}
	}

	// Sort by fused score descending
	sorted := make([]*fusedEntry, 0, len(entries))
	for _, e := range entries {
		sorted = append(sorted, e)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].score > sorted[j].score
	})

	if len(sorted) > limit {
		sorted = sorted[:limit]
	}

	matches := make([]string, len(sorted))
	blockIDs := make([]int64, len(sorted))
	for i, e := range sorted {
		matches[i] = fmt.Sprintf("[%d] [%s] (%s) %s", e.blockID, e.scope, e.sources, e.text)
		blockIDs[i] = e.blockID
	}
	return matches, blockIDs
}

func (s *Server) handleForget(authCtx *auth.Context, args map[string]any) CallToolResult {
	factIDFloat, ok := args["fact_id"].(float64)
	if !ok {
		return toolError("fact_id is required")
	}
	factID := int64(factIDFloat)

	ctx := auth.WithContext(context.Background(), authCtx)

	block, err := s.queries.GetMemoryBlockByID(ctx, sqlc.GetMemoryBlockByIDParams{
		ID:     factID,
		UserID: authCtx.UserID,
	})
	if err != nil {
		return toolError(fmt.Sprintf("Memory with ID %d not found", factID))
	}

	if err := s.queries.DeleteMemoryBlock(ctx, sqlc.DeleteMemoryBlockParams{
		UserID: authCtx.UserID,
		Name:   block.Name,
	}); err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	if s.vectorDB != nil {
		pointID := vector.MemoryPointID(authCtx.UserID, block.Name)
		s.goBackground(func() {
			delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.vectorDB.Delete(delCtx, pointID); err != nil {
				s.logger.Warn("failed to delete embedding",
					"error", err.Error(),
					"point_id", pointID,
				)
			}
		})
	}

	return toolResult(fmt.Sprintf("Removed memory with ID %d", factID))
}

func (s *Server) handleListMemories(authCtx *auth.Context, args map[string]any) CallToolResult {
	ctx := auth.WithContext(context.Background(), authCtx)

	memoryType, _ := args["memory_type"].(string)

	// Parse optional session_id filter
	sessionFilter, err := parseSessionIDArg(args)
	if err != nil {
		return toolError(err.Error())
	}

	var blocks []sqlc.CurrentMemoryBlock

	if memoryType != "" {
		blocks, err = s.queries.GetAccessibleMemoryBlocksByType(ctx, sqlc.GetAccessibleMemoryBlocksByTypeParams{
			UserID:     authCtx.UserID,
			MemoryType: memoryType,
		})
	} else {
		blocks, err = s.queries.GetAccessibleMemoryBlocks(ctx, authCtx.UserID)
	}

	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	if len(blocks) == 0 {
		return toolResult("No memories found")
	}

	var tagFilter []string
	if tags, ok := args["tags"].([]any); ok {
		for _, t := range tags {
			if ts, ok := t.(string); ok {
				tagFilter = append(tagFilter, strings.ToLower(ts))
			}
		}
	}

	var lines []string
	for _, block := range blocks {
		if len(tagFilter) > 0 && !matchesTags(block.Tags, tagFilter) {
			continue
		}
		if sessionFilter != "" {
			if !block.SessionID.Valid || uuid.UUID(block.SessionID.Bytes).String() != sessionFilter {
				continue
			}
		}

		value := ""
		if block.Value.Valid {
			value = block.Value.String
		}

		lines = append(lines, fmt.Sprintf("[%d] [%s,%s] [%s] %s", block.ID, block.Name, block.Tier, block.Scope, value))
	}

	if len(lines) == 0 {
		return toolResult("No matching memories found")
	}

	return toolResult(strings.Join(lines, "\n"))
}

func (s *Server) handleShareMemory(authCtx *auth.Context, args map[string]any) CallToolResult {
	factID, ok := args["fact_id"].(float64)
	if !ok {
		return toolError("fact_id is required and must be a number")
	}

	scope, _ := args["scope"].(string)
	if scope == "" {
		return toolError("scope is required")
	}
	if !validScopes[scope] {
		return toolError("scope must be 'personal' or 'org'")
	}

	ctx := auth.WithContext(context.Background(), authCtx)

	block, err := s.queries.GetMemoryBlockByID(ctx, sqlc.GetMemoryBlockByIDParams{
		ID:     int64(factID),
		UserID: authCtx.UserID,
	})
	if err != nil {
		return toolError("Memory not found or you don't have permission to modify it")
	}

	updated, err := s.queries.UpdateMemoryBlockScope(ctx, sqlc.UpdateMemoryBlockScopeParams{
		UserID: authCtx.UserID,
		Name:   block.Name,
		Scope:  scope,
	})
	if err != nil {
		return toolError(fmt.Sprintf("Error updating scope: %v", err))
	}

	// Re-upsert the Qdrant point with updated scope metadata.
	if s.embedder != nil && s.vectorDB != nil {
		value := ""
		if updated.Value.Valid {
			value = updated.Value.String
		}
		s.goBackground(func() {
			embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vec, err := s.embedder.Embed(embedCtx, value)
			if err != nil {
				s.logger.Warn("failed to re-embed after scope change",
					"error", err.Error(),
					"block_id", updated.ID,
				)
				return
			}

			sessionID := ""
			if updated.SessionID.Valid {
				sessionID = uuid.UUID(updated.SessionID.Bytes).String()
			}

			point := vector.Point{
				ID:         vector.MemoryPointID(authCtx.UserID, block.Name),
				UserID:     authCtx.UserID,
				OrgID:      authCtx.OrgID,
				BlockID:    updated.ID,
				Text:       value,
				Scope:      scope,
				MemoryType: updated.MemoryType,
				Tags:       updated.Tags,
				SessionID:  sessionID,
				Vector:     vec,
			}

			if err := s.vectorDB.Upsert(embedCtx, point); err != nil {
				s.logger.Warn("failed to update embedding scope",
					"error", err.Error(),
					"block_id", updated.ID,
				)
			}
		})
	}

	scopeLabel := "personal"
	if scope == "org" {
		scopeLabel = "organization"
	}

	return toolResult(fmt.Sprintf("Memory #%d is now %s", int64(factID), scopeLabel))
}

func (s *Server) handleExportMemories(authCtx *auth.Context, args map[string]any) CallToolResult {
	ctx := auth.WithContext(context.Background(), authCtx)

	memories, err := s.queries.ExportMemories(ctx, authCtx.UserID)
	if err != nil {
		return toolError(fmt.Sprintf("Error exporting memories: %v", err))
	}

	memoryType, _ := args["memory_type"].(string)
	scope, _ := args["scope"].(string)
	since, _ := args["since"].(string)

	// Parse optional session_id filter
	sessionFilter, parseErr := parseSessionIDArg(args)
	if parseErr != nil {
		return toolError(parseErr.Error())
	}

	var sinceTime time.Time
	if since != "" {
		sinceTime, err = time.Parse(time.RFC3339, since)
		if err != nil {
			return toolError("Invalid since timestamp format (use RFC3339)")
		}
	}

	var tagFilter []string
	if tags, ok := args["tags"].([]any); ok {
		for _, t := range tags {
			if ts, ok := t.(string); ok {
				tagFilter = append(tagFilter, strings.ToLower(ts))
			}
		}
	}

	var jsonlLines []string
	for _, mem := range memories {
		if memoryType != "" && mem.MemoryType != memoryType {
			continue
		}
		if scope != "" && mem.Scope != scope {
			continue
		}
		if !sinceTime.IsZero() && mem.CreatedAt.Before(sinceTime) && mem.ModifiedAt.Before(sinceTime) {
			continue
		}
		if len(tagFilter) > 0 && !matchesTags(mem.Tags, tagFilter) {
			continue
		}
		if sessionFilter != "" {
			if !mem.SessionID.Valid || uuid.UUID(mem.SessionID.Bytes).String() != sessionFilter {
				continue
			}
		}

		content := ""
		if mem.Value.Valid {
			content = mem.Value.String
		}

		exportRecord := map[string]interface{}{
			"id":          mem.ID,
			"guid":        mem.Guid.String(),
			"user_id":     mem.UserID.String(),
			"org_id":      mem.OrgID.String(),
			"memory_type": mem.MemoryType,
			"scope":       mem.Scope,
			"tags":        mem.Tags,
			"content":     content,
			"created_at":  mem.CreatedAt.Format(time.RFC3339),
			"modified_at": mem.ModifiedAt.Format(time.RFC3339),
		}

		if mem.SessionID.Valid {
			exportRecord["session_id"] = uuid.UUID(mem.SessionID.Bytes).String()
		}
		if mem.ExpiresAt.Valid {
			exportRecord["expires_at"] = mem.ExpiresAt.Time.Format(time.RFC3339)
		}

		jsonBytes, err := json.Marshal(exportRecord)
		if err != nil {
			continue
		}
		jsonlLines = append(jsonlLines, string(jsonBytes))
	}

	if len(jsonlLines) == 0 {
		return toolResult("No memories match the specified filters")
	}

	output := strings.Join(jsonlLines, "\n") + "\n"
	return toolResult(fmt.Sprintf("Exported %d memories:\n\n%s", len(jsonlLines), output))
}

// Tag parsing helpers

// matchesTags returns true if tags contains all entries in filter (AND logic).
func matchesTags(tags []string, filter []string) bool {
	if len(tags) == 0 {
		return false
	}
	tagSet := make(map[string]bool, len(tags))
	for _, t := range tags {
		tagSet[strings.ToLower(t)] = true
	}
	for _, f := range filter {
		if !tagSet[f] {
			return false
		}
	}
	return true
}

func sanitizeName(s string) string {
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
