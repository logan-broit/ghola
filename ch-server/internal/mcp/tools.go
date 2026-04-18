package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mneme"
	"github.com/thinkwright/chapterhouse/ch-server/internal/secrets"

	"github.com/google/uuid"
)

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

// parseSessionIDArg validates an optional session_id argument string.
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

// parseSessionIDPtr returns a *uuid.UUID from an optional session_id arg.
func parseSessionIDPtr(args map[string]any) (*uuid.UUID, error) {
	raw, _ := args["session_id"].(string)
	if raw == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid session_id: must be a valid UUID")
	}
	return &parsed, nil
}

// resolveSessionID returns the explicit arg session, or falls back to transport session.
func resolveSessionID(authCtx *auth.Context, args map[string]any) (*uuid.UUID, error) {
	ptr, err := parseSessionIDPtr(args)
	if err != nil {
		return nil, err
	}
	if ptr != nil {
		return ptr, nil
	}
	if authCtx.SessionID != uuid.Nil {
		id := authCtx.SessionID
		return &id, nil
	}
	return nil, nil
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

	var tagStrs []string
	if tags, ok := args["tags"].([]any); ok && len(tags) > 0 {
		for _, t := range tags {
			if ts, ok := t.(string); ok {
				tagStrs = append(tagStrs, strings.ToLower(strings.TrimSpace(ts)))
			}
		}
	}

	sessionID, err := resolveSessionID(authCtx, args)
	if err != nil {
		return toolError(err.Error())
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	m, dup, err := s.store.Remember(ctx, authCtx.UserID, authCtx.OrgID, fact, memoryType, scope, "index", tagStrs, sessionID)
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	var nearDuplicateNotice string
	if dup != nil {
		nearDuplicateNotice = fmt.Sprintf(
			"\n\nNote: Similar memory exists (id=%s, similarity=%.0f%%): %s",
			dup.ID, dup.Similarity*100, truncateText(dup.Content, 120),
		)
	}

	return toolResult(fmt.Sprintf("Remembered (id=%s): %s%s", m.ID, fact, nearDuplicateNotice))
}

// parseTurnsArg pulls the turns array out of MCP args and validates each
// element shape. It does NOT check reconstruction against session_text --
// that's mneme.validateTurnReconstruction's job, called by RememberWithTurns.
// This step just normalizes the incoming JSON into typed TurnInput values
// and assigns char offsets based on cumulative content length.
func parseTurnsArg(raw any) ([]mneme.TurnInput, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("turns must be an array")
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("turns must have at least one entry")
	}
	result := make([]mneme.TurnInput, 0, len(list))
	cursor := 0
	for i, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("turn %d is not an object", i)
		}
		role, _ := obj["role"].(string)
		if role == "" {
			return nil, fmt.Errorf("turn %d missing role", i)
		}
		content, contentOK := obj["content"].(string)
		if !contentOK {
			return nil, fmt.Errorf("turn %d missing content", i)
		}
		if content == "" {
			return nil, fmt.Errorf("turn %d content is empty", i)
		}
		start := cursor
		end := cursor + len(content)
		result = append(result, mneme.TurnInput{
			Role:      role,
			Content:   content,
			CharStart: start,
			CharEnd:   end,
		})
		cursor = end
	}
	return result, nil
}

