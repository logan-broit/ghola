package vector

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/google/uuid"
	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	grpcinsecure "google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Client wraps the Qdrant gRPC client with convenience methods.
type Client struct {
	conn       *grpc.ClientConn
	points     pb.PointsClient
	collection string
	dimensions uint64
}

// Config holds Qdrant client configuration.
type Config struct {
	Host       string
	GRPCPort   int
	APIKey     string
	UseTLS     bool
	Collection string
	Dimensions int
}

// chapterhouseNamespace is a fixed UUID v5 namespace for generating deterministic
// Qdrant point IDs from (user_id, memory_name) pairs.
var chapterhouseNamespace = uuid.MustParse("7b8e9f0a-1c2d-4e5f-8a9b-0c1d2e3f4a5b")

// MemoryPointID returns a deterministic UUID for a memory's vector point.
// The same (userID, name) pair always produces the same UUID, so Qdrant
// upserts naturally overwrite previous versions of the same logical memory.
func MemoryPointID(userID uuid.UUID, name string) string {
	return uuid.NewSHA1(chapterhouseNamespace, []byte(userID.String()+":"+name)).String()
}

// Point represents a vector with metadata for storage.
type Point struct {
	ID      string
	UserID  uuid.UUID
	OrgID   uuid.UUID
	BlockID int64
	Text    string
	Scope   string // "personal" or "org"
	Vector  []float32
}

// SearchResult represents a search match.
type SearchResult struct {
	BlockID int64
	Score   float32
	Text    string
	Scope   string
}

// NewClient creates a new Qdrant client.
func NewClient(cfg Config) (*Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.GRPCPort)

	var opts []grpc.DialOption

	if cfg.UseTLS {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(grpcinsecure.NewCredentials()))
	}

	if cfg.APIKey != "" {
		opts = append(opts, grpc.WithUnaryInterceptor(apiKeyInterceptor(cfg.APIKey)))
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Qdrant: %w", err)
	}

	return &Client{
		conn:       conn,
		points:     pb.NewPointsClient(conn),
		collection: cfg.Collection,
		dimensions: uint64(cfg.Dimensions),
	}, nil
}

