package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"
	"github.com/thinkwright/chapterhouse/ch-server/internal/primitives"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"
)

// coActivationEnqueuer is the slim interface the ingest handler depends
// on for buffering co-activation pairs after a successful event upsert.
// *repository.Repository satisfies it via EnqueueCoActivations; tests
// substitute a fake-implementing-the-interface to exercise the best-
// effort error-handling branch without going through the real DB.
//
// Kept as a one-method interface so the dependency surface stays narrow
// and the seam exists exclusively for testability — production wiring
// always points at the real repo (defaulted in NewEpisodicHandler).
type coActivationEnqueuer interface {
	EnqueueCoActivations(ctx context.Context, pairs []repository.CoActivationPair) error
}

// EpisodicHandler services /v1/episodic/* — the per-user raw event
// store called only by the ghola local service (Pipeline A writes;
// recall reads). Not agent-facing; auth is per-user API key and is
// expected to be set on the request context by middleware (Phase
// 3.8); tests inject directly via auth.WithContext.
//
// embedder is an optional server-side backstop. When non-nil, the
// /v1/episodic/ingest handler fills in any event with a nil embedding
// from the configured embedder before persistence — defense-in-depth
// against future ingesters that forget to embed at the wire boundary.
// When nil, NULL embeddings pass through (preserves test ergonomics
// and lighter-client paths). Wire it in via .WithEmbedder().
//
// assocRepo is the seam the ingest handler uses to enqueue co-activation
// pairs after a successful event upsert (Task C1). Defaulted to the
// same *Repository as `repo` in NewEpisodicHandler; tests can swap in a
// fake via WithCoActivationEnqueuer to exercise the failure branch.
//
// assocLookup is the seam the settle path (computeExpansion) uses to
// build the BFS neighborhood graph. Defaulted to repo in
// NewEpisodicHandler; tests can swap in a fakeAssocLookup so the settle
// path runs without a real DB.
type EpisodicHandler struct {
	repo        *repository.Repository
	embedder    embedding.Provider
	assocRepo   coActivationEnqueuer
	assocLookup AssocLookup
}

func NewEpisodicHandler(repo *repository.Repository) *EpisodicHandler {
	return &EpisodicHandler{repo: repo, assocRepo: repo, assocLookup: repo}
}

// WithEmbedder attaches an embedding provider for the server-side
// backstop on /v1/episodic/ingest. Returns the handler for fluent
// chaining at construction. Pass nil to disable (default).
func (h *EpisodicHandler) WithEmbedder(p embedding.Provider) *EpisodicHandler {
	h.embedder = p
	return h
}

// WithCoActivationEnqueuer overrides the default repo-backed enqueuer
// used after event upsert to buffer co-activation pairs. Returns the
// handler for fluent chaining. Production code never calls this — the
// default *Repository wiring in NewEpisodicHandler already satisfies
// the interface; tests use it to inject an erroring fake and pin the
// best-effort failure branch in Ingest.
func (h *EpisodicHandler) WithCoActivationEnqueuer(e coActivationEnqueuer) *EpisodicHandler {
	h.assocRepo = e
	return h
}

// WithAssocLookup overrides the default repo-backed AssocLookup used by
// the settle expansion path in Query. Returns the handler for fluent
// chaining. Production code never calls this — the default *Repository
// wiring in NewEpisodicHandler satisfies the interface; tests inject a
// fakeAssocLookup to exercise the settle path without a real DB.
func (h *EpisodicHandler) WithAssocLookup(a AssocLookup) *EpisodicHandler {
	h.assocLookup = a
	return h
}

// ---------------------------------------------------------------------
// DTOs (mirror docs/api/v1-chapterhouse.yaml)
// ---------------------------------------------------------------------

type ingestRequest struct {
	Session repository.EpisodicSession `json:"session"`
	Events  []repository.EpisodicEvent `json:"events"`
}

type ingestResponse struct {
	SessionID uuid.UUID `json:"session_id"`
	Inserted  int       `json:"inserted"`
	Updated   int       `json:"updated"`
}

// ---------------------------------------------------------------------
// Ingest
// ---------------------------------------------------------------------

