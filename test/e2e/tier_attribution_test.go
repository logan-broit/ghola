//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTierAttribution_NonExclusive pins down the Core.Recall contract:
// tier attribution is NOT deduplicated across tiers. A single event
// id may surface as a "working" hit (sietch vector/FTS) AND an
// "episodic" hit (chapterhouse postgres) once it's been consolidated
// — each tier reports its own view, and callers can dedupe by id at
// presentation time if they want.
//
// Covers three transitions:
//
//  1. Pre-consolidate: the event only lives in sietch.
//     → recall surfaces it ONLY in working tier.
//
//  2. Post-consolidate: sietch still has the row (soft watermark
//     advance; nothing deletes from sietch), and chapterhouse has
//     the event too.
//     → recall surfaces it in BOTH working and episodic.
//
//  3. With a semantic mneme seeded directly into the workspace,
//     recall (include_semant + workspace) labels it tier="semantic".
//     The mneme is inserted via docker exec psql — replay would
//     normally do this but Mentat is blocked (Gate 3).
func TestTierAttribution_NonExclusive(t *testing.T) {
	c := newClient(t)
	c.waitHealthy(30 * time.Second)

	composeDir := os.Getenv("GHOLA_COMPOSE_DIR")
	if composeDir == "" {
		t.Skip("GHOLA_COMPOSE_DIR required to seed the semantic tier")
	}

	user := e2eUser()
	marker := "gate6-tier-marker-" + time.Now().UTC().Format("20060102T150405.000")

	sess := c.startSession(user, "claude-code")
	ev := c.recordText(sess.ID, user,
		"Phase 11 Gate 6 probe — "+marker+
			" — tier attribution is non-exclusive by design.")

	// ------------------------------------------------------------------
	// (1) Pre-consolidate: working tier only.
	// ------------------------------------------------------------------
	pre := c.recallSessionScoped(user, sess.ID, marker, ev.ID, 3*time.Second)
	if !seenInTier(pre.Hits, ev.ID, "working") {
		t.Fatalf("pre-consolidate: event %s not in working tier; hits=%s",
			ev.ID, formatHits(pre.Hits))
	}
	if seenInTier(pre.Hits, ev.ID, "episodic") {
		t.Errorf("pre-consolidate: event %s should NOT be in episodic yet; hits=%s",
			ev.ID, formatHits(pre.Hits))
	}

	// ------------------------------------------------------------------
	// (2) Post-consolidate: both working AND episodic.
	// ------------------------------------------------------------------
	c.consolidate(sess.ID)

	post := c.recallSessionScoped(user, sess.ID, marker, ev.ID, 5*time.Second)
	if !seenInTier(post.Hits, ev.ID, "working") {
		t.Errorf("post-consolidate: working tier lost the event (sietch shouldn't prune on flush); hits=%s",
			formatHits(post.Hits))
	}
	if !seenInTier(post.Hits, ev.ID, "episodic") {
		t.Errorf("post-consolidate: event %s missing from episodic tier; hits=%s",
			ev.ID, formatHits(post.Hits))
	}
	if post.TierCounts["working"] < 1 || post.TierCounts["episodic"] < 1 {
		t.Errorf("tier_counts should reflect per-tier hit presence, got %v", post.TierCounts)
	}

	// ------------------------------------------------------------------
	// (3) Semantic tier: seed a mneme directly in postgres, query with
	//     include_semant + workspace, expect tier="semantic".
	// ------------------------------------------------------------------
	workspace := uuid.New().String()
	semMarker := "gate6-semantic-marker-" + time.Now().UTC().Format("20060102T150405.000")
	mnemeID := seedSemanticMneme(t, composeDir, workspace,
		"Gate 6 semantic probe",
		"This mneme exists to validate semantic-tier attribution. "+semMarker)

	semOut := c.recallSemantic(user, workspace, semMarker, 3*time.Second)
	if !seenInTier(semOut.Hits, mnemeID, "semantic") {
		t.Fatalf("semantic recall didn't label mneme %s as tier=semantic; hits=%s",
			mnemeID, formatHits(semOut.Hits))
	}
	if semOut.TierCounts["semantic"] < 1 {
		t.Errorf("tier_counts.semantic should be >= 1, got %v", semOut.TierCounts)
	}
}

// seedSemanticMneme inserts a row directly into semantic.mnemes via
// the postgres container. Uses a zero vector so any real query
// vector will have cosine=0 to it — the FTS path on concept+content
// is what surfaces it, which is fine: we only care that the tier
// label is correct.
func seedSemanticMneme(t *testing.T, composeDir, workspace, concept, content string) string {
	t.Helper()
	id := uuid.New().String()
	// 1024-dim zero vector literal.
	parts := make([]string, 1024)
	for i := range parts {
		parts[i] = "0"
	}
	zeroVec := "[" + strings.Join(parts, ",") + "]"

	sql := fmt.Sprintf(
		`INSERT INTO semantic.mnemes (id, workspace_id, concept, content, embedding, confidence)
		 VALUES ('%s', '%s', %s, %s, '%s'::vector, 0.8);`,
		id, workspace,
		pqQuote(concept), pqQuote(content), zeroVec,
	)

	cmd := exec.Command("docker", "compose", "exec", "-T", "postgres",
		"psql", "-U", "memory_api", "-d", "memories", "-c", sql)
	cmd.Dir = composeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("seed semantic mneme failed: %v\n%s", err, string(out))
	}
	return id
}

// pqQuote does the minimal single-quote escaping for our controlled
// test inputs. Not for untrusted text.
func pqQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