// apiKeyInterceptor returns a gRPC unary interceptor that attaches the API key
// as metadata on every request.
func apiKeyInterceptor(apiKey string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "api-key", apiKey)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// EnsureCollection creates the collection if it doesn't exist and ensures
// all required payload indexes are present.
func (c *Client) EnsureCollection(ctx context.Context) error {
	collections := pb.NewCollectionsClient(c.conn)

	_, err := collections.Get(ctx, &pb.GetCollectionInfoRequest{
		CollectionName: c.collection,
	})
	if err != nil {
		_, err = collections.Create(ctx, &pb.CreateCollection{
			CollectionName: c.collection,
			VectorsConfig: &pb.VectorsConfig{
				Config: &pb.VectorsConfig_Params{
					Params: &pb.VectorParams{
						Size:     c.dimensions,
						Distance: pb.Distance_Cosine,
					},
				},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to create collection: %w", err)
		}
	}

	// Ensure payload indexes exist (idempotent — Qdrant ignores if already present)
	indexes := []struct {
		name      string
		fieldType pb.FieldType
	}{
		{"user_id", pb.FieldType_FieldTypeKeyword},
		{"block_id", pb.FieldType_FieldTypeInteger},
		{"scope", pb.FieldType_FieldTypeKeyword},
		{"org_id", pb.FieldType_FieldTypeKeyword},
	}
	for _, idx := range indexes {
		_, err = c.points.CreateFieldIndex(ctx, &pb.CreateFieldIndexCollection{
			CollectionName: c.collection,
			FieldName:      idx.name,
			FieldType:      idx.fieldType.Enum(),
		})
		if err != nil {
			return fmt.Errorf("failed to create %s index: %w", idx.name, err)
		}
	}

	return nil
}

// Upsert stores or updates a vector point.
func (c *Client) Upsert(ctx context.Context, point Point) error {
	pointID := pb.NewIDUUID(point.ID)

	_, err := c.points.Upsert(ctx, &pb.UpsertPoints{
		CollectionName: c.collection,
		Points: []*pb.PointStruct{
			{
				Id: pointID,
				Vectors: &pb.Vectors{
					VectorsOptions: &pb.Vectors_Vector{
						Vector: &pb.Vector{Data: point.Vector},
					},
				},
				Payload: map[string]*pb.Value{
					"user_id":  pb.NewValueString(point.UserID.String()),
					"org_id":   pb.NewValueString(point.OrgID.String()),
					"block_id": pb.NewValueInt(point.BlockID),
					"text":     pb.NewValueString(point.Text),
					"scope":    pb.NewValueString(point.Scope),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to upsert point: %w", err)
	}

	return nil
}

// UpsertBatch stores multiple vector points.
func (c *Client) UpsertBatch(ctx context.Context, points []Point) error {
	if len(points) == 0 {
		return nil
	}

	pbPoints := make([]*pb.PointStruct, len(points))
	for i, p := range points {
		pbPoints[i] = &pb.PointStruct{
			Id: pb.NewIDUUID(p.ID),
			Vectors: &pb.Vectors{
				VectorsOptions: &pb.Vectors_Vector{
					Vector: &pb.Vector{Data: p.Vector},
				},
			},
			Payload: map[string]*pb.Value{
				"user_id":  pb.NewValueString(p.UserID.String()),
				"org_id":   pb.NewValueString(p.OrgID.String()),
				"block_id": pb.NewValueInt(p.BlockID),
				"text":     pb.NewValueString(p.Text),
				"scope":    pb.NewValueString(p.Scope),
			},
		}
	}

	_, err := c.points.Upsert(ctx, &pb.UpsertPoints{
		CollectionName: c.collection,
		Points:         pbPoints,
	})
	if err != nil {
		return fmt.Errorf("failed to upsert batch: %w", err)
	}

	return nil
}

// Search finds similar vectors for a user, including org-scoped memories from the same org.
func (c *Client) Search(ctx context.Context, userID uuid.UUID, orgID uuid.UUID, vector []float32, limit uint64) ([]SearchResult, error) {
	resp, err := c.points.Search(ctx, &pb.SearchPoints{
		CollectionName: c.collection,
		Vector:         vector,
		Limit:          limit,
		WithPayload: &pb.WithPayloadSelector{
			SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true},
		},
		Filter: &pb.Filter{
			// (user_id = X) OR (org_id = Y AND scope = 'org')
			Should: []*pb.Condition{
				{
					ConditionOneOf: &pb.Condition_Filter{
						Filter: &pb.Filter{
							Must: []*pb.Condition{
								{
									ConditionOneOf: &pb.Condition_Field{
										Field: &pb.FieldCondition{
											Key: "user_id",
											Match: &pb.Match{
												MatchValue: &pb.Match_Keyword{
													Keyword: userID.String(),
												},
											},
										},
									},
								},
							},
						},
					},
				},
				{
					ConditionOneOf: &pb.Condition_Filter{
						Filter: &pb.Filter{
							Must: []*pb.Condition{
								{
									ConditionOneOf: &pb.Condition_Field{
										Field: &pb.FieldCondition{
											Key: "org_id",
											Match: &pb.Match{
												MatchValue: &pb.Match_Keyword{
													Keyword: orgID.String(),
												},
											},
										},
									},
								},
								{
									ConditionOneOf: &pb.Condition_Field{
										Field: &pb.FieldCondition{
											Key: "scope",
											Match: &pb.Match{
												MatchValue: &pb.Match_Keyword{
													Keyword: "org",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search: %w", err)
	}

	results := make([]SearchResult, len(resp.Result))
	for i, r := range resp.Result {
		blockID := int64(0)
		text := ""
		scope := "personal"

		if v, ok := r.Payload["block_id"]; ok {
			if iv, ok := v.Kind.(*pb.Value_IntegerValue); ok {
				blockID = iv.IntegerValue
			}
		}
		if v, ok := r.Payload["text"]; ok {
			if sv, ok := v.Kind.(*pb.Value_StringValue); ok {
				text = sv.StringValue
			}
		}
		if v, ok := r.Payload["scope"]; ok {
			if sv, ok := v.Kind.(*pb.Value_StringValue); ok {
				scope = sv.StringValue
			}
		}

		results[i] = SearchResult{
			BlockID: blockID,
			Score:   r.Score,
			Text:    text,
			Scope:   scope,
		}
	}

	return results, nil
}

// Delete removes a vector point by its UUID.
func (c *Client) Delete(ctx context.Context, pointID string) error {
	_, err := c.points.Delete(ctx, &pb.DeletePoints{
		CollectionName: c.collection,
		Points: &pb.PointsSelector{
			PointsSelectorOneOf: &pb.PointsSelector_Points{
				Points: &pb.PointsIdsList{
					Ids: []*pb.PointId{pb.NewIDUUID(pointID)},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to delete point: %w", err)
	}

	return nil
}

// RecreateCollection drops and recreates the collection, clearing all points.
// Used by the reindex command to purge stale data before rebuilding.
func (c *Client) RecreateCollection(ctx context.Context) error {
	collections := pb.NewCollectionsClient(c.conn)

	_, err := collections.Delete(ctx, &pb.DeleteCollection{
		CollectionName: c.collection,
	})
	if err != nil {
		return fmt.Errorf("failed to delete collection: %w", err)
	}

	return c.EnsureCollection(ctx)
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// CollectionStats holds statistics about a Qdrant collection.
type CollectionStats struct {
	VectorsCount   uint64 `json:"vectors_count"`
	PointsCount    uint64 `json:"points_count"`
	SegmentsCount  uint64 `json:"segments_count"`
	Status         string `json:"status"`
	DiskDataSizeKB uint64 `json:"disk_data_size_kb"`
	RAMDataSizeKB  uint64 `json:"ram_data_size_kb"`
}

// GetCollectionStats retrieves statistics about the collection.
func (c *Client) GetCollectionStats(ctx context.Context) (*CollectionStats, error) {
	collections := pb.NewCollectionsClient(c.conn)

	info, err := collections.Get(ctx, &pb.GetCollectionInfoRequest{
		CollectionName: c.collection,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get collection info: %w", err)
	}

	result := info.GetResult()
	if result == nil {
		return nil, fmt.Errorf("no collection info returned")
	}

	stats := &CollectionStats{
		VectorsCount:  result.GetIndexedVectorsCount(),
		PointsCount:   result.GetPointsCount(),
		SegmentsCount: result.GetSegmentsCount(),
		Status:        result.GetStatus().String(),
	}

	// Estimate memory usage based on vector dimensions
	// Each float32 is 4 bytes
	stats.DiskDataSizeKB = result.GetIndexedVectorsCount() * c.dimensions * 4 / 1024
	stats.RAMDataSizeKB = result.GetPointsCount() * c.dimensions * 4 / 1024

	return stats, nil
}