// Ingest upserts one session and its events. Idempotent — same event
// id POSTed twice is counted as Updated, not a duplicate. Pipeline A's
// at-least-once delivery depends on this.
//
// Server-side embedding backstop: if h.embedder is configured AND any
// incoming event has a nil embedding AND a non-nil text body, the
// entire batch of missing-embedding events is embedded server-side
// (one EmbedBatch call) before persistence. Events without text bodies
// (tool calls, system events) are skipped — they persist with NULL
// embedding, mirroring what import-logs does at the wire boundary. If
// the embedder fails, the entire ingest fails (no partial commits with
// NULL embeddings). If h.embedder is nil, NULL embeddings pass through
// (legacy/test path).
func (h *EpisodicHandler) Ingest(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}

	if err := validateIngest(&req, userID); err != nil {
		apierror.BadRequest(err.Error()).WriteJSON(w)
		return
	}

	// Default times.
	if req.Session.StartedAt.IsZero() {
		req.Session.StartedAt = time.Now().UTC()
	}
	for i := range req.Events {
		if req.Events[i].CreatedAt.IsZero() {
			req.Events[i].CreatedAt = time.Now().UTC()
		}
	}

	ctx := r.Context()

	// Server-side embedding backstop. Filter events that need embedding
	// (nil Embedding AND non-nil Text), batch them in a single
	// EmbedBatch round-trip, splice the results back in. The embedder
	// returns []float32; the wire/DB type is []float64 — convert.
	if h.embedder != nil {
		var nilIdx []int
		var nilTexts []string
		for i := range req.Events {
			ev := &req.Events[i]
			if ev.Embedding == nil && ev.Text != nil && *ev.Text != "" {
				nilIdx = append(nilIdx, i)
				nilTexts = append(nilTexts, *ev.Text)
			}
		}
		if len(nilTexts) > 0 {
			vecs, err := h.embedder.EmbedBatch(ctx, nilTexts)
			if err != nil {
				apierror.InternalError("embed missing event embeddings").
					WithError(err).WriteJSON(w)
				return
			}
			if len(vecs) != len(nilTexts) {
				apierror.InternalError(fmt.Sprintf(
					"embedder returned %d vectors for %d inputs",
					len(vecs), len(nilTexts))).WriteJSON(w)
				return
			}
			for j, idx := range nilIdx {
				req.Events[idx].Embedding = float32sToFloat64s(vecs[j])
			}
		}
	}

	inserted, updated, err := h.repo.IngestEpisodicBatch(ctx, &req.Session, req.Events)
	if err != nil {
		apierror.InternalError("ingest failed").WithError(err).WriteJSON(w)
		return
	}

	// Hebbian co-activation buffering (Task C1). Every pair of events
	// in the same ingest batch is treated as having lit up together;
	// the consolidation worker (C2-C4) drains the queue and folds each
	// pair into semantic.associations. Best-effort by design — the
	// events are already durably upserted, and the queue insertion is
	// downstream of any caller's success criterion.
	//
	// All-pairs construction yields n*(n-1)/2 rows for n events:
	// i ranges over [0, n), j over (i, n), preserving event order so
	// (src, dst) reads as "src lit up before dst" in chronological
	// session time. Single- and zero-event batches skip the path
	// entirely (no pairs, no DB hit, no log).
	if len(req.Events) > 1 {
		pairs := make([]repository.CoActivationPair, 0, len(req.Events)*(len(req.Events)-1)/2)
		for i, a := range req.Events {
			for _, b := range req.Events[i+1:] {
				pairs = append(pairs, repository.CoActivationPair{
					SrcEventID:  a.ID,
					DstEventID:  b.ID,
					WorkspaceID: req.Session.WorkspaceID,
				})
			}
		}
		if err := h.assocRepo.EnqueueCoActivations(ctx, pairs); err != nil {
			slog.Warn("co-activation enqueue failed",
				"err", err.Error(),
				"workspace_id", req.Session.WorkspaceID,
				"session_id", req.Session.ID,
				"event_count", len(req.Events),
				"pair_count", len(pairs),
			)
		}
	}

	OK(w, ingestResponse{
		SessionID: req.Session.ID,
		Inserted:  inserted,
		Updated:   updated,
	})
}

// float32sToFloat64s widens an embedding vector from the embedder's
// native []float32 to the []float64 used on the wire + in
// repository.EpisodicEvent.Embedding (the pgvector codec writes float8
// arrays; matching the wire type avoids a re-allocation on every row).
func float32sToFloat64s(in []float32) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = float64(v)
	}
	return out
}

