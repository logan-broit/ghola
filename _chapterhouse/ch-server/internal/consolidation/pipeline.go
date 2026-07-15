package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// ErrConsolidationBusy signals that another consolidation run already holds
// the workspace's advisory lock. RunWorkspace returns it instead of racing
// a concurrent run (which could double-insert clusters or double-reinforce).
// The manual-trigger handler maps it to 409 Conflict; the nightly job
// logs-and-skips that workspace and retries next tick.
var ErrConsolidationBusy = errors.New("consolidation already running for this workspace")

// Embedder is the seam for embedding the digest text (satisfied by
// *embedding.OpenAIProvider). Nil disables the digest write.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Deps bundles the pipeline's collaborators so both the worker tick and
// the API endpoint construct them once and pass them in.
type Deps struct {
	Repo           *repository.Repository
	Mentat         *mentat.Client
	Pooler         SessionPooler // *semantic.Writer
	LLM            *LLMClient    // nil => skip label/digest
	Embedder       Embedder      // nil => skip digest embedding
	Logger         *slog.Logger
	MinClusterSize int // 0 => 3
	RepK           int // 0 => 4
	TagTopN        int // 0 => 10
}

// RunWorkspace runs the full nightly consolidation for one workspace:
// reconcile -> cluster -> match/apply -> enrich -> label/digest.
//
// Loud-fails on mentat errors (aborts; the nightly retry recovers).
// LLM/embedder failures are swallowed with a log line — labels/digest
// are best-effort metadata, never a gate on the free enrichment.
//
// Re-run convergence (pool-based, no batch-spanning transaction — see
// review note): each stage converges on re-run, so a mid-run failure leaves
// a partial but VALID state the next nightly run reconciles. Reconcile
// overwrites L1s; ApplyClusters is idempotent ACROSS runs (unchanged-
// membership reinforcements are skipped) AND consistent WITHIN a run — it
// threads a working in-memory view of the level-1 set through the
// assignments so a cluster split (one existing mneme overlapping several new
// assignments) reinforces exactly one row and inserts the rest rather than
// last-write-wins collapsing them into one; UpdateMneme-
// Enrichment overwrites content columns; ArchivePriorDigest + Insert-
// DigestMneme atomically-enough replace the single active digest (a crash
// between them leaves zero active digests, which the next run repairs).
// The design deliberately declines a workspace-wide transaction: the
// stages touch two schemas and an external service, and the idempotency
// above makes the transaction's only benefit — crash atomicity — moot.
//
// Concurrency: this is the single seam both the nightly job and the manual
// trigger flow through, so it is where the per-workspace advisory lock is
// acquired. A second concurrent run on the same workspace short-circuits
// with ErrConsolidationBusy rather than racing the first (which could
// double-insert a new cluster or double-reinforce an existing mneme). The
// lock is held on a dedicated connection for the run's full duration and
// released on return.
func RunWorkspace(ctx context.Context, d Deps, workspaceID uuid.UUID) error {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}

	// Serialize per workspace: decline rather than race a concurrent run.
	release, acquired, err := d.Repo.TryWorkspaceConsolidationLock(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("acquire consolidation lock: %w", err)
	}
	if !acquired {
		return ErrConsolidationBusy
	}
	defer release()

	mcs := d.MinClusterSize
	if mcs == 0 {
		mcs = 3
	}
	repK := d.RepK
	if repK == 0 {
		repK = 4
	}
	tagTopN := d.TagTopN
	if tagTopN == 0 {
		tagTopN = 10
	}

	t0 := time.Now()
	// 1. Reconcile: pool any closed session still missing an L1 vector so
	// the cluster step sees a complete session set. Overlaps benignly with
	// the api-side semantic.Reconciler ticker (both consume
	// ClosedSessionsMissingL1); PoolSessionToL1 is idempotent and only
	// touches l1_embedding IS NULL rows, so whichever runs first wins and
	// the other no-ops. Retirement of the old ticker is Task 21's job.
	nPooled, err := Reconcile(ctx, d.Repo, d.Pooler, 512)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	tReconcile := time.Since(t0)

	// 2. Cluster.
	t1 := time.Now()
	sessions, err := d.Repo.WorkspaceSessionL1s(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("read session l1s: %w", err)
	}
	if len(sessions) == 0 {
		log.Info("consolidation: no L1 sessions; nothing to do", "workspace_id", workspaceID)
		return nil
	}
	ids := make([]string, len(sessions))
	embs := make([][]float32, len(sessions))
	for i, s := range sessions {
		ids[i] = s.SessionID.String()
		embs[i] = s.Embedding
	}
	clusterResp, err := d.Mentat.Cluster(ctx, mentat.ClusterRequest{
		IDs: ids, Embeddings: embs, MinClusterSize: mcs,
	})
	if err != nil {
		return fmt.Errorf("mentat cluster (loud-fail): %w", err) // ABORT
	}
	tCluster := time.Since(t1)

	// Group members + centroid per non-noise label. The centroid rides on
	// each assignment (it becomes the mneme embedding), so enrichment can
	// reuse it without re-reading the mneme vector.
	assigns := groupClusters(clusterResp, sessions)

	// 3. Match + apply.
	t2 := time.Now()
	nWritten, err := ApplyClusters(ctx, d.Repo, workspaceID, assigns)
	if err != nil {
		return fmt.Errorf("apply clusters: %w", err)
	}
	tApply := time.Since(t2)

	// 4. Enrich each active level-1 mneme in this workspace (free path).
	t3 := time.Now()
	labels, enrichFailures := enrichAll(ctx, d, workspaceID, assigns, repK, tagTopN, log)
	tEnrich := time.Since(t3)

	// 5. Label + digest (best-effort; requires an LLM).
	t4 := time.Now()
	if d.LLM != nil {
		writeDigest(ctx, d, workspaceID, labels, log)
	} else {
		log.Info("consolidation: LLM unset; skipping labels + digest",
			"workspace_id", workspaceID)
	}
	tDigest := time.Since(t4)

	log.Info("consolidation complete",
		"workspace_id", workspaceID,
		"pooled", nPooled, "sessions", len(sessions),
		"clusters", len(assigns), "written", nWritten,
		"enrich_failures", enrichFailures,
		"ms_reconcile", tReconcile.Milliseconds(),
		"ms_cluster", tCluster.Milliseconds(),
		"ms_apply", tApply.Milliseconds(),
		"ms_enrich", tEnrich.Milliseconds(),
		"ms_digest", tDigest.Milliseconds(),
	)
	return nil
}