func (s *Server) handleRememberSession(authCtx *auth.Context, args map[string]any) CallToolResult {
	sessionText, _ := args["session_text"].(string)
	if sessionText == "" {
		return toolError("session_text is required")
	}

	turnsRaw, hasTurns := args["turns"]
	if !hasTurns {
		return toolError("turns is required")
	}
	turns, err := parseTurnsArg(turnsRaw)
	if err != nil {
		return toolError(err.Error())
	}

	// Run secret scan on full session_text before storing. If we find
	// anything, refuse the whole call -- don't store a partial session.
	if findings := s.scanner.Scan(sessionText); len(findings) > 0 {
		s.logger.Warn("secret detected in session payload",
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

	var tagStrs []string
	if tags, ok := args["tags"].([]any); ok && len(tags) > 0 {
		for _, t := range tags {
			if ts, ok := t.(string); ok {
				tagStrs = append(tagStrs, strings.ToLower(strings.TrimSpace(ts)))
			}
		}
	}

	sessionID, err := resolveSessionID(authCtx, args)
	if err != nil {
		return toolError(err.Error())
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	m, dup, err := s.store.RememberWithTurns(
		ctx, authCtx.UserID, authCtx.OrgID,
		sessionText, turns,
		memoryType, scope, "index", tagStrs, sessionID,
	)
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	var nearDuplicateNotice string
	if dup != nil {
		nearDuplicateNotice = fmt.Sprintf(
			"\n\nNote: Similar session exists (id=%s, similarity=%.0f%%): %s",
			dup.ID, dup.Similarity*100, truncateText(dup.Content, 120),
		)
	}

	preview := truncateText(sessionText, 120)
	return toolResult(fmt.Sprintf(
		"Remembered session (id=%s, turns=%d): %s%s",
		m.ID, len(turns), preview, nearDuplicateNotice,
	))
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

	memoryType, _ := args["memory_type"].(string)

	var tagFilter []string
	if tags, ok := args["tags"].([]any); ok {
		for _, t := range tags {
			if ts, ok := t.(string); ok {
				tagFilter = append(tagFilter, strings.ToLower(strings.TrimSpace(ts)))
			}
		}
	}

	sessionID, err := parseSessionIDPtr(args)
	if err != nil {
		return toolError(err.Error())
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	results, err := s.store.Recall(ctx, authCtx.UserID, authCtx.OrgID, query, limit, mode, memoryType, tagFilter, sessionID)
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	if len(results) == 0 {
		return toolResult("No matching memories found")
	}

	var matches []string
	var mnemeIDs []uuid.UUID
	for _, r := range results {
		matches = append(matches, fmt.Sprintf("[%s] [%s] (score=%.2f) %s", r.MnemeID, r.Scope, r.Score, r.Content))
		mnemeIDs = append(mnemeIDs, r.MnemeID)
	}

	// Score-weighted confirmation in background (Bayesian confidence update)
	// Only confirm mnemes with score >= 0.3 to skip noise.
	var filteredIDs []uuid.UUID
	var filteredScores []float64
	for i, r := range results {
		if r.Score >= 0.3 {
			filteredIDs = append(filteredIDs, mnemeIDs[i])
			filteredScores = append(filteredScores, r.Score)
		}
	}
	if len(filteredIDs) > 0 {
		ids := filteredIDs
		scores := filteredScores
		s.goBackground(func() {
			trackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.store.WeightedConfirmRecall(trackCtx, ids, scores); err != nil {
				s.logger.Warn("failed to confirm recall",
					"error", err.Error(),
				)
			}
		})
	}

	return toolResult(strings.Join(matches, "\n"))
}

func (s *Server) handleForget(authCtx *auth.Context, args map[string]any) CallToolResult {
	factIDStr, ok := args["fact_id"].(string)
	if !ok || factIDStr == "" {
		return toolError("fact_id is required")
	}

	mnemeID, err := uuid.Parse(factIDStr)
	if err != nil {
		return toolError("fact_id must be a valid UUID")
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	if err := s.store.Forget(ctx, authCtx.UserID, authCtx.OrgID, mnemeID); err != nil {
		return toolError(err.Error())
	}

	return toolResult(fmt.Sprintf("Removed memory %s", mnemeID))
}

func (s *Server) handleListMemories(authCtx *auth.Context, args map[string]any) CallToolResult {
	memoryType, _ := args["memory_type"].(string)
	limit := 50
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	var tagFilter []string
	if tags, ok := args["tags"].([]any); ok {
		for _, t := range tags {
			if ts, ok := t.(string); ok {
				tagFilter = append(tagFilter, strings.ToLower(ts))
			}
		}
	}

	sessionID, err := parseSessionIDPtr(args)
	if err != nil {
		return toolError(err.Error())
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	mnemes, err := s.store.List(ctx, authCtx.UserID, authCtx.OrgID, memoryType, tagFilter, sessionID, limit)
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	if len(mnemes) == 0 {
		return toolResult("No memories found")
	}

	var lines []string
	for _, m := range mnemes {
		lines = append(lines, fmt.Sprintf("[%s] [%s] [%s] %s", m.ID, m.MemoryType, m.Scope, m.Content))
	}

	return toolResult(strings.Join(lines, "\n"))
}

func (s *Server) handleShareMemory(authCtx *auth.Context, args map[string]any) CallToolResult {
	factIDStr, ok := args["fact_id"].(string)
	if !ok || factIDStr == "" {
		return toolError("fact_id is required")
	}

	mnemeID, err := uuid.Parse(factIDStr)
	if err != nil {
		return toolError("fact_id must be a valid UUID")
	}

	scope, _ := args["scope"].(string)
	if scope == "" {
		return toolError("scope is required")
	}
	if !validScopes[scope] {
		return toolError("scope must be 'personal' or 'org'")
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	if err := s.store.ChangeScope(ctx, authCtx.UserID, authCtx.OrgID, mnemeID, scope); err != nil {
		return toolError(err.Error())
	}

	scopeLabel := "personal"
	if scope == "org" {
		scopeLabel = "organization"
	}
	return toolResult(fmt.Sprintf("Memory %s is now %s", mnemeID, scopeLabel))
}

func (s *Server) handleExportMemories(authCtx *auth.Context, args map[string]any) CallToolResult {
	ctx := auth.WithContext(context.Background(), authCtx)

	allMnemes, err := s.store.Export(ctx, authCtx.UserID, authCtx.OrgID)
	if err != nil {
		return toolError(fmt.Sprintf("Error exporting memories: %v", err))
	}

	memoryType, _ := args["memory_type"].(string)
	scope, _ := args["scope"].(string)
	since, _ := args["since"].(string)

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
	for _, m := range allMnemes {
		if memoryType != "" && m.MemoryType != memoryType {
			continue
		}
		if scope != "" && m.Scope != scope {
			continue
		}
		if !sinceTime.IsZero() && m.CreatedAt.Before(sinceTime) {
			continue
		}
		if len(tagFilter) > 0 && !matchesTags(m.Tags, tagFilter) {
			continue
		}
		if sessionFilter != "" {
			if m.SessionID == nil || m.SessionID.String() != sessionFilter {
				continue
			}
		}

		exportRecord := map[string]interface{}{
			"id":          m.ID.String(),
			"memory_type": m.MemoryType,
			"scope":       m.Scope,
			"tags":        m.Tags,
			"content":     m.Content,
			"confidence":  m.Confidence,
			"created_at":  m.CreatedAt.Format(time.RFC3339),
		}

		if m.SessionID != nil {
			exportRecord["session_id"] = m.SessionID.String()
		}
		if m.ExpiresAt != nil {
			exportRecord["expires_at"] = m.ExpiresAt.Format(time.RFC3339)
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

var validRatings = map[string]float64{
	"positive": 0.85,
	"negative": 0.20,
	"wrong":    0.05,
}

func (s *Server) handleFeedback(authCtx *auth.Context, args map[string]any) CallToolResult {
	memoryIDStr, _ := args["memory_id"].(string)
	if memoryIDStr == "" {
		return toolError("memory_id is required")
	}

	mnemeID, err := uuid.Parse(memoryIDStr)
	if err != nil {
		return toolError("memory_id must be a valid UUID")
	}

	rating, _ := args["rating"].(string)
	evidence, ok := validRatings[rating]
	if !ok {
		return toolError("rating must be 'positive', 'negative', or 'wrong'")
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	newConf, err := s.store.FeedbackMemory(ctx, authCtx.UserID, authCtx.OrgID, mnemeID, evidence)
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	return toolResult(fmt.Sprintf("Feedback recorded for %s (%s). New confidence: %.3f", mnemeID, rating, newConf))
}

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

// relativeTime formats a time as a human-readable relative string.
func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 48*time.Hour:
		return "yesterday"
	default:
		days := int(d.Hours() / 24)
		if days < 7 {
			return fmt.Sprintf("%d days ago", days)
		}
		return t.Format("Jan 2, 2006")
	}
}

func (s *Server) handleListSessions(authCtx *auth.Context, args map[string]any) CallToolResult {
	limit := 10
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}

	ctx := auth.WithContext(context.Background(), authCtx)
	sessions, err := s.store.ListSessions(ctx, authCtx.UserID, authCtx.OrgID, limit)
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	if len(sessions) == 0 {
		return toolResult("No sessions with memories found")
	}

	var lines []string
	for _, sess := range sessions {
		lastActive := relativeTime(sess.LastActivity)
		duration := sess.LastActivity.Sub(sess.FirstActivity)

		durationStr := ""
		if duration < time.Minute {
			durationStr = "< 1 min"
		} else if duration < time.Hour {
			durationStr = fmt.Sprintf("%d min", int(duration.Minutes()))
		} else {
			durationStr = fmt.Sprintf("%.1f hrs", duration.Hours())
		}

		lines = append(lines, fmt.Sprintf("[%s] %d memories, %s duration, last active %s",
			sess.SessionID, sess.MemoryCount, durationStr, lastActive))
	}

	return toolResult(fmt.Sprintf("Found %d sessions:\n\n%s", len(lines), strings.Join(lines, "\n")))
}

func (s *Server) handleSessionSummary(authCtx *auth.Context, args map[string]any) CallToolResult {
	sessionIDStr, err := parseSessionIDArg(args)
	if err != nil {
		return toolError(err.Error())
	}
	if sessionIDStr == "" {
		return toolError("session_id is required")
	}

	parsed, _ := uuid.Parse(sessionIDStr)
	ctx := auth.WithContext(context.Background(), authCtx)
	memories, err := s.store.GetSessionMemories(ctx, authCtx.UserID, authCtx.OrgID, parsed)
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	if len(memories) == 0 {
		return toolResult("No memories found for this session")
	}

	return formatSessionSummary(sessionIDStr, memories)
}

func (s *Server) handleSessionContext(authCtx *auth.Context, args map[string]any) CallToolResult {
	sessionIDStr, err := parseSessionIDArg(args)
	if err != nil {
		return toolError(err.Error())
	}
	if sessionIDStr == "" {
		return toolError("session_id is required")
	}

	parsed, _ := uuid.Parse(sessionIDStr)
	ctx := auth.WithContext(context.Background(), authCtx)
	memories, err := s.store.GetSessionMemories(ctx, authCtx.UserID, authCtx.OrgID, parsed)
	if err != nil {
		return toolError(fmt.Sprintf("Error: %v", err))
	}

	if len(memories) == 0 {
		return toolResult("No memories found for this session")
	}

	return formatSessionContext(memories)
}

func formatSessionSummary(sessionID string, memories []mneme.Mneme) CallToolResult {
	first := memories[0].CreatedAt
	last := memories[len(memories)-1].CreatedAt
	for _, m := range memories {
		if m.CreatedAt.Before(first) {
			first = m.CreatedAt
		}
		if m.CreatedAt.After(last) {
			last = m.CreatedAt
		}
	}

	typeCounts := make(map[string]int)
	tagSet := make(map[string]bool)
	for _, m := range memories {
		typeCounts[m.MemoryType]++
		for _, t := range m.Tags {
			if t != "" {
				tagSet[t] = true
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Session %s\n", sessionID))
	sb.WriteString(fmt.Sprintf("Time: %s — %s (%s)\n",
		first.Format("Jan 2 15:04"), last.Format("15:04 MST"), relativeTime(last)))
	sb.WriteString(fmt.Sprintf("Memories: %d total", len(memories)))

	var typeParts []string
	for _, t := range []string{"factual", "experiential", "working"} {
		if c, ok := typeCounts[t]; ok {
			typeParts = append(typeParts, fmt.Sprintf("%d %s", c, t))
		}
	}
	if len(typeParts) > 0 {
		sb.WriteString(fmt.Sprintf(" (%s)", strings.Join(typeParts, ", ")))
	}
	sb.WriteString("\n")

	if len(tagSet) > 0 {
		var tags []string
		for t := range tagSet {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		sb.WriteString(fmt.Sprintf("Tags: %s\n", strings.Join(tags, ", ")))
	}

	sb.WriteString("\nMemories:\n")
	for _, m := range memories {
		sb.WriteString(fmt.Sprintf("  [%s] [%s] %s\n", m.ID, m.MemoryType, truncateText(m.Content, 120)))
	}

	return toolResult(sb.String())
}

func formatSessionContext(memories []mneme.Mneme) CallToolResult {
	groups := map[string][]mneme.Mneme{
		"factual":      {},
		"experiential": {},
		"working":      {},
	}
	for _, m := range memories {
		groups[m.MemoryType] = append(groups[m.MemoryType], m)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Session context (%d memories):\n", len(memories)))

	for _, memType := range []string{"factual", "experiential", "working"} {
		group := groups[memType]
		if len(group) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("\n## %s (%d)\n", strings.ToUpper(memType[:1])+memType[1:], len(group)))
		for _, m := range group {
			sb.WriteString(fmt.Sprintf("[%s] %s\n", m.ID, m.Content))
		}
	}

	return toolResult(sb.String())
}
