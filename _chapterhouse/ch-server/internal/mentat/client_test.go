package mentat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
)

func TestClient_Cluster_PureContract(t *testing.T) {
	id1, id2, id3 := uuid.New(), uuid.New(), uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/cluster", r.URL.Path)
		var req mentat.ClusterRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Len(t, req.IDs, 3)
		require.Len(t, req.Embeddings, 3)
		require.Equal(t, 3, req.MinClusterSize)
		_ = json.NewEncoder(w).Encode(mentat.ClusterResponse{
			Labels:   []int{0, 0, -1},
			Outliers: []string{req.IDs[2]},
		})
	}))
	defer srv.Close()

	c := mentat.NewClient(srv.URL, nil)
	resp, err := c.Cluster(context.Background(), mentat.ClusterRequest{
		IDs:            []string{id1.String(), id2.String(), id3.String()},
		Embeddings:     [][]float32{{0.1, 0.2}, {0.11, 0.19}, {9, 9}},
		MinClusterSize: 3,
	})
	require.NoError(t, err)
	require.Equal(t, []int{0, 0, -1}, resp.Labels)
	require.Equal(t, []string{id3.String()}, resp.Outliers)
}