// clusterMeta is the per-mneme rollup written to the meta jsonb column.
//
// Shape note: episodic.sessions DOES carry cwd + git_branch (see
// 001_episodic.sql), but surfacing them would require a session-metadata
// read beyond this task's two enumerated repo methods (ArchivePriorDigest
// + InsertDigestMneme). Per the plan's YAGNI guidance we emit the
// counts-only shape and leave cwd/branches to a follow-up if a consumer
// ever needs them.
type clusterMeta struct {
	SessionCount int `json:"session_count"`
	EventCount   int `json:"event_count"`
}

// labeledCluster carries the ordering material writeDigest needs: the
// LLM label plus the signals it ranks by (recency via span_end,
// confidence). Only clusters that actually produced a label appear here.
type labeledCluster struct {
	mnemeID    uuid.UUID
	label      string
	confidence float64
	spanEnd    time.Time
}

// groupClusters folds mentat's per-id labels into one assignment per
// non-noise cluster: sorted member session ids + the L2-normalized mean
// of member L1 vectors (matching mentat's old centroid). Deterministic:
// labels are visited in ascending order and members are UUID-sorted.
func groupClusters(resp *mentat.ClusterResponse, sessions []repository.SessionL1) []ClusterAssignment {
	if resp == nil {
		return nil
	}
	byLabel := make(map[int][]repository.SessionL1)
	n := len(resp.Labels)
	if n > len(sessions) {
		n = len(sessions)
	}
	for i := 0; i < n; i++ {
		lbl := resp.Labels[i]
		if lbl < 0 {
			continue // noise
		}
		byLabel[lbl] = append(byLabel[lbl], sessions[i])
	}
	labels := make([]int, 0, len(byLabel))
	for l := range byLabel {
		labels = append(labels, l)
	}
	sort.Ints(labels)

	out := make([]ClusterAssignment, 0, len(labels))
	for _, l := range labels {
		members := byLabel[l]
		ids := make([]uuid.UUID, len(members))
		embs := make([][]float32, len(members))
		for i, m := range members {
			ids[i] = m.SessionID
			embs[i] = m.Embedding
		}
		sort.Slice(ids, func(i, j int) bool { return lessUUID(ids[i], ids[j]) })
		out = append(out, ClusterAssignment{
			MemberIDs: ids,
			Centroid:  l2Normalize(meanVec(embs)),
		})
	}
	return out
}

