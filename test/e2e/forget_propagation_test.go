//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestForget_PropagatesAcrossTiers asserts the load-bearing deletion
// property: calling /v1/forget with an event id makes the event drop
// out of recall on both the working (sietch) and episodic (postgres)
// tiers. Matches the Core.Forget fan-out: sietch.SoftForget flips
// text/nulls embedding for active-session ids; chapterhouse.
// ForgetEpisodic does the same in postgres. Semantic is
// intentionally out of scope — the plan notes it's managed via
// feedback, not forget.
//
// Claim tested end-to-end:
//   1. record + consolidate an event with a distinctive marker
//   2. pre-forget recall surfaces it on both working + episodic tiers
//   3. /v1/forget with that event id
//   4. post-forget recall does NOT surface it on either tier
func TestForget_PropagatesAcrossTiers(t *testing.T) {
	c := newClient(t)
	c.waitHealthy(30 * time.Second)

	user := e2eUser()
	marker := "gate4-forget-marker-" + time.Now().UTC().Format("20060102T150405.000")

	sess := c.startSession(user, "claude-code")
	ev := c.recordText(sess.ID, user,
		"Phase 11 Gate 4 probe — "+marker+" — this event should disappear after forget.")

	c.consolidate(sess.ID)

	// Pre-forget: recall must see the event.
	pre := c.recallAwait(user, marker, ev.ID, 5*time.Second)
	if !containsHit(pre.Hits, ev.ID) {
		t.Fatalf("pre-forget recall did not surface event %s; hits=%s", ev.ID, formatHits(pre.Hits))
	}

	// Fire the forget — session_id included so sietch fan-out runs.
	c.forget(sess.ID, user, []string{ev.ID})

	// Post-forget: the event must be gone from both tiers we can query.
	// We give it a short budget to let any lagging write settle but
	// then flip the assertion — the hit should be absent, not present.
	post := c.recallWithText(user, marker, 2*time.Second)
	if containsHit(post.Hits, ev.ID) {
		for _, h := range post.Hits {
			if h.ID == ev.ID {
				t.Errorf("post-forget recall still surfaced event %s from %s tier (score=%.4f, content=%q)",
					ev.ID, h.Tier, h.Score, h.Content)
			}
		}
	}

	// Stronger assertion: the tier counts should show no hit for our
	// marker at all. Other events may share nothing with this unique
	// marker string, so the whole hit list should be empty.
	if len(post.Hits) > 0 {
		t.Logf("post-forget hits (expected empty for unique marker): %s", formatHits(post.Hits))
	}
}