func validateIngest(req *ingestRequest, caller uuid.UUID) error {
	if req.Session.ID == uuid.Nil {
		return errors.New("session.id is required")
	}
	if req.Session.UserID == uuid.Nil {
		return errors.New("session.user_id is required")
	}
	if req.Session.UserID != caller {
		return errors.New("session.user_id must match caller")
	}
	// Workspace scoping is non-optional. A session ingested without a
	// workspace_id has no scoping primitive, would silently be excluded
	// from every recall, and would only be discoverable via direct SQL.
	// Fail loud, same posture as the recall side.
	if req.Session.WorkspaceID == uuid.Nil {
		return errors.New("session.workspace_id is required")
	}
	for i, ev := range req.Events {
		if ev.ID == uuid.Nil {
			return fmt.Errorf("events[%d].id is required", i)
		}
		if ev.SessionID != req.Session.ID {
			return fmt.Errorf("events[%d].session_id must match session.id", i)
		}
		if ev.UserID != caller {
			return fmt.Errorf("events[%d].user_id must match caller", i)
		}
		switch ev.Type {
		case "user", "assistant", "tool_result", "system":
		default:
			return fmt.Errorf("events[%d].type must be user|assistant|tool_result|system", i)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// Query / Share / Forget — implemented in Tasks 3.5 + 3.6.
// ---------------------------------------------------------------------

// queryRequest mirrors the OpenAPI EpisodicQueryRequest. Post-A8 the
// /v1/episodic/query endpoint is multi-ranking-only: callers MUST set
// a non-empty `rankings` list naming the subset of
// {"vector","fts","session_vector"} they want ranked separately. The
// legacy hybrid mode (no `rankings` field, returns a single fused
// `hits` array) was removed once LongMemEval (A7) confirmed parity.
//
// TagsAny is a top-level overlap-style tag filter forwarded to every
// event-grain tier (vector, fts); session_vector ignores it. Empty/
// absent → no filter applied.
type queryRequest struct {
	UserID         uuid.UUID `json:"user_id"`
	WorkspaceID    uuid.UUID `json:"workspace_id"`
	QueryText      string    `json:"query_text,omitempty"`
	QueryEmbedding []float64 `json:"query_embedding,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	IncludeShared  *bool     `json:"include_shared,omitempty"`
	TagsAny        []string  `json:"tags_any,omitempty"`
	// Rankings names the per-tier subset to fan out across. A non-empty
	// list is required — an absent or empty list is rejected with 400.
	Rankings []string `json:"rankings,omitempty"`
	// Primitives opts the response into the Hebbian-boosted fourth
	// sub-list. Default off keeps the baseline cheap; ghola passes
	// `true` only when the caller has asked for primitive enrichment.
	Primitives bool `json:"primitives,omitempty"`
	// Settle opts the response into the P4 recurrent-settle expansion
	// sub-list. Absent (nil) means disabled; expansion never appears.
	Settle *SettleRequest `json:"settle,omitempty"`
}

// ScoreBreakdown is the per-tier score envelope carried by every
// multi-ranking hit. Semantic + FTS are the raw per-leg scores; Merged
// is the tier's single sort key (matches Semantic on dense-only tiers,
// FTS on keyword).
//
// Exported so the ghola client can decode multi-ranking hits without
// redefining the shape on its side.
type ScoreBreakdown struct {
	Semantic float64 `json:"semantic"`
	FTS      float64 `json:"fts"`
	Merged   float64 `json:"merged"`
}

// validRankings is the closed set of tier names the multi-ranking path
// accepts. Kept as a package-level map so the validator stays a one-
// liner and D1's `primitives` flag has a clean place to land.
var validRankings = map[string]struct{}{
	"vector":         {},
	"fts":            {},
	"session_vector": {},
}

// Query is the entry point for /v1/episodic/query. Validates the
// request, fans the requested rankings out concurrently, and projects
// per-tier raw repo hits into the MultiRankingResponse sub-lists.
// Sub-lists for tiers the caller did NOT request stay nil → omitempty
// drops them. Sub-lists for requested tiers are always allocated (even
// when empty) so the response carries an explicit `[]` rather than the
// tier silently disappearing — contract is "you asked for it, you get
// an array, possibly empty".
//
// Each tier in `req.Rankings` maps to a single repo call:
//
//   - "vector"         -> QueryEpisodicEventsByVector (pure cosine, event grain)
//   - "fts"            -> QueryEpisodicKeyword       (Postgres FTS, event grain)
//   - "session_vector" -> QueryEpisodicSessionVector (cosine on session pool)
//
// Validation:
//   - missing/invalid auth -> 401
//   - missing user_id / workspace_id, or user_id != caller -> 400/403
//   - empty rankings list -> 400 (caller must name at least one tier)
//   - unknown ranking name -> 400 (typos / future tier names not yet wired)
//   - duplicates -> deduped silently (per-tier identity, not list)
//
// Fan-out uses errgroup so the first failure cancels in-flight peers.
// Per-tier limit defaults to 10; per-tier tags_any mirrors the
// request's TagsAny field.
func (h *EpisodicHandler) Query(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.UserID == uuid.Nil {
		apierror.BadRequest("user_id is required").WriteJSON(w)
		return
	}
	if req.UserID != userID {
		// Agents may only query as themselves. (A future admin
		// endpoint could lift this; not v1a.)
		apierror.Forbidden("user_id must match caller").WriteJSON(w)
		return
	}
	if req.WorkspaceID == uuid.Nil {
		apierror.BadRequest("workspace_id is required").WriteJSON(w)
		return
	}
	if len(req.Rankings) == 0 {
		apierror.BadRequest("rankings must be a non-empty subset of " +
			`{"vector","fts","session_vector"}`).WriteJSON(w)
		return
	}
	requested := make(map[string]struct{}, len(req.Rankings))
	for _, name := range req.Rankings {
		if _, ok := validRankings[name]; !ok {
			apierror.BadRequest(fmt.Sprintf(
				"unknown ranking %q (valid: vector, fts, session_vector)",
				name)).WriteJSON(w)
			return
		}
		requested[name] = struct{}{}
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	// Per-tier slots — goroutines touch independent slots, no mutex
	// needed beyond the errgroup's barrier on Wait().
	var (
		vectorHits  []repository.EpisodicEventHit
		ftsHits     []repository.EpisodicEventHit
		sessionHits []repository.EpisodicSessionVectorHit
	)

	g, gctx := errgroup.WithContext(r.Context())
	if _, ok := requested["vector"]; ok {
		g.Go(func() error {
			hits, err := h.repo.QueryEpisodicEventsByVector(gctx, repository.EpisodicVectorParams{
				UserID:         userID,
				WorkspaceID:    req.WorkspaceID,
				QueryEmbedding: req.QueryEmbedding,
				Limit:          limit,
				TagsAny:        req.TagsAny,
			})
			if err != nil {
				return fmt.Errorf("vector tier: %w", err)
			}
			vectorHits = hits
			return nil
		})
	}
	if _, ok := requested["fts"]; ok {
		g.Go(func() error {
			hits, err := h.repo.QueryEpisodicKeyword(gctx, repository.EpisodicKeywordParams{
				UserID:      userID,
				WorkspaceID: req.WorkspaceID,
				QueryText:   req.QueryText,
				Limit:       limit,
				TagsAny:     req.TagsAny,
			})
			if err != nil {
				return fmt.Errorf("fts tier: %w", err)
			}
			ftsHits = hits
			return nil
		})
	}
	if _, ok := requested["session_vector"]; ok {
		g.Go(func() error {
			hits, err := h.repo.QueryEpisodicSessionVector(gctx, repository.EpisodicSessionVectorParams{
				UserID:         userID,
				WorkspaceID:    req.WorkspaceID,
				QueryEmbedding: req.QueryEmbedding,
				Limit:          limit,
			})
			if err != nil {
				return fmt.Errorf("session_vector tier: %w", err)
			}
			sessionHits = hits
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		Error(w, r, apierror.InternalError("query failed").WithError(err))
		return
	}

	out := MultiRankingResponse{}
	if _, ok := requested["vector"]; ok {
		out.Vector = make([]MultiRankingHit, 0, len(vectorHits))
		for i := range vectorHits {
			eventID := vectorHits[i].Event.ID
			out.Vector = append(out.Vector, MultiRankingHit{
				EventID: &eventID,
				Tier:    "vector",
				Score: ScoreBreakdown{
					Semantic: vectorHits[i].Semantic,
					FTS:      vectorHits[i].FTS,
					Merged:   vectorHits[i].Merged,
				},
				Text:             vectorHits[i].Event.Text,
				SessionChunkText: vectorHits[i].SessionChunkText,
			})
		}
	}
	if _, ok := requested["fts"]; ok {
		out.FTS = make([]MultiRankingHit, 0, len(ftsHits))
		for i := range ftsHits {
			eventID := ftsHits[i].Event.ID
			out.FTS = append(out.FTS, MultiRankingHit{
				EventID: &eventID,
				Tier:    "fts",
				Score: ScoreBreakdown{
					Semantic: ftsHits[i].Semantic,
					FTS:      ftsHits[i].FTS,
					Merged:   ftsHits[i].Merged,
				},
				Text:             ftsHits[i].Event.Text,
				SessionChunkText: ftsHits[i].SessionChunkText,
			})
		}
	}
	if _, ok := requested["session_vector"]; ok {
		out.SessionVector = make([]MultiRankingHit, 0, len(sessionHits))
		for i := range sessionHits {
			sessionID := sessionHits[i].SessionID
			out.SessionVector = append(out.SessionVector, MultiRankingHit{
				SessionID: &sessionID,
				Tier:      "session_vector",
				// session_vector tier is dense-only: Semantic carries
				// the cosine projection, FTS is zero by construction,
				// Merged tracks Semantic so callers see a single sort
				// key.
				Score: ScoreBreakdown{
					Semantic: sessionHits[i].Score,
					FTS:      0,
					Merged:   sessionHits[i].Score,
				},
				SessionChunkText: sessionHits[i].SessionChunkText,
			})
		}
	}

	// Primitives sub-list (D1). Best-effort enrichment: caller asked
	// for primitive boosts on top of the standard tiers. Failure to
	// look up associations does NOT fail the query — the events have
	// already been ranked, primitives are a delta. On lookup error we
	// log and leave the field nil so omitempty drops it from the wire.
	//
	// On success the field is always set (pointer-to-empty-slice when
	// no in-set boosts) so the caller distinguishes "asked, got
	// nothing" from "didn't ask".
	if req.Primitives {
		out.Primitives = computePrimitivesRanking(
			r.Context(), h.repo, req.WorkspaceID, out.Vector, out.FTS, limit,
		)
	}

	// Settle expansion sub-list (P4). Best-effort: errors log and leave the
	// field absent. Expansion is a separate sub-list — it is never merged
	// into any tier list and is absent entirely when settle is disabled.
	if req.Settle != nil && req.Settle.Enabled {
		out.Expansion = computeExpansion(
			r.Context(), h.repo, h.assocLookup,
			userID, req.WorkspaceID,
			out.Vector, out.FTS,
			req.Settle.params(),
		)
	}

	OK(w, out)
}

// computePrimitivesRanking is the D1 read-side enrichment. Given the
// per-tier event-grain hits already computed for the response, build
// the candidate set (union of vector + fts event ids), bulk-look up
// their hebbian associations, fold the per-candidate boosts, and emit
// a sorted MultiRankingHit slice (descending by boost). Zero-boost
// candidates drop out — they have no in-set neighbors and therefore
// no primitives signal to surface.
//
// Best-effort: a lookup error is logged and the function returns nil
// so the caller's `,omitempty` drops the field. Success — including
// the empty-set case — returns a non-nil pointer so the wire shape
// distinguishes "lookup failed / not requested" from "requested,
// nothing surfaced".
func computePrimitivesRanking(
	ctx context.Context,
	repo *repository.Repository,
	workspaceID uuid.UUID,
	vectorHits, ftsHits []MultiRankingHit,
	limit int,
) *[]MultiRankingHit {
	candidates := uniqueEventIDs(vectorHits, ftsHits)

	associations, err := repo.LookupAssociations(ctx, candidates, "hebbian", workspaceID)
	if err != nil {
		slog.Warn("primitives: association lookup failed (best-effort, dropping field)",
			"err", err.Error(),
			"workspace_id", workspaceID,
			"candidate_count", len(candidates),
		)
		return nil
	}

	boosts := primitives.BoostsFor(candidates, associations)
	hits := sortedHits(candidates, boosts, vectorHits, ftsHits, limit)
	return &hits
}

// uniqueEventIDs collects the deduped event ids from the event-grain
// tiers (vector + fts). session_vector hits don't contribute — they
// are session-grain and BoostsFor's input shape is event-keyed.
// Order-preserving so the deterministic ordering of the upstream
// hits is reflected in the returned slice (helps when boosts are
// identical and the test wants stable assertions).
func uniqueEventIDs(vector, fts []MultiRankingHit) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(vector)+len(fts))
	out := make([]uuid.UUID, 0, len(vector)+len(fts))
	add := func(hits []MultiRankingHit) {
		for _, h := range hits {
			if h.EventID == nil {
				continue
			}
			id := *h.EventID
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	add(vector)
	add(fts)
	return out
}

// sortedHits projects (candidate, boost) pairs into the wire-level
// MultiRankingHit shape. Drops zero-boost entries (no in-set
// neighbors == no primitives signal), sorts descending by boost, and
// caps at `limit` (matches the per-tier limit the rest of the
// handler uses). Texts are lifted from the upstream vector / fts hits
// so we don't re-query the events table — the candidate set is
// strictly a subset of the union of those tiers.
func sortedHits(
	candidates []uuid.UUID,
	boosts map[uuid.UUID]float64,
	vectorHits, ftsHits []MultiRankingHit,
	limit int,
) []MultiRankingHit {
	textByID := make(map[uuid.UUID]string, len(candidates))
	collect := func(hits []MultiRankingHit) {
		for _, h := range hits {
			if h.EventID == nil || h.Text == nil {
				continue
			}
			if _, ok := textByID[*h.EventID]; ok {
				continue
			}
			textByID[*h.EventID] = *h.Text
		}
	}
	collect(vectorHits)
	collect(ftsHits)

	type scored struct {
		id    uuid.UUID
		boost float64
	}
	scoredCands := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		b := boosts[c]
		if b <= 0 {
			continue
		}
		scoredCands = append(scoredCands, scored{c, b})
	}
	sort.SliceStable(scoredCands, func(i, j int) bool {
		return scoredCands[i].boost > scoredCands[j].boost
	})
	if limit > 0 && len(scoredCands) > limit {
		scoredCands = scoredCands[:limit]
	}

	out := make([]MultiRankingHit, 0, len(scoredCands))
	for _, sc := range scoredCands {
		id := sc.id
		var textPtr *string
		if t, ok := textByID[id]; ok {
			text := t
			textPtr = &text
		}
		out = append(out, MultiRankingHit{
			EventID: &id,
			Tier:    "primitives",
			Score:   ScoreBreakdown{Merged: sc.boost},
			Text:    textPtr,
		})
	}
	return out
}

// computeExpansion runs the P4 recurrent-settle pipeline:
//
//  1. Build a seed mass from the vector + fts hit event IDs.
//     Normalization: per-tier max-normalize each tier's Merged scores so
//     both tiers contribute on the same scale, then for events that appear
//     in BOTH tiers average the two normalized scores, then L1-normalize
//     the seed vector so it sums to 1 (the personalized PageRank contract).
//     Events with no score (missing from both tiers) get mass 0.
//     Rationale: max-normalization is safe here because we only need
//     relative ordering within the seed set; absolute score values vary
//     across queries and between tiers, so normalizing per tier prevents
//     one tier from dominating the seed mass solely due to score magnitude.
//
//  2. BuildSettleGraph over the Hebbian neighborhood (BFS, HopCap/NodeCap).
//
//  3. Settle to convergence.
//
//  4. TopExpansion: top-M non-seed nodes by activation.
//
//  5. Hydrate text for expansion IDs from PG.
//
// Best-effort: any error is logged and nil is returned; the caller's
// `,omitempty` drops the field. Success — including empty — returns a
// non-nil pointer so the wire distinguishes "requested, nothing found"
// from "not requested".
func computeExpansion(
	ctx context.Context,
	repo *repository.Repository,
	assocLookup AssocLookup,
	userID, workspaceID uuid.UUID,
	vectorHits, ftsHits []MultiRankingHit,
	p primitives.SettleParams,
) *[]ExpansionHit {
	// Step 1: build seed mass from vector + fts hits.
	//
	// Max-normalize each tier independently so neither tier's score
	// magnitude dominates. For events in both tiers, average the two
	// normalized scores. Then L1-normalize the seed vector.
	seedRaw := make(map[uuid.UUID]float64)

	// Per-tier max-normalize.
	addNormalized := func(hits []MultiRankingHit) {
		var maxScore float64
		for _, h := range hits {
			if h.EventID == nil {
				continue
			}
			if h.Score.Merged > maxScore {
				maxScore = h.Score.Merged
			}
		}
		if maxScore <= 0 {
			return
		}
		for _, h := range hits {
			if h.EventID == nil {
				continue
			}
			// Accumulate: for events in both tiers this will sum two
			// normalized scores; we divide by tier count below.
			seedRaw[*h.EventID] += h.Score.Merged / maxScore
		}
	}
	addNormalized(vectorHits)
	addNormalized(ftsHits)

	// Determine how many tiers contributed to each event so we can
	// average (divide by count of tiers that scored this event).
	tierCount := make(map[uuid.UUID]int)
	for _, h := range vectorHits {
		if h.EventID != nil {
			tierCount[*h.EventID]++
		}
	}
	for _, h := range ftsHits {
		if h.EventID != nil {
			tierCount[*h.EventID]++
		}
	}
	for id, tc := range tierCount {
		if tc > 1 {
			seedRaw[id] /= float64(tc)
		}
	}

	// L1-normalize so seed vector sums to 1.
	var total float64
	for _, v := range seedRaw {
		total += v
	}
	seeds := make(map[uuid.UUID]float64, len(seedRaw))
	if total > 0 {
		for id, v := range seedRaw {
			seeds[id] = v / total
		}
	}

	if len(seeds) == 0 {
		empty := []ExpansionHit{}
		return &empty
	}

	// Step 2: BFS neighborhood graph.
	seedIDs := make([]uuid.UUID, 0, len(seeds))
	for id := range seeds {
		seedIDs = append(seedIDs, id)
	}
	g, err := BuildSettleGraph(ctx, assocLookup, seedIDs, workspaceID, "hebbian", p)
	if err != nil {
		slog.Warn("settle: BuildSettleGraph failed (best-effort, dropping expansion)",
			"err", err.Error(),
			"workspace_id", workspaceID,
		)
		return nil
	}

	// Step 3: settle to convergence.
	act, _ := primitives.Settle(seeds, g, p)

	// Step 4: top-M non-seed expansion nodes.
	expansionIDs := primitives.TopExpansion(act, seeds, p.TopM)

	// Always return a non-nil pointer (empty slice when no expansion).
	result := make([]ExpansionHit, 0, len(expansionIDs))
	if len(expansionIDs) == 0 {
		return &result
	}

	// Step 5: hydrate text for expansion IDs.
	texts, err := repo.GetEventTextByIDs(ctx, expansionIDs, userID, workspaceID)
	if err != nil {
		slog.Warn("settle: GetEventTextByIDs failed (best-effort, dropping expansion)",
			"err", err.Error(),
			"workspace_id", workspaceID,
			"expansion_count", len(expansionIDs),
		)
		return nil
	}

	for _, id := range expansionIDs {
		hit := ExpansionHit{
			EventID:    id,
			Activation: act[id],
		}
		if t, ok := texts[id]; ok {
			hit.Text = &t
		}
		result = append(result, hit)
	}
	return &result
}

// ---------------------------------------------------------------------
// Multi-ranking request/response types — wire shape for /v1/episodic/query.
//
// Sub-lists `vector`, `fts`, `session_vector` mirror ghola.Recall's RRF
// fan-out so the client can issue one HTTP round-trip instead of three.
// A future `primitives` sub-list lands in D1 behind a flag.
// ---------------------------------------------------------------------

// MultiRankingRequest is the wire shape for /v1/episodic/query.
// Required: a non-empty Rankings list naming the per-tier subset to
// fan out across. Exported so the ghola client can marshal it without
// redefining the shape on its side.
type MultiRankingRequest struct {
	UserID         uuid.UUID `json:"user_id"`
	WorkspaceID    uuid.UUID `json:"workspace_id"`
	QueryText      string    `json:"query_text,omitempty"`
	QueryEmbedding []float64 `json:"query_embedding,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	IncludeShared  *bool     `json:"include_shared,omitempty"`
	TagsAny        []string  `json:"tags_any,omitempty"`
	// Rankings names the tiers to rank separately
	// ("vector", "fts", "session_vector"). A non-empty list is
	// required — the handler rejects an empty/absent list with 400.
	Rankings []string `json:"rankings,omitempty"`
	// Primitives opts the response into the Hebbian-boosted fourth
	// sub-list (D1). When true, the handler computes per-candidate
	// boost over the union of vector + fts top-K event ids and
	// returns the result as `response.primitives`. Default off keeps
	// the baseline cheap; the ghola client flips this on when the
	// caller asks for primitive enrichment (D2).
	Primitives bool `json:"primitives,omitempty"`
	// Settle opts the response into the P4 recurrent-settle expansion
	// sub-list.  When present and enabled:true, runs spreading activation
	// over the Hebbian neighborhood seeded by the vector+fts hits and
	// returns the top-M non-seed nodes as `response.expansion`.
	// All params are optional — omitted fields take defaults from
	// primitives.DefaultSettleParams().  Absent entirely (nil) means
	// disabled; expansion is never present in the response.
	Settle *SettleRequest `json:"settle,omitempty"`
}

// SettleRequest is the optional settle configuration block on
// MultiRankingRequest.  Presence of this field (even with just
// `{"enabled":true}`) enables the expansion sub-list.  All numeric
// params are optional; zero values fall back to DefaultSettleParams().
type SettleRequest struct {
	Enabled  bool    `json:"enabled"`
	Lambda   float64 `json:"lambda,omitempty"`
	HopCap   int     `json:"hop_cap,omitempty"`
	NodeCap  int     `json:"node_cap,omitempty"`
	TopM     int     `json:"top_m,omitempty"`
	Eps      float64 `json:"eps,omitempty"`
	MaxIters int     `json:"max_iters,omitempty"`
}

// params builds a primitives.SettleParams from the request, applying
// DefaultSettleParams() for any unset (zero-value) field.
func (s *SettleRequest) params() primitives.SettleParams {
	p := primitives.DefaultSettleParams()
	if s.Lambda > 0 {
		p.Lambda = s.Lambda
	}
	if s.HopCap > 0 {
		p.HopCap = s.HopCap
	}
	if s.NodeCap > 0 {
		p.NodeCap = s.NodeCap
	}
	if s.TopM > 0 {
		p.TopM = s.TopM
	}
	if s.Eps > 0 {
		p.Eps = s.Eps
	}
	if s.MaxIters > 0 {
		p.MaxIters = s.MaxIters
	}
	return p
}

// MultiRankingHit is the shared hit shape across all per-tier sub-lists
// of MultiRankingResponse. event_id, session_id, and tier are all
// omitempty so a tier that doesn't produce one of them (e.g.
// session_vector has no per-event id) doesn't litter the wire with
// zero-uuid noise.
//
// score uses ,omitzero (Go 1.24+) rather than ,omitempty because the
// field is a value-typed struct — encoding/json's ,omitempty is a
// no-op for non-pointer structs, so a zero-value Score would always
// emit `"score":{"semantic":0,"fts":0,"merged":0}`. ,omitzero treats
// the all-zero struct as empty and drops it. text / session_chunk_text
// follow the per-grain convention the recall-side fusion in ghola
// expects: for event-grain hits (vector / fts tiers), `text` carries
// the event content and `session_chunk_text` carries the consolidated
// session chunk for cross-encoder rerank input; for session-grain
// hits (session_vector tier) `text` is omitted and `session_chunk_text`
// alone carries the consolidated chunk. Uniform across tiers so the
// reranker scores against a single surface.
type MultiRankingHit struct {
	EventID          *uuid.UUID     `json:"event_id,omitempty"`
	SessionID        *uuid.UUID     `json:"session_id,omitempty"`
	Tier             string         `json:"tier,omitempty"`
	Score            ScoreBreakdown `json:"score,omitzero"`
	Text             *string        `json:"text,omitempty"`
	SessionChunkText string         `json:"session_chunk_text,omitempty"`
}

// MultiRankingResponse carries one ranked sub-list per requested tier.
// Sub-list keys are the snake-case tier names ghola.Recall consumes
// (vector, fts, session_vector, primitives). A tier that wasn't
// requested simply omits its sub-list (omitempty); a requested tier
// with zero hits serializes as an empty array rather than null so
// client code can iterate without a nil-check.
//
// Primitives is the Hebbian-boosted fourth sub-list (D1). It uses a
// pointer-to-slice rather than a plain slice so the wire contract has
// three distinct states: absent (flag was false → omit the key),
// present-and-empty (flag was true, no in-set boosts → `[]`), and
// present-with-hits (`[…]`). encoding/json's `,omitempty` collapses
// nil and empty slices to "absent" for plain slices, which would lose
// the second state. The pointer makes the absence/presence signal
// explicit; nil is dropped by `,omitempty`, an addressable empty
// slice marshals as `[]`.
//
// Expansion is the P4 recurrent-settle expansion sub-list. Same
// pointer-to-slice three-state contract as Primitives. NOT part of
// any tier list — it is a separate output surface.
type MultiRankingResponse struct {
	Vector        []MultiRankingHit  `json:"vector,omitempty"`
	FTS           []MultiRankingHit  `json:"fts,omitempty"`
	SessionVector []MultiRankingHit  `json:"session_vector,omitempty"`
	Primitives    *[]MultiRankingHit `json:"primitives,omitempty"`
	Expansion     *[]ExpansionHit    `json:"expansion,omitempty"`
}

// ExpansionHit is one entry in the settle expansion sub-list.
// Each entry is a non-seed node whose activation exceeded the seed
// set after the recurrent settle converged.  Activations are in
// descending order.  text is present when the event has a non-NULL
// text column and the caller's workspace ACL grants access.
type ExpansionHit struct {
	EventID    uuid.UUID `json:"event_id"`
	Activation float64   `json:"activation"`
	Text       *string   `json:"text,omitempty"`
}

// ---------------------------------------------------------------------
// Share
// ---------------------------------------------------------------------

type shareRequest struct {
	OwnerUserID uuid.UUID  `json:"owner_user_id"`
	Target      string     `json:"target"`
	TargetID    *uuid.UUID `json:"target_id,omitempty"`
	ScopeType   string     `json:"scope_type"`
	ScopeID     uuid.UUID  `json:"scope_id"`
}

type shareResponse struct {
	ID uuid.UUID `json:"id"`
}

func (h *EpisodicHandler) Share(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserIDFromContext(r.Context())
	if caller == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req shareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.OwnerUserID != caller {
		apierror.Forbidden("owner_user_id must match caller").WriteJSON(w)
		return
	}
	switch req.Target {
	case "team", "user":
	default:
		apierror.BadRequest("target must be 'team' or 'user'").WriteJSON(w)
		return
	}
	if req.Target == "user" && (req.TargetID == nil || *req.TargetID == uuid.Nil) {
		apierror.BadRequest("target_id is required when target='user'").WriteJSON(w)
		return
	}
	switch req.ScopeType {
	case "session", "branch", "event":
	default:
		apierror.BadRequest("scope_type must be 'session' | 'branch' | 'event'").WriteJSON(w)
		return
	}
	if req.ScopeID == uuid.Nil {
		apierror.BadRequest("scope_id is required").WriteJSON(w)
		return
	}

	id, err := h.repo.CreateEpisodicShare(r.Context(), repository.CreateShareParams{
		Caller:    caller,
		Target:    req.Target,
		TargetID:  req.TargetID,
		ScopeType: req.ScopeType,
		ScopeID:   req.ScopeID,
	})
	if err != nil {
		if errors.Is(err, repository.ErrShareNotOwned) {
			apierror.Forbidden("caller does not own the referenced scope").WriteJSON(w)
			return
		}
		Error(w, r, apierror.InternalError("share failed").WithError(err))
		return
	}

	OK(w, shareResponse{ID: id})
}

// ---------------------------------------------------------------------
// Forget
// ---------------------------------------------------------------------

type forgetRequest struct {
	UserID   uuid.UUID   `json:"user_id"`
	EventIDs []uuid.UUID `json:"event_ids"`
}

type forgetResponse struct {
	Forgotten int `json:"forgotten"`
}

func (h *EpisodicHandler) Forget(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserIDFromContext(r.Context())
	if caller == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req forgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.UserID != uuid.Nil && req.UserID != caller {
		apierror.Forbidden("user_id must match caller").WriteJSON(w)
		return
	}
	if len(req.EventIDs) == 0 {
		apierror.BadRequest("event_ids must be non-empty").WriteJSON(w)
		return
	}

	forgotten, err := h.repo.SoftDeleteEpisodicEvents(r.Context(), caller, req.EventIDs)
	if err != nil {
		Error(w, r, apierror.InternalError("forget failed").WithError(err))
		return
	}

	OK(w, forgetResponse{Forgotten: forgotten})
}

// ---------------------------------------------------------------------
// AddSessionWorkspace
// ---------------------------------------------------------------------

type addSessionWorkspaceRequest struct {
	UserID      uuid.UUID `json:"user_id"`
	SessionID   uuid.UUID `json:"session_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
}

type addSessionWorkspaceResponse struct {
	Added bool `json:"added"`
}

// AddSessionWorkspace tags an existing session into an additional
// workspace. Wire surface for repo.AddSessionWorkspace; the ghola
// HTTP layer re-emits the same status codes upstream so an MCP-side
// agent reads a coherent error story end-to-end.
//
// Error mapping:
//   - ErrSessionNotFound  -> 409 with a literal "consolidate first"
//     hint (the consolidate handshake is the only fix; the message
//     tells the caller what to do without a docs lookup).
//   - ErrSessionNotOwned  -> 403 (per-user ACL).
//   - decode / nil-UUID   -> 400.
//   - missing auth        -> 401.
//   - everything else     -> 500.
//
// Happy path returns {added: bool}; added=false means the row already
// existed (idempotent re-tag).
func (h *EpisodicHandler) AddSessionWorkspace(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserIDFromContext(r.Context())
	if caller == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req addSessionWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.UserID == uuid.Nil || req.SessionID == uuid.Nil || req.WorkspaceID == uuid.Nil {
		apierror.BadRequest("user_id, session_id, workspace_id are required").WriteJSON(w)
		return
	}
	if req.UserID != caller {
		apierror.Forbidden("user_id must match caller").WriteJSON(w)
		return
	}

	added, err := h.repo.AddSessionWorkspace(r.Context(), repository.AddSessionWorkspaceParams{
		UserID:      req.UserID,
		SessionID:   req.SessionID,
		WorkspaceID: req.WorkspaceID,
	})
	if errors.Is(err, repository.ErrSessionNotFound) {
		apierror.Conflict("session not yet persisted; consolidate first").WriteJSON(w)
		return
	}
	if errors.Is(err, repository.ErrSessionNotOwned) {
		apierror.Forbidden("session not owned by caller").WriteJSON(w)
		return
	}
	if err != nil {
		Error(w, r, apierror.InternalError("add_session_workspace failed").WithError(err))
		return
	}

	OK(w, addSessionWorkspaceResponse{Added: added})
}