// meanVec returns the element-wise mean of same-length vectors. Empty
// input yields nil.
func meanVec(vs [][]float32) []float32 {
	if len(vs) == 0 {
		return nil
	}
	dim := len(vs[0])
	acc := make([]float64, dim)
	count := 0
	for _, v := range vs {
		if len(v) != dim {
			continue // defensive: skip mis-sized vectors
		}
		for i := 0; i < dim; i++ {
			acc[i] += float64(v[i])
		}
		count++
	}
	if count == 0 {
		return nil
	}
	out := make([]float32, dim)
	for i := range acc {
		out[i] = float32(acc[i] / float64(count))
	}
	return out
}

// l2Normalize scales v to unit length. A zero vector is returned
// unchanged (no NaNs from division by zero).
func l2Normalize(v []float32) []float32 {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return v
	}
	norm = math.Sqrt(norm)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

// enrichAll writes selection-first content onto each cluster's mneme:
// per-member most-central event -> MMR representatives -> bounded excerpts
// + aggregated tags/entities/span + counts meta. When an LLM is
// configured it also asks for a one-line label (errors swallowed). Returns
// the ordering material for the digest step plus a count of per-mneme
// enrichment-WRITE failures (surfaced on the completion log line for
// visibility). All failures here are logged, never fatal — enrichment is
// the "free" tier and must not gate the run.
func enrichAll(ctx context.Context, d Deps, workspaceID uuid.UUID, assigns []ClusterAssignment, repK, tagTopN int, log *slog.Logger) ([]labeledCluster, int) {
	active, err := d.Repo.WorkspaceLevel1Mnemes(ctx, workspaceID)
	if err != nil {
		log.Warn("consolidation.enrich: read active mnemes failed", "error", err.Error())
		return nil, 0
	}
	confByID := make(map[uuid.UUID]float64, len(active))
	for _, m := range active {
		confByID[m.ID] = m.Confidence
	}

	var labeled []labeledCluster
	writeFailures := 0
	for _, a := range assigns {
		mnemeID := BestOverlapMatch(a.MemberIDs, active)
		if mnemeID == uuid.Nil {
			continue // no mneme owns this cluster (should not happen post-apply)
		}

		type owned struct {
			ev  repository.EnrichEvent
			sid uuid.UUID
		}
		byEvent := make(map[uuid.UUID]owned)
		var cands []Candidate
		eventCount := 0

		for _, sid := range a.MemberIDs {
			evs, err := d.Repo.SessionEnrichmentEvents(ctx, sid)
			if err != nil {
				log.Warn("consolidation.enrich: session events failed",
					"session_id", sid, "error", err.Error())
				continue
			}
			eventCount += len(evs)
			if len(evs) == 0 {
				continue
			}
			best := mostCentral(evs, a.Centroid)
			byEvent[best.ID] = owned{ev: best, sid: sid}
			cands = append(cands, Candidate{ID: best.ID, Embedding: best.Embedding})
		}
		if len(cands) == 0 {
			continue // nothing to represent
		}

		picks := SelectRepresentatives(cands, a.Centroid, repK)
		reps := make([]Rep, 0, len(picks))
		for _, c := range picks {
			o := byEvent[c.ID]
			reps = append(reps, Rep{
				EventID:   o.ev.ID,
				SessionID: o.sid,
				Excerpt:   Excerpt(o.ev.Text),
				Score:     cosine(o.ev.Embedding, a.Centroid),
				CreatedAt: o.ev.CreatedAt,
				Tags:      o.ev.Tags,
				Entities:  o.ev.Entities,
			})
		}
		agg := Aggregate(reps, tagTopN)

		repsJSON, err := json.Marshal(reps)
		if err != nil {
			log.Warn("consolidation.enrich: marshal reps failed", "mneme_id", mnemeID, "error", err.Error())
			continue
		}
		metaJSON, err := json.Marshal(clusterMeta{
			SessionCount: len(a.MemberIDs),
			EventCount:   eventCount,
		})
		if err != nil {
			log.Warn("consolidation.enrich: marshal meta failed", "mneme_id", mnemeID, "error", err.Error())
			continue
		}

		// LLM label (best-effort): compute BEFORE the enrichment write so a
		// single UpdateMnemeEnrichment persists label + content atomically.
		var labelPtr *string
		var labelStr string
		if d.LLM != nil {
			excerpts := make([]string, len(reps))
			for i, r := range reps {
				excerpts[i] = r.Excerpt
			}
			if lbl, err := d.LLM.Label(ctx, excerpts); err != nil {
				log.Warn("consolidation.enrich: LLM label failed", "mneme_id", mnemeID, "error", err.Error())
			} else if lbl != "" {
				labelStr = lbl
				labelPtr = &labelStr
			}
		}

		if err := d.Repo.UpdateMnemeEnrichment(ctx, mnemeID, labelPtr,
			repsJSON, agg.Tags, agg.Entities, agg.SpanStart, agg.SpanEnd, metaJSON); err != nil {
			log.Warn("consolidation.enrich: update enrichment failed", "mneme_id", mnemeID, "error", err.Error())
			writeFailures++
			continue
		}

		if labelPtr != nil {
			labeled = append(labeled, labeledCluster{
				mnemeID:    mnemeID,
				label:      labelStr,
				confidence: confByID[mnemeID],
				spanEnd:    agg.SpanEnd,
			})
		}
	}
	return labeled, writeFailures
}

