package core_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/logan-broit/ghola/internal/core"
)

// TestFuseRRF_SingleList verifies that a single non-empty list is
// returned in its input order — RRF over one tier degenerates to that
// tier's ranking. This is the load-bearing case for production until
// the semantic tier (PR-E) is populated; the bench team relies on this
// to argue B.1 doesn't regress today's behavior.
func TestFuseRRF_SingleList(t *testing.T) {
	got := core.FuseRRF([][]string{{"a", "b", "c"}}, 60)
	require := assert.New(t)
	require.Len(got, 3)
	require.Equal("a", got[0].ID)
	require.Equal("b", got[1].ID)
	require.Equal("c", got[2].ID)
	// Rank 1 -> 1/(60+1), rank 2 -> 1/62, rank 3 -> 1/63
	require.InDelta(1.0/61.0, got[0].Score, 1e-12)
	require.InDelta(1.0/62.0, got[1].Score, 1e-12)
	require.InDelta(1.0/63.0, got[2].Score, 1e-12)
}

// TestFuseRRF_TwoLists exercises the additive property: a doc that
// shows up in both lists at rank 1 should score highest.
func TestFuseRRF_TwoLists(t *testing.T) {
	got := core.FuseRRF([][]string{
		{"x", "y", "z"},
		{"y", "x", "w"},
	}, 60)
	require := assert.New(t)

	// Lookup by id for ordering-independent assertions on the math.
	scoreOf := map[string]float64{}
	for _, sd := range got {
		scoreOf[sd.ID] = sd.Score
	}
	require.Len(got, 4) // x, y, z, w
	// x: 1/61 + 1/62
	require.InDelta(1.0/61.0+1.0/62.0, scoreOf["x"], 1e-12)
	// y: 1/62 + 1/61
	require.InDelta(1.0/62.0+1.0/61.0, scoreOf["y"], 1e-12)
	// z: 1/63 only
	require.InDelta(1.0/63.0, scoreOf["z"], 1e-12)
	// w: 1/63 only
	require.InDelta(1.0/63.0, scoreOf["w"], 1e-12)

	// x and y tied; first-seen tiebreak puts x before y (x came first
	// in list 1). Then z and w tied at 1/63; z came first (list 1).
	require.Equal([]string{"x", "y", "z", "w"}, idsOf(got))
}

// TestFuseRRF_KControlsTopWeight confirms smaller k makes the top
// ranks dominate more aggressively. The shape is sigmoid-flat for
// k=60, sharper for k=1.
func TestFuseRRF_KControlsTopWeight(t *testing.T) {
	list := [][]string{{"a", "b"}}
	hi := core.FuseRRF(list, 60)
	lo := core.FuseRRF(list, 1)
	// Ratio of top-to-second score is bigger when k is smaller.
	hiRatio := hi[0].Score / hi[1].Score
	loRatio := lo[0].Score / lo[1].Score
	assert.Greater(t, loRatio, hiRatio)
	// k=1 -> 1/2 / 1/3 = 1.5; k=60 -> 1/61 / 1/62 ≈ 1.016.
	assert.InDelta(t, 1.5, loRatio, 1e-12)
	assert.InDelta(t, 62.0/61.0, hiRatio, 1e-12)
}

// TestFuseRRF_DefaultsKWhenInvalid pins the safety net: k <= 0 falls
// back to 60. Avoids div-by-zero / negative-rank surprises if a caller
// forgets to set Core.RRFK.
func TestFuseRRF_DefaultsKWhenInvalid(t *testing.T) {
	got0 := core.FuseRRF([][]string{{"a"}}, 0)
	got60 := core.FuseRRF([][]string{{"a"}}, 60)
	assert.True(t, math.Abs(got0[0].Score-got60[0].Score) < 1e-12)
}

// TestFuseRRF_EmptyInputs covers both no-lists and empty-list cases.
func TestFuseRRF_EmptyInputs(t *testing.T) {
	assert.Empty(t, core.FuseRRF(nil, 60))
	assert.Empty(t, core.FuseRRF([][]string{}, 60))
	assert.Empty(t, core.FuseRRF([][]string{{}}, 60))
	assert.Empty(t, core.FuseRRF([][]string{{}, {}}, 60))
}

func idsOf(sds []core.ScoredDoc) []string {
	out := make([]string, len(sds))
	for i, sd := range sds {
		out[i] = sd.ID
	}
	return out
}
