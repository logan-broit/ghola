package chapterhouse

import "testing"

// TestRequestTimeout_ConsolidatePathGetsLongDeadline pins Finding 1b's
// timeout-selection helper on the daemon->chapterhouse hop: the
// consolidate call blocks for the batch's full duration (tens of seconds
// to minutes), so it must get the generous per-request deadline instead of
// the shared 30s that governs every other call.
func TestRequestTimeout_ConsolidatePathGetsLongDeadline(t *testing.T) {
	if got := requestTimeout("/v1/semantic/consolidate"); got != consolidateTimeout {
		t.Fatalf("consolidate path timeout = %s, want %s", got, consolidateTimeout)
	}
	if got := requestTimeout("/v1/episodic/query"); got != defaultRequestTimeout {
		t.Fatalf("default path timeout = %s, want %s", got, defaultRequestTimeout)
	}
}
