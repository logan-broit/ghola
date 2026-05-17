//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"
)

// defaultUserID is the user chapterhouse expects in default-auth mode.
// Docker-compose sets AUTH_DEFAULT_USER to this value. Tests that
// call /v1/record must use it, else chapterhouse rejects the ingest
// with "session.user_id must match caller".
const defaultUserID = "00000000-0000-0000-0000-000000000001"

func e2eUser() string {
	if v := os.Getenv("GHOLA_E2E_USER"); v != "" {
		return v
	}
	return defaultUserID
}

// TestGoldenPath drives the headline acceptance scenario:
//   1. Two independent "agents" each start a session with the same
//      user and record a distinctive turn.
//   2. Both sessions are force-consolidated so pending events flush
//      to chapterhouse (bypasses the 5-minute encoding worker tick).
//   3. Recall by text unique to each turn surfaces that event from
//      the episodic tier, demonstrating the encoding pipeline is
//      lossless end-to-end.
//
// This is criterion 1 of the Phase 11 acceptance gates and the
// smallest test that exercises every service in the compose stack
// (postgres, guild-stub, chapterhouse, ghola). Everything
// downstream (pi-mono, MCP bridge, replay) is layered on top of this
// working path.
func TestGoldenPath(t *testing.T) {
	c := newClient(t)
	c.waitHealthy(30 * time.Second)

	userID := e2eUser()

	// Agent A — Claude Code simulating a planning session.
	agentA := c.startSession(userID, "claude-code")
	evA := c.recordText(agentA.ID, userID,
		"We migrated the ghola episodic schema to pg17 with pgvector")

	// Agent B — a separate pi-mono shell doing unrelated work.
	agentB := c.startSession(userID, "pi-mono")
	evB := c.recordText(agentB.ID, userID,
		"Switched the cron scheduler to run replay at 02:00 local")

	if agentA.ID == agentB.ID {
		t.Fatalf("session ids collided: %s", agentA.ID)
	}

	// Force both sietch files past the encoding watermark. We don't
	// want to wait 5 minutes for the tick.
	c.consolidate(agentA.ID)
	c.consolidate(agentB.ID)

	// Query terms distinctive to each turn; each should surface the
	// matching event from the episodic tier (working tier alone
	// would also match, but the point of this test is the flush).
	a := c.recallAwait(userID, "pgvector migration", evA.ID, 5*time.Second)
	b := c.recallAwait(userID, "replay cron scheduler 02:00", evB.ID, 5*time.Second)

	if !seenInTier(a.Hits, evA.ID, "episodic") {
		t.Errorf("evA not in episodic tier; hits=%s", formatHits(a.Hits))
	}
	if !seenInTier(b.Hits, evB.ID, "episodic") {
		t.Errorf("evB not in episodic tier; hits=%s", formatHits(b.Hits))
	}

	// (A broad "give me everything for this user" recall check lived
	// here originally, but it was flaky against a dev DB that
	// accumulates events across runs — a top-10 cross-session query
	// can get crowded out by prior test noise. The per-event specific
	// recalls above already prove the gate's claim: two independent
	// sessions, same service, each agent sees its own events
	// consolidated to episodic.)
}

// seenInTier returns true if hits contains (id, tier).
func seenInTier(hits []recallHit, id, tier string) bool {
	for _, h := range hits {
		if h.ID == id && h.Tier == tier {
			return true
		}
	}
	return false
}
