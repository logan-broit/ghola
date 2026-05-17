//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestSessionResume_DaemonRestart asserts the load-bearing continuity
// property: an event consolidated to episodic before the ghola daemon
// restarts is still retrievable by recall after the daemon comes back.
//
// Sietch (working tier) lives in the ghola container's fs with no
// volume mount, so the restart intentionally wipes it. If the recall
// still works, the episodic tier is doing its job.
//
// Requires GHOLA_COMPOSE_DIR to point at the compose project (so we
// can run `docker compose restart ghola`). Skipped otherwise.
func TestSessionResume_DaemonRestart(t *testing.T) {
	c := newClient(t)

	composeDir := os.Getenv("GHOLA_COMPOSE_DIR")
	if composeDir == "" {
		t.Skip("GHOLA_COMPOSE_DIR not set; can't orchestrate docker restart")
	}

	c.waitHealthy(30 * time.Second)

	user := e2eUser()

	// Marker text unique to this run so we don't collide with events
	// seeded by prior tests against the same dev stack.
	marker := "gate2-resume-marker-" + time.Now().UTC().Format("20060102T150405.000")

	sess := c.startSession(user, "claude-code")
	ev := c.recordText(sess.ID, user,
		"Phase 11 Gate 2 probe — "+marker+
			" — this event must survive a ghola daemon restart.")

	// Force the encoding watermark past this event so it lives in
	// episodic (Postgres), not just sietch (will be wiped on restart).
	c.consolidate(sess.ID)

	// Sanity: recall must see it right now.
	c.recallAwait(user, marker, ev.ID, 5*time.Second)

	// Restart the ghola container. Postgres + chapterhouse stay up.
	restartGhola(t, composeDir)

	// New daemon, new sietch. Wait for /health to come back.
	c.waitHealthy(30 * time.Second)

	// Recall must still surface the event — from episodic, since the
	// working tier is freshly empty.
	hits := c.recallAwait(user, marker, ev.ID, 10*time.Second)

	// Assert the hit is explicitly from the episodic tier. If it
	// came from working the test is lying to us (new sietch should
	// not have our pre-restart event).
	if !seenInTier(hits.Hits, ev.ID, "episodic") {
		t.Fatalf("post-restart recall did not surface event from episodic tier; hits=%s",
			formatHits(hits.Hits))
	}

	// Extra: there should be zero working-tier hits for this id —
	// the new sietch is blank so nothing local could have matched.
	for _, h := range hits.Hits {
		if h.ID == ev.ID && h.Tier == "working" {
			t.Fatalf("working tier unexpectedly surfaced event %s — was sietch persisted?", ev.ID)
		}
	}
}

// restartGhola runs `docker compose restart ghola` in the given
// compose project dir and t.Fatals on non-zero exit.
func restartGhola(t *testing.T, composeDir string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "restart", "ghola")
	cmd.Dir = composeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose restart ghola failed: %v\n%s", err, string(out))
	}
	t.Logf("restart complete: %s", string(out))
}