// mostCentral returns the event whose embedding is closest (cosine) to the
// centroid. Ties break by smallest event UUID for determinism.
func mostCentral(evs []repository.EnrichEvent, centroid []float32) repository.EnrichEvent {
	best := evs[0]
	bestSim := cosine(best.Embedding, centroid)
	for _, e := range evs[1:] {
		sim := cosine(e.Embedding, centroid)
		if sim > bestSim || (sim == bestSim && lessUUID(e.ID, best.ID)) {
			best = e
			bestSim = sim
		}
	}
	return best
}

// writeDigest asks the LLM for a project-state paragraph from the labeled
// clusters (ordered most-recent, most-confident first), embeds it, archives
// the prior level-2 digest, and inserts the fresh one. Best-effort: any
// failure logs and returns without touching the prior digest's state
// beyond the archive step. No-op when there are no labels or no embedder.
func writeDigest(ctx context.Context, d Deps, workspaceID uuid.UUID, labels []labeledCluster, log *slog.Logger) {
	if len(labels) == 0 {
		log.Info("consolidation: no cluster labels; skipping digest", "workspace_id", workspaceID)
		return
	}
	// Recency first (span_end desc), then confidence desc, then UUID for a
	// total, deterministic order.
	sort.Slice(labels, func(i, j int) bool {
		a, b := labels[i], labels[j]
		if !a.spanEnd.Equal(b.spanEnd) {
			return a.spanEnd.After(b.spanEnd)
		}
		if a.confidence != b.confidence {
			return a.confidence > b.confidence
		}
		return lessUUID(a.mnemeID, b.mnemeID)
	})
	lines := make([]string, len(labels))
	for i, l := range labels {
		lines[i] = l.label
	}

	para, err := d.LLM.Digest(ctx, lines)
	if err != nil {
		log.Warn("consolidation.digest: LLM digest failed", "workspace_id", workspaceID, "error", err.Error())
		return
	}
	if d.Embedder == nil {
		log.Info("consolidation.digest: embedder unset; skipping digest write", "workspace_id", workspaceID)
		return
	}
	emb, err := d.Embedder.Embed(ctx, para)
	if err != nil {
		log.Warn("consolidation.digest: embed digest failed", "workspace_id", workspaceID, "error", err.Error())
		return
	}
	if err := d.Repo.ArchivePriorDigest(ctx, workspaceID); err != nil {
		log.Warn("consolidation.digest: archive prior failed", "workspace_id", workspaceID, "error", err.Error())
		return
	}
	if _, err := d.Repo.InsertDigestMneme(ctx, workspaceID, emb, para); err != nil {
		log.Warn("consolidation.digest: insert digest failed", "workspace_id", workspaceID, "error", err.Error())
	}
}
