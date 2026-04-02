package handler

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SystemStatsHandler handles system infrastructure statistics.
type SystemStatsHandler struct {
	pool *pgxpool.Pool
}

// NewSystemStatsHandler creates a new system stats handler.
func NewSystemStatsHandler(pool *pgxpool.Pool) *SystemStatsHandler {
	return &SystemStatsHandler{pool: pool}
}

// PostgresStats holds PostgreSQL connection pool statistics.
type PostgresStats struct {
	TotalConns           int32 `json:"total_conns"`
	AcquiredConns        int32 `json:"acquired_conns"`
	IdleConns            int32 `json:"idle_conns"`
	MaxConns             int32 `json:"max_conns"`
	AcquireCount         int64 `json:"acquire_count"`
	EmptyAcquireCount    int64 `json:"empty_acquire_count"`
	CanceledAcquireCount int64 `json:"canceled_acquire_count"`
}

// MemoryStats holds mneme statistics from pg_ghola.
type MemoryStats struct {
	UsersWithMemories int64 `json:"users_with_memories"`
	TotalMemoryBlocks int64 `json:"total_memory_blocks"`
	TotalContentBytes int64 `json:"total_content_bytes"`
	UniqueMemoryNames int64 `json:"unique_memory_names"`
}

// MemoryTypeDistribution represents count of memories by type.
type MemoryTypeDistribution struct {
	MemoryType string `json:"memory_type"`
	Count      int64  `json:"count"`
}

// TopTag represents a tag and its usage count.
type TopTag struct {
	Tag   string `json:"tag"`
	Count int64  `json:"count"`
}

// MemoryScopeDistribution represents count of memories by scope.
type MemoryScopeDistribution struct {
	Scope string `json:"scope"`
	Count int64  `json:"count"`
}

// SystemStatsResponse represents the full system statistics.
type SystemStatsResponse struct {
	Postgres *PostgresStats `json:"postgres"`
	Memory   *MemoryStats   `json:"memory,omitempty"`
}

// GetSystemStats handles GET /api/v1/admin/system-stats
func (h *SystemStatsHandler) GetSystemStats(w http.ResponseWriter, r *http.Request) {
	resp := &SystemStatsResponse{}

	if h.pool != nil {
		poolStats := h.pool.Stat()
		resp.Postgres = &PostgresStats{
			TotalConns:           poolStats.TotalConns(),
			AcquiredConns:        poolStats.AcquiredConns(),
			IdleConns:            poolStats.IdleConns(),
			MaxConns:             poolStats.MaxConns(),
			AcquireCount:         poolStats.AcquireCount(),
			EmptyAcquireCount:    poolStats.EmptyAcquireCount(),
			CanceledAcquireCount: poolStats.CanceledAcquireCount(),
		}

		stats, err := queryMemoryStats(r.Context(), h.pool)
		if err == nil {
			resp.Memory = stats
		}
	}

	OK(w, resp)
}

// GetMemoryTypeDistribution handles GET /api/v1/admin/memory-type-distribution
func (h *SystemStatsHandler) GetMemoryTypeDistribution(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), `
		SELECT memory_type, COUNT(*)::bigint AS count
		FROM pg_ghola.mnemes
		WHERE state = 'active'
		  AND memory_type IS NOT NULL
		GROUP BY memory_type
		ORDER BY count DESC
	`)
	if err != nil {
		Error(w, err)
		return
	}
	defer rows.Close()

	result := make([]MemoryTypeDistribution, 0)
	for rows.Next() {
		var d MemoryTypeDistribution
		if err := rows.Scan(&d.MemoryType, &d.Count); err != nil {
			Error(w, err)
			return
		}
		result = append(result, d)
	}

	OK(w, result)
}

// GetTopTags handles GET /api/v1/admin/top-tags
func (h *SystemStatsHandler) GetTopTags(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT tag, COUNT(*)::bigint AS count
		FROM (
			SELECT unnest(tags) AS tag
			FROM pg_ghola.mnemes
			WHERE state = 'active'
			  AND array_length(tags, 1) > 0
		) AS tags_extracted
		WHERE tag != ''
		GROUP BY tag
		ORDER BY count DESC
		LIMIT $1
	`, limit)
	if err != nil {
		Error(w, err)
		return
	}
	defer rows.Close()

	result := make([]TopTag, 0)
	for rows.Next() {
		var t TopTag
		if err := rows.Scan(&t.Tag, &t.Count); err != nil {
			Error(w, err)
			return
		}
		result = append(result, t)
	}

	OK(w, result)
}

// GetMemoryScopeDistribution handles GET /api/v1/admin/memory-scope-distribution
func (h *SystemStatsHandler) GetMemoryScopeDistribution(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), `
		SELECT scope, COUNT(*)::bigint AS count
		FROM pg_ghola.mnemes
		WHERE state = 'active'
		  AND scope IS NOT NULL
		GROUP BY scope
		ORDER BY count DESC
	`)
	if err != nil {
		Error(w, err)
		return
	}
	defer rows.Close()

	result := make([]MemoryScopeDistribution, 0)
	for rows.Next() {
		var d MemoryScopeDistribution
		if err := rows.Scan(&d.Scope, &d.Count); err != nil {
			Error(w, err)
			return
		}
		result = append(result, d)
	}

	OK(w, result)
}

func queryMemoryStats(ctx context.Context, pool *pgxpool.Pool) (*MemoryStats, error) {
	var stats MemoryStats
	err := pool.QueryRow(ctx, `
		SELECT
			COUNT(DISTINCT workspace_id)::bigint AS users_with_memories,
			COUNT(*)::bigint AS total_memory_blocks,
			COALESCE(SUM(LENGTH(content)), 0)::bigint AS total_content_bytes,
			COUNT(DISTINCT concept)::bigint AS unique_memory_names
		FROM pg_ghola.mnemes
		WHERE state = 'active'
	`).Scan(
		&stats.UsersWithMemories,
		&stats.TotalMemoryBlocks,
		&stats.TotalContentBytes,
		&stats.UniqueMemoryNames,
	)
	if err != nil {
		return nil, err
	}
	return &stats, nil
}
