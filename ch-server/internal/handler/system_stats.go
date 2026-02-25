package handler

import (
	"context"
	"net/http"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"
	"github.com/thinkwright/chapterhouse/ch-server/internal/vector"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SystemStatsHandler handles system infrastructure statistics.
type SystemStatsHandler struct {
	pool    *pgxpool.Pool
	qdrant  *vector.Client
	queries *sqlc.Queries
}

// NewSystemStatsHandler creates a new system stats handler.
func NewSystemStatsHandler(pool *pgxpool.Pool, qdrant *vector.Client, queries *sqlc.Queries) *SystemStatsHandler {
	return &SystemStatsHandler{
		pool:    pool,
		qdrant:  qdrant,
		queries: queries,
	}
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

// QdrantStats holds Qdrant vector database statistics.
type QdrantStats struct {
	VectorsCount   uint64 `json:"vectors_count"`
	PointsCount    uint64 `json:"points_count"`
	SegmentsCount  uint64 `json:"segments_count"`
	Status         string `json:"status"`
	DiskDataSizeKB uint64 `json:"disk_data_size_kb"`
	RAMDataSizeKB  uint64 `json:"ram_data_size_kb"`
}

// MemoryStats holds memory block statistics.
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
	Qdrant   *QdrantStats   `json:"qdrant,omitempty"`
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
	}

	if h.qdrant != nil {
		qdrantStats, err := h.qdrant.GetCollectionStats(context.Background())
		if err == nil {
			resp.Qdrant = &QdrantStats{
				VectorsCount:   qdrantStats.VectorsCount,
				PointsCount:    qdrantStats.PointsCount,
				SegmentsCount:  qdrantStats.SegmentsCount,
				Status:         qdrantStats.Status,
				DiskDataSizeKB: qdrantStats.DiskDataSizeKB,
				RAMDataSizeKB:  qdrantStats.RAMDataSizeKB,
			}
		}
	}

	if h.queries != nil {
		memStats, err := h.queries.GetMemoryStats(r.Context())
		if err == nil {
			resp.Memory = &MemoryStats{
				UsersWithMemories: memStats.UsersWithMemories,
				TotalMemoryBlocks: memStats.TotalMemoryBlocks,
				TotalContentBytes: memStats.TotalContentBytes,
				UniqueMemoryNames: memStats.UniqueMemoryNames,
			}
		}
	}

	OK(w, resp)
}

// GetMemoryTypeDistribution handles GET /api/v1/admin/memory-type-distribution
func (h *SystemStatsHandler) GetMemoryTypeDistribution(w http.ResponseWriter, r *http.Request) {
	if h.queries == nil {
		Error(w, apierror.InternalError("Database queries not available"))
		return
	}

	rows, err := h.queries.GetMemoryTypeDistribution(r.Context())
	if err != nil {
		Error(w, apierror.InternalError("Failed to get memory type distribution").WithError(err))
		return
	}

	result := make([]MemoryTypeDistribution, 0, len(rows))
	for _, row := range rows {
		result = append(result, MemoryTypeDistribution{
			MemoryType: string(row.MemoryType),
			Count:      row.Count,
		})
	}

	OK(w, result)
}

// GetTopTags handles GET /api/v1/admin/top-tags
func (h *SystemStatsHandler) GetTopTags(w http.ResponseWriter, r *http.Request) {
	if h.queries == nil {
		Error(w, apierror.InternalError("Database queries not available"))
		return
	}

	// Default to top 20 tags
	limit := int32(20)

	rows, err := h.queries.GetTopTags(r.Context(), limit)
	if err != nil {
		Error(w, apierror.InternalError("Failed to get top tags").WithError(err))
		return
	}

	result := make([]TopTag, 0, len(rows))
	for _, row := range rows {
		result = append(result, TopTag{
			Tag:   row.Tag,
			Count: row.Count,
		})
	}

	OK(w, result)
}

// GetMemoryScopeDistribution handles GET /api/v1/admin/memory-scope-distribution
func (h *SystemStatsHandler) GetMemoryScopeDistribution(w http.ResponseWriter, r *http.Request) {
	if h.queries == nil {
		Error(w, apierror.InternalError("Database queries not available"))
		return
	}

	rows, err := h.queries.GetMemoryScopeDistribution(r.Context())
	if err != nil {
		Error(w, apierror.InternalError("Failed to get memory scope distribution").WithError(err))
		return
	}

	result := make([]MemoryScopeDistribution, 0, len(rows))
	for _, row := range rows {
		result = append(result, MemoryScopeDistribution{
			Scope: string(row.Scope),
			Count: row.Count,
		})
	}

	OK(w, result)
}
