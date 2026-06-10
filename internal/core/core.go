package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/logan-broit/ghola/internal/truthsayer"
)

// Core is the single behavioral surface the HTTP + MCP wrappers share.
type Core struct {
	Sietch       SietchStore
	Chapterhouse ChapterhouseClient
	Embedder     Embedder
	// Truthsayer is the cross-encoder reranker client. nil disables
	// Stage 2/3 — Recall falls back to RRF-only and never errors on
	// the rerank path. Wired by main.go from TRUTHSAYER_URL env.
	Truthsayer *truthsayer.Client
	// Now is the time source; tests inject a fixed clock.
	Now func() time.Time
	// RRFK is the reciprocal-rank-fusion constant used by Recall to
	// merge ranked tiers. 60 is the literature default. Smaller k lets
	// the very top ranks dominate; larger k flattens the curve.
	RRFK int
	// RerankTopK is the size of the candidate pool sent to Truthsayer.
	// 50 matches the bench reference and gives the cross-encoder a
	// meaningful chance to reorder beyond the user's top-N. Capped by
	// fan-out width when the actual fused pool is smaller.
	RerankTopK int
	// RerankWeight is the cross-encoder's share of the final fused
	// score, [0,1]. 0 = RRF only, 1 = reranker only.
	//
	// Default 0.5 is the bench-validated balance point. The PR-C.3
	// sweep on LongMemEval-S found 0.7 was a +0.6pp R@5 win at the
	// time (57.6% vs 57.0%), but only because two of the three RRF
	// tiers were empty (semantic.mnemes pending PR-E, BM25 keyword
	// pending PR-D), so RRF was effectively a single-tier rank-list.
	// With the full three-tier fan-out, the RRF agreement signal
	// strengthens and the equilibrium shifts back toward 0.5. Holding
	// the default here so we don't over-fit to the under-populated
	// RRF distribution; re-run the sweep after PR-D and again after
	// PR-E to set a data-driven default against the production shape.
	RerankWeight float64
	// RerankTimeout caps the truthsayer round-trip. Failures (timeout,
	// 5xx, transport) degrade to RRF-only with a warn log.
	RerankTimeout time.Duration
}

// New builds a Core with sensible defaults.
func New(s SietchStore, ch ChapterhouseClient, emb Embedder) *Core {
	return &Core{
		Sietch:        s,
		Chapterhouse:  ch,
		Embedder:      emb,
		Now:           func() time.Time { return time.Now().UTC() },
		RRFK:          60,
		RerankTopK:    50,
		RerankWeight:  0.5,
		RerankTimeout: 30 * time.Second,
	}
}

// ErrValidation marks input-validation errors that should surface as
// HTTP 400 at the boundary. Callers use errors.Is(err, ErrValidation)
// instead of string-matching the message text. Every sentinel below
// wraps it, plus any ad-hoc validation errors thrown inline (e.g. the
// invalid-UUID case in SessionStart).
var ErrValidation = errors.New("validation")

var (
	ErrMissingSessionID = fmt.Errorf("%w: session_id required", ErrValidation)
	ErrMissingUserID    = fmt.Errorf("%w: user_id required", ErrValidation)
	ErrMissingWorkspace = fmt.Errorf("%w: workspace required", ErrValidation)
	// ErrMissingWorkspaceOrCwd: SessionStart needs at least one anchor
	// for workspace scoping. Symmetric with ErrMissingWorkspace on the
	// recall path; together they enforce "every chapterhouse-bound
	// query carries a workspace, every ingested session is scoped to
	// one."
	ErrMissingWorkspaceOrCwd = fmt.Errorf("%w: workspace_id or cwd required", ErrValidation)
)

// SessionStart provisions a new sietch file + inserts the session row.
func (c *Core) SessionStart(ctx context.Context, in SessionStartInput) (Session, error) {
	if in.UserID == "" {
		return Session{}, ErrMissingUserID
	}

	// Resolve workspace_id: explicit > cwd-derived > error.
	//
	// The explicit `*in.Cwd != ""` guard catches both nil-pointer and
	// empty-string-pointer cases — without it a non-nil pointer to ""
	// would derive WorkspaceForCwd("") and silently collapse all
	// empty-cwd sessions to one workspace_id.
	var wsID uuid.UUID
	switch {
	case in.WorkspaceID != "":
		parsed, err := uuid.Parse(in.WorkspaceID)
		if err != nil {
			return Session{}, fmt.Errorf("%w: workspace_id must be a valid UUID: %w", ErrValidation, err)
		}
		wsID = parsed
	case in.Cwd != nil && *in.Cwd != "":
		wsID = WorkspaceForCwd(*in.Cwd)
	default:
		return Session{}, ErrMissingWorkspaceOrCwd
	}

	s := Session{
		ID:           NewID(),
		UserID:       in.UserID,
		StartedAt:    c.Now(),
		WorkspaceID:  wsID.String(),
		AgentKind:    in.AgentKind,
		Cwd:          in.Cwd,
		GitBranch:    in.GitBranch,
		SourceDevice: in.SourceDevice,
	}
	if err := c.Sietch.OpenSession(ctx, s); err != nil {
		return Session{}, fmt.Errorf("open session: %w", err)
	}
	return s, nil
}

// resolveImplicitSession picks (or provisions) a session when Record is
// called without an explicit SessionID. Rule: reuse the most-recent
// un-ended session for (UserID, WorkspaceForCwd(*Cwd)); else create
// one via SessionStart. Cwd is required — without it there's no
// workspace anchor, and we fail closed with ErrMissingWorkspaceOrCwd
// (a validation error → boundary returns 400).
func (c *Core) resolveImplicitSession(ctx context.Context, in RecordInput) (string, error) {
	if in.Cwd == nil || *in.Cwd == "" {
		return "", ErrMissingWorkspaceOrCwd
	}
	workspaceID := WorkspaceForCwd(*in.Cwd).String()

	sessions, err := c.Sietch.ListSessions(ctx, in.UserID)
	if err != nil {
		return "", fmt.Errorf("list sessions (user=%q): %w", in.UserID, err)
	}
	var best *Session
	for i := range sessions {
		s := &sessions[i]
		if s.WorkspaceID != workspaceID || s.EndedAt != nil {
			continue
		}
		if best == nil || s.StartedAt.After(best.StartedAt) {
			best = s
		}
	}
	if best != nil {
		return best.ID, nil
	}

	sess, err := c.SessionStart(ctx, SessionStartInput{
		UserID: in.UserID,
		Cwd:    in.Cwd,
	})
	if err != nil {
		return "", fmt.Errorf("auto session_start: %w", err)
	}
	return sess.ID, nil
}

// SessionEnd finalizes the session — flushes any remaining events to
// chapterhouse and closes the sietch handle. Pipeline A's continuous
// tick (Phase 5) supersedes most of this; SessionEnd is the explicit
// fallback for agents that quit before the tick.
//
// Order matters: MarkEnded must precede Consolidate so the
// chapterhouse upsert carries ended_at. The chapterhouse reconciler
// pools sessions where `ended_at IS NOT NULL AND l1_embedding IS
// NULL` — leaving ended_at NULL silently disables predictive replay
// for the entire session.
func (c *Core) SessionEnd(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return ErrMissingSessionID
	}
	if err := c.Sietch.MarkEnded(ctx, sessionID, c.Now()); err != nil {
		return fmt.Errorf("mark ended (session=%q): %w", sessionID, err)
	}
	if _, err := c.Consolidate(ctx, sessionID); err != nil {
		return fmt.Errorf("consolidate before end (session=%q): %w", sessionID, err)
	}
	return c.Sietch.CloseSession(ctx, sessionID)
}

// ListSessions enumerates a user's sessions (sietch-visible slice
// only for now — episodic listing lands with Pipeline B in Phase 8).
func (c *Core) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	if userID == "" {
		return nil, ErrMissingUserID
	}
	return c.Sietch.ListSessions(ctx, userID)
}

// Record appends an event to the current session. The embedding is
// computed when absent.
//
// When SessionID is empty, Record falls back to cwd-derived session
// resolution (resolveImplicitSession): reuse the most-recent open
// session for (UserID, WorkspaceForCwd(*Cwd)), else provision one
// inline. This is the affordance for MCP hosts (Claude Code, Codex,
// Cursor) where the protocol has no conversation-start hook, so the
// model would otherwise have to remember to call session_start
// itself — which it usually doesn't.
func (c *Core) Record(ctx context.Context, in RecordInput) (Event, error) {
	if in.UserID == "" {
		return Event{}, ErrMissingUserID
	}
	if in.SessionID == "" {
		sid, err := c.resolveImplicitSession(ctx, in)
		if err != nil {
			return Event{}, err
		}
		in.SessionID = sid
	}

	ev := in.Event
	if ev.ID == "" {
		ev.ID = NewID()
	}
	ev.SessionID = in.SessionID
	ev.UserID = in.UserID
	if ev.ParentID == nil && in.ParentID != nil {
		ev.ParentID = in.ParentID
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = c.Now()
	}

	if len(ev.Embedding) == 0 && ev.Text != nil && *ev.Text != "" {
		emb, err := c.Embedder.Embed(ctx, *ev.Text)
		if err != nil {
			// Never lose a write because the embedder is down. The
			// event lands in sietch without an embedding; the encoding
			// worker's backfill pass (BackfillEmbeddings) fills it in
			// when the embedder recovers, and Consolidate holds it
			// back from chapterhouse until then.
			slog.WarnContext(ctx, "embed failed; recording without embedding",
				"session_id", ev.SessionID, "err", err.Error())
		} else {
			ev.Embedding = emb
		}
	}

	stored, err := c.Sietch.RecordEvent(ctx, ev)
	if err != nil {
		return Event{}, fmt.Errorf("record (session=%q): %w", ev.SessionID, err)
	}
	if err := c.Sietch.SetCurrent(ctx, stored.SessionID, stored.ID); err != nil {
		return stored, fmt.Errorf("set current: %w", err)
	}
	return stored, nil
}

// BackfillEmbeddings embeds events recorded while the embedder was
// down (Record degrades to no-embedding rather than losing the write).
// Returns the number backfilled. Called by the encoding worker before
// each Consolidate pass; safe to call any time.
func (c *Core) BackfillEmbeddings(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, ErrMissingSessionID
	}
	const batch = 64
	total := 0
	for {
		evs, err := c.Sietch.EventsNeedingEmbedding(ctx, sessionID, batch)
		if err != nil {
			return total, fmt.Errorf("events needing embedding (session=%q): %w", sessionID, err)
		}
		if len(evs) == 0 {
			return total, nil
		}
		for _, ev := range evs {
			emb, err := c.Embedder.Embed(ctx, *ev.Text)
			if err != nil {
				return total, fmt.Errorf("backfill embed (session=%q event=%q): %w", sessionID, ev.ID, err)
			}
			if err := c.Sietch.SetEmbedding(ctx, sessionID, ev.ID, emb); err != nil {
				return total, fmt.Errorf("set embedding (event=%q): %w", ev.ID, err)
			}
			total++
		}
		if len(evs) < batch {
			return total, nil
		}
	}
}

// Branch starts a new child event from an existing parent and moves
// the session's current pointer to the new node.
func (c *Core) Branch(ctx context.Context, in RecordInput) (Event, error) {
	if in.ParentID == nil || *in.ParentID == "" {
		return Event{}, errors.New("parent_id required for branch")
	}
	return c.Record(ctx, in)
}

// Bookmark labels an event for later recall / review.
func (c *Core) Bookmark(ctx context.Context, sessionID, eventID, label string) error {
	if sessionID == "" {
		return ErrMissingSessionID
	}
	if eventID == "" || label == "" {
		return errors.New("event_id and label required")
	}
	return c.Sietch.SetBookmark(ctx, sessionID, eventID, label)
}

// Navigate moves the current-event pointer.
func (c *Core) Navigate(ctx context.Context, sessionID, eventID string) error {
	if sessionID == "" {
		return ErrMissingSessionID
	}
	if eventID == "" {
		return errors.New("event_id required")
	}
	return c.Sietch.SetCurrent(ctx, sessionID, eventID)
}

// Recall fans out across tiers, fuses their ranked outputs with
// reciprocal rank fusion (Cormack 2009; see internal/core/rrf.go) and
// returns the top-N hits. Each output hit's Score is the RRF score, not
// the original tier-level cosine/FTS score — the per-tier score scales
// don't compete on the same axis, which is exactly what RRF dodges.
//
// Tier dedup: within a tier, multiple events from the same session
// collapse to the highest-scoring event for ranking purposes. Across
// tiers, the same session/document is fused into one output hit. The
// exemplar event (used for Content + ID + Tier attribution in the
// output) is the one with the highest *raw* tier score across the
// tiers that returned it.
func (c *Core) Recall(ctx context.Context, in RecallInput) (RecallResult, error) {
	if in.UserID == "" {
		return RecallResult{}, ErrMissingUserID
	}
	if in.Workspace == "" {
		return RecallResult{}, ErrMissingWorkspace
	}
	if in.Limit <= 0 {
		in.Limit = 10
	}

	// Default to all three tiers when the caller didn't say.
	if !in.IncludeSietch && !in.IncludeEpisode && !in.IncludeSemant {
		in.IncludeSietch = true
		in.IncludeEpisode = true
		in.IncludeSemant = true
	}

	// Per-stage wall-clock timings, opt-in via in.IncludeTimings.
	// Default off so agent callers (Claude via MCP) don't pay the
	// context-window cost of ~250 bytes of diagnostic JSON per recall;
	// bench harnesses and explicit debugging callers set the flag and
	// get the full breakdown. When off, the map stays nil, time.Now()
	// calls are skipped at the call sites, and the response Timings
	// field is omitted (omitempty on RecallResult.Timings).
	//
	// Concurrency-safe by construction: parallel tiers each capture
	// their own duration to a closure-local variable regardless of
	// the flag (cheap); the timings map is written only from the main
	// goroutine after Wait, gated on the flag.
	var timings map[string]float64
	var recallStart time.Time
	if in.IncludeTimings {
		timings = make(map[string]float64, 12)
		recallStart = time.Now()
	}

	var emb []float32
	if in.QueryText != "" {
		var embStart time.Time
		if in.IncludeTimings {
			embStart = time.Now()
		}
		e, err := c.Embedder.Embed(ctx, in.QueryText)
		if in.IncludeTimings {
			timings["embed"] = ms(time.Since(embStart))
		}
		if err != nil {
			return RecallResult{}, fmt.Errorf("embed query (user=%q workspace=%q): %w", in.UserID, in.Workspace, err)
		}
		emb = e
	}

	// Per-tier fan-out, parallelized via errgroup. Each tier writes to
	// its own captured slice — no shared state, no mutex needed. The
	// errgroup-derived context cancels in-flight tiers if any one
	// returns an error, so we don't waste compute on results that will
	// be discarded. Sietch's two sub-queries (vector + FTS) write to
	// separate slices and merge after Wait so we don't race on a single
	// append target.
	//
	// Ordering after Wait is irrelevant: RRF + dedup-by-key are
	// order-independent, and exemplar selection picks the highest raw
	// score across tiers. The sequential order the previous version had
	// (sietch → episodic → keyword → session-vector → semantic) was an
	// accident of code layout, not a behavioral contract.
	var (
		sietchVectorHits  []RecallHit
		sietchFTSHits     []RecallHit
		episodicHits      []RecallHit
		keywordHits       []RecallHit
		sessionVectorHits []RecallHit
		semanticHits      []RecallHit
		// primitivesHits is the 4th sub-list from QueryEpisodicMulti,
		// surfaced when in.Primitives is true AND the chapterhouse
		// handler returned a non-nil Primitives slice. nil → tier absent
		// from the fan-out (either flag was off, OR the server-side
		// association lookup failed and the handler dropped the field);
		// non-nil empty → flag was on but no in-set boosts surfaced (the
		// seeded set has no associations among themselves) — RRF treats
		// this as a tier with zero entries, a clean no-op.
		primitivesHits    []RecallHit

		sietchVectorDur   time.Duration
		sietchFTSDur      time.Duration
		episodicMultiDur  time.Duration
		semanticDur       time.Duration
	)

	var fanoutStart time.Time
	if in.IncludeTimings {
		fanoutStart = time.Now()
	}
	g, gctx := errgroup.WithContext(ctx)

	if in.IncludeSietch && in.SessionID != "" {
		if len(emb) > 0 {
			g.Go(func() error {
				s := time.Now()
				h, err := c.Sietch.SearchVector(gctx, in.SessionID, emb, in.Limit)
				sietchVectorDur = time.Since(s)
				if err != nil {
					return fmt.Errorf("sietch vector (session=%q): %w", in.SessionID, err)
				}
				sietchVectorHits = h
				return nil
			})
		}
		if in.QueryText != "" {
			g.Go(func() error {
				s := time.Now()
				h, err := c.Sietch.SearchFTS(gctx, in.SessionID, in.QueryText, in.Limit)
				sietchFTSDur = time.Since(s)
				if err != nil {
					return fmt.Errorf("sietch fts (session=%q): %w", in.SessionID, err)
				}
				sietchFTSHits = h
				return nil
			})
		}
	}

	if in.IncludeEpisode {
		// Build the rankings subset by gating each tier on the inputs
		// it actually uses:
		//   - "vector" runs unconditionally: the chapterhouse handler
		//     short-circuits when QueryEmbedding is empty, so the
		//     wasted call cost is negligible and the symmetry keeps
		//     the gate logic simple.
		//   - "fts" requires a query string; FTS over an empty query
		//     is meaningless.
		//   - "session_vector" requires an embedding; without one the
		//     pooled-vector cosine has nothing to score against.
		// One round-trip drives all requested tiers (errgroup is
		// preserved so the semantic tier still runs in parallel with
		// the multi call).
		rankings := make([]string, 0, 3)
		rankings = append(rankings, "vector")
		if in.QueryText != "" {
			rankings = append(rankings, "fts")
		}
		if len(emb) > 0 {
			rankings = append(rankings, "session_vector")
		}

		g.Go(func() error {
			s := time.Now()
			res, err := c.Chapterhouse.QueryEpisodicMulti(gctx, EpisodicMultiQuery{
				UserID:         in.UserID,
				WorkspaceID:    in.Workspace,
				QueryText:      in.QueryText,
				QueryEmbedding: emb,
				Limit:          in.Limit,
				IncludeShared:  in.IncludeShared,
				TagsAny:        in.TagsAny,
				Rankings:       rankings,
				Primitives:     in.Primitives,
			})
			episodicMultiDur = time.Since(s)
			if err != nil {
				return fmt.Errorf("episodic multi (user=%q workspace=%q): %w", in.UserID, in.Workspace, err)
			}
			// Map each requested ranking sub-list back onto the
			// per-tier hit slot the post-fan-out RRF logic already
			// consumes. Tier strings on the response are the
			// downstream-canonical ones the chapterhouse client
			// already remapped ("episodic", "keyword",
			// "session_vector"), so hitKey() and TierCounts work
			// unchanged.
			episodicHits = res.Vector
			keywordHits = res.FTS
			sessionVectorHits = res.SessionVector
			// Primitives uses pointer-to-slice for 3-state semantics
			// (nil → field absent on the wire; non-nil → flag was on
			// and the handler returned the sub-list, possibly empty).
			// Dereference only on non-nil; an empty slice flows through
			// to RRF as a zero-entry tier (no-op).
			if res.Primitives != nil {
				primitivesHits = *res.Primitives
			}
			return nil
		})
	}

	if in.IncludeSemant {
		g.Go(func() error {
			s := time.Now()
			h, err := c.Chapterhouse.QuerySemantic(gctx, SemanticQuery{
				Workspace:      in.Workspace,
				QueryText:      in.QueryText,
				QueryEmbedding: emb,
				Limit:          in.Limit,
			})
			semanticDur = time.Since(s)
			if err != nil {
				return fmt.Errorf("semantic: %w", err)
			}
			semanticHits = h
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return RecallResult{}, err
	}
	if in.IncludeTimings {
		timings["fanout_total"] = ms(time.Since(fanoutStart))
		if sietchVectorDur > 0 {
			timings["sietch_vector"] = ms(sietchVectorDur)
		}
		if sietchFTSDur > 0 {
			timings["sietch_fts"] = ms(sietchFTSDur)
		}
		// One key for the multi-ranking call replaces the previous
		// per-tier (episodic/keyword/session_vector) trio that A6 left
		// all carrying the same round-trip elapsed — a misleading
		// signal for any future reader trying to attribute latency
		// to a specific tier.
		if episodicMultiDur > 0 {
			timings["episodic_multi"] = ms(episodicMultiDur)
		}
		if semanticDur > 0 {
			timings["semantic"] = ms(semanticDur)
		}
	}

	// Merge sietch's two sub-tiers back into a single working-tier
	// slice. Append order is irrelevant; dedup-by-key collapses to the
	// highest score per key regardless of which sub-tier surfaced it.
	sietchHits := append(sietchVectorHits, sietchFTSHits...)
	var rrfStart time.Time
	if in.IncludeTimings {
		rrfStart = time.Now()
	}

	// Per-tier dedup → one entry per dedup key (session_id when present,
	// id otherwise — the semantic tier returns mneme rows that have no
	// session_id; using their id makes them participate in RRF as
	// distinct "documents"). Higher-score event wins within a tier.
	//
	// Session-vector hits' id is the session_id, so dedup-by-key
	// naturally collapses them with per-event episodic hits from the
	// same session. The exemplar selection below picks the highest
	// raw-tier-score across tiers — what we want.
	sietchByKey := dedupByKey(sietchHits)
	episodicByKey := dedupByKey(episodicHits)
	keywordByKey := dedupByKey(keywordHits)
	sessionVectorByKey := dedupByKey(sessionVectorHits)
	semanticByKey := dedupByKey(semanticHits)
	// primitivesByKey: 6th tier (D3). Empty/absent when in.Primitives
	// is off OR the chapterhouse handler dropped the field on
	// associations failure — dedupByKey on nil/empty returns an empty
	// map and rankedKeys yields []. FuseRRF's spec is "missing tiers
	// contribute nothing", so adding it unconditionally is safe — the
	// flag's effect is purely "did chapterhouse populate this slice".
	primitivesByKey := dedupByKey(primitivesHits)

	// Per-tier ranked key lists (best-first). These feed FuseRRF as
	// six independent ranked lists; missing tiers (empty input)
	// contribute nothing per FuseRRF's spec.
	//
	// Primitives joins as a 6th equal-weight tier rather than a
	// side-channel. Equal-weight is the safe first cut: a hit that
	// ranks #1 in primitives AND #1 in episodic gets the same RRF
	// agreement boost any other cross-tier match gets, so the eval
	// harness (D5) can measure the lift directly against the existing
	// 5-tier baseline. If the experiment shows a need for tilted
	// weights, the lever lives here (FuseRRF accepts equal-weight
	// only — a weighted variant would replace this call).
	tiers := [][]string{
		rankedKeys(sietchByKey),
		rankedKeys(episodicByKey),
		rankedKeys(keywordByKey),
		rankedKeys(sessionVectorByKey),
		rankedKeys(semanticByKey),
		rankedKeys(primitivesByKey),
	}
	fused := FuseRRF(tiers, c.RRFK)

	// Build the candidate pool. Cap depends on whether Stage 2 (rerank)
	// is configured: the cross-encoder needs a wider pool than the
	// user's top-N to have room to reorder, so we emit up to
	// RerankTopK candidates when Truthsayer is set, falling back to
	// in.Limit when it isn't (rerank disabled).
	poolSize := in.Limit
	if c.Truthsayer != nil && c.RerankTopK > poolSize {
		poolSize = c.RerankTopK
	}

	// SessionChunkText is a session-level attribute carried by whichever
	// hit happened to surface it (typically the episodic tier — see
	// chapterhouse's /v1/episodic/query JOIN). The exemplar event
	// chosen for output attribution may come from a tier that doesn't
	// carry it (sietch/working), so build a per-key lookup table from
	// every hit across every tier and copy onto the exemplar below.
	chunkTextByKey := collectSessionChunkText(sietchHits, episodicHits, keywordHits, sessionVectorHits, semanticHits, primitivesHits)

	// Re-emit one RecallHit per fused id, up to poolSize. Exemplar
	// selection: the highest raw-tier-score hit across the tiers that
	// contained this key. The output hit's Score is the RRF score; the
	// raw tier score doesn't compete on a comparable axis across tiers.
	hits := make([]RecallHit, 0, poolSize)
	rrfByID := make(map[string]float64, poolSize)
	tierMaps := []map[string]RecallHit{sietchByKey, episodicByKey, keywordByKey, sessionVectorByKey, semanticByKey, primitivesByKey}
	for _, sd := range fused {
		var best *RecallHit
		for _, tm := range tierMaps {
			if h, ok := tm[sd.ID]; ok {
				if best == nil || h.Score > best.Score {
					hCopy := h
					best = &hCopy
				}
			}
		}
		if best == nil {
			continue
		}
		best.Score = sd.Score
		// Surface the session-scoped chunk text on the exemplar so the
		// output hit carries it (downstream HTTP/MCP clients see it)
		// AND so rerankAndFuse can read it without a parallel param.
		if best.SessionChunkText == "" {
			best.SessionChunkText = chunkTextByKey[sd.ID]
		}
		hits = append(hits, *best)
		rrfByID[best.ID] = sd.Score
		if len(hits) >= poolSize {
			break
		}
	}

	if in.IncludeTimings {
		timings["rrf_dedup"] = ms(time.Since(rrfStart))
	}

	// Stage 2/3: cross-encoder rerank + score fusion. Only runs when
	// Truthsayer is configured AND there's a non-empty query string
	// (the cross-encoder is a query-vs-document model — without a
	// query the call is meaningless). Failures degrade to RRF-only
	// silently (warn-logged): recall must never error because rerank
	// is down.
	if c.Truthsayer != nil && in.QueryText != "" && len(hits) > 0 {
		var rerankStart time.Time
		if in.IncludeTimings {
			rerankStart = time.Now()
		}
		hits = c.rerankAndFuse(ctx, in.QueryText, hits, rrfByID)
		if in.IncludeTimings {
			timings["rerank"] = ms(time.Since(rerankStart))
		}
	}

	// Truncate to user-requested limit (rerank pool was wider).
	if len(hits) > in.Limit {
		hits = hits[:in.Limit]
	}

	counts := map[string]int{"working": 0, "episodic": 0, "keyword": 0, "session_vector": 0, "semantic": 0, "primitives": 0}
	for _, h := range hits {
		counts[h.Tier]++
	}
	if in.IncludeTimings {
		timings["total"] = ms(time.Since(recallStart))
	}
	return RecallResult{Hits: hits, TierCounts: counts, Timings: timings}, nil
}

// ms converts a duration to milliseconds with sub-ms precision (3 decimals).
func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// rerankAndFuse runs Stage 2 (cross-encoder rerank) and Stage 3 (score
// fusion) on the post-RRF candidate pool. On any error the function
// returns the input hits unchanged with a warn log — recall never
// fails because rerank is unavailable.
func (c *Core) rerankAndFuse(ctx context.Context, query string, hits []RecallHit, rrfByID map[string]float64) []RecallHit {
	// Build candidates only for hits that have text the reranker can
	// score. Skip no-text hits (semantic mnemes today; possibly other
	// tiers in the future) — sending them as empty-text candidates
	// would have the cross-encoder return ~0, and FuseScores would
	// then systematically demote them despite their RRF signal.
	// FuseScores handles missing-rerank entries by trusting the RRF
	// prior, so omitting them here keeps the prior intact.
	candidates := make([]truthsayer.Candidate, 0, len(hits))
	for _, h := range hits {
		text := h.SessionChunkText
		if text == "" {
			text = h.Content
		}
		if text == "" {
			continue
		}
		candidates = append(candidates, truthsayer.Candidate{ID: h.ID, Text: text})
	}
	if len(candidates) == 0 {
		// No candidate had text — nothing to rerank. Score fusion
		// against an empty rerank map degenerates cleanly to RRF-only.
		return hits
	}

	rerankCtx, cancel := context.WithTimeout(ctx, c.RerankTimeout)
	defer cancel()
	scored, err := c.Truthsayer.Rerank(rerankCtx, query, candidates, 0)
	if err != nil {
		slog.WarnContext(ctx, "truthsayer rerank failed; falling back to RRF-only",
			"err", err.Error(), "candidates", len(candidates))
		return hits
	}

	rerankByID := make(map[string]float64, len(scored))
	for _, s := range scored {
		rerankByID[s.ID] = s.Score
	}
	fused := FuseScores(rrfByID, rerankByID, c.RerankWeight)

	for i := range hits {
		if v, ok := fused[hits[i].ID]; ok {
			hits[i].Score = v
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

// dedupByKey collapses hits by SessionID (when set) or ID (otherwise),
// keeping the highest-score hit per key. Used per-tier before RRF so a
// session that produced multiple matching events doesn't multi-count.
func dedupByKey(hits []RecallHit) map[string]RecallHit {
	byKey := make(map[string]RecallHit, len(hits))
	for _, h := range hits {
		key := hitKey(h)
		cur, ok := byKey[key]
		if !ok || h.Score > cur.Score {
			byKey[key] = h
		}
	}
	return byKey
}

// hitKey returns the rerank/RRF dedup key for a hit, namespaced by
// grain so that session-grain hits (session-vector tier, where the
// hit's id IS the session_id and represents the whole session as one
// document) live in a separate key space from event-grain hits
// (episodic / keyword / working — one row per matched event).
//
// Pre-fix bug: session-vector hits collided with event-grain hits from
// the same session and the exemplar-selection step picked the higher
// raw-score one (always the session-vector hit, raw cosine 1.0 on the
// pooled embedding), silently erasing the per-event identities of the
// event-grain matches. Empirical impact: top-K event recall drops
// because resolving commits/PRs that score lower than the surrounding
// session summary never make it into the output.
//
// Grain rules:
//   - session_vector tier: session-grain. Key = "session:" + session_id
//     (h.ID == session_id for this tier).
//   - working / episodic / keyword: event-grain. Key = "event:" + event_id.
//   - semantic: mneme-grain (no session_id, distinct documents). Key =
//     "event:" + h.ID is fine — semantic hits don't collide with
//     either of the other grains because mneme ids are distinct from
//     event ids.
func hitKey(h RecallHit) string {
	if h.Tier == "session_vector" {
		return "session:" + h.ID
	}
	return "event:" + h.ID
}

// collectSessionChunkText builds a per-key lookup of the role-prefixed
// full session text. Chapterhouse JOINs episodic.sessions.l1_chunk_text
// into the /v1/episodic/query response, so episodic hits carry it; the
// sietch and semantic tiers don't. First non-empty value per key wins
// (all events from the same session row carry the same value, so the
// "first" choice is just deterministic-by-iteration).
func collectSessionChunkText(tiers ...[]RecallHit) map[string]string {
	out := make(map[string]string, 32)
	for _, tier := range tiers {
		for _, h := range tier {
			if h.SessionChunkText == "" {
				continue
			}
			key := hitKey(h)
			if _, ok := out[key]; ok {
				continue
			}
			out[key] = h.SessionChunkText
		}
	}
	return out
}

// rankedKeys returns the dedup keys sorted by raw tier score descending.
// Stable on score-tie so the result is deterministic.
func rankedKeys(byKey map[string]RecallHit) []string {
	type entry struct {
		key   string
		score float64
	}
	all := make([]entry, 0, len(byKey))
	for k, h := range byKey {
		all = append(all, entry{k, h.Score})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].key < all[j].key
	})
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = e.key
	}
	return out
}

// Forget soft-deletes events in sietch + asks chapterhouse to do the
// same in episodic. Semantic is never forgotten by event id — distilled
// mnemes flow out through replay decay, not direct deletion.
func (c *Core) Forget(ctx context.Context, in ForgetInput) (int, error) {
	if in.UserID == "" {
		return 0, ErrMissingUserID
	}
	if len(in.EventIDs) == 0 {
		return 0, errors.New("event_ids required")
	}
	if in.SessionID != "" {
		if err := c.Sietch.SoftForget(ctx, in.SessionID, in.EventIDs); err != nil {
			return 0, fmt.Errorf("sietch forget: %w", err)
		}
	}
	return c.Chapterhouse.ForgetEpisodic(ctx, in.UserID, in.EventIDs)
}

// Share grants cross-user visibility to a scope.
func (c *Core) Share(ctx context.Context, in ShareInput) (string, error) {
	if in.UserID == "" {
		return "", ErrMissingUserID
	}
	if in.ScopeID == "" {
		return "", errors.New("scope_id required")
	}
	return c.Chapterhouse.ShareEpisodic(ctx, in)
}

// ExpandSessionWorkspace tags an existing session into an additional
// workspace. Used when conversation drift makes the session relevant
// to a workspace beyond its primary at session_start. Pre-condition:
// the session has already been consolidated into chapterhouse at
// least once; otherwise the FK on session_workspaces.session_id
// rejects the row and chapterhouse returns 409 (surfaced as
// *chapterhouse.StatusError for the HTTP layer to re-emit).
func (c *Core) ExpandSessionWorkspace(ctx context.Context, in AddSessionWorkspaceInput) (bool, error) {
	if in.UserID == "" {
		return false, ErrMissingUserID
	}
	if in.SessionID == "" {
		return false, ErrMissingSessionID
	}
	if in.WorkspaceID == "" {
		return false, errors.New("workspace_id required")
	}
	return c.Chapterhouse.AddSessionWorkspace(ctx, in)
}

// Consolidate flushes the pending events (after the watermark) to
// chapterhouse's episodic store. Phase 5's Pipeline A worker calls
// this on a tick; Core exposes it so agents can force a flush on
// demand (e.g. before they exit).
func (c *Core) Consolidate(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, ErrMissingSessionID
	}
	watermark, err := c.Sietch.Watermark(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("watermark: %w", err)
	}
	pending, err := c.Sietch.PendingEvents(ctx, sessionID, watermark)
	if err != nil {
		return 0, fmt.Errorf("pending: %w", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}

	// Ship only the contiguous prefix of embedded events. Events past
	// the first un-embedded one are held back — the watermark must stay
	// a contiguous prefix of the id-ordered log, and the backfill pass
	// (BackfillEmbeddings) will fill the gap before the next tick.
	cut := len(pending)
	for i := range pending {
		if needsEmbedding(pending[i]) {
			cut = i
			break
		}
	}
	if held := len(pending) - cut; held > 0 {
		slog.InfoContext(ctx, "consolidate: holding back events awaiting embedding",
			"session_id", sessionID, "held", held)
	}
	if cut == 0 {
		return 0, nil
	}
	pending = pending[:cut]

	// Source the session row from sietch so ended_at, cwd, agent_kind,
	// etc. all flow through to chapterhouse on every consolidation
	// pass. Chapterhouse UPSERTs on id, so the same session metadata
	// can land repeatedly — but ended_at MUST propagate or the
	// predictive-replay reconciler (eligibility predicate
	// `ended_at IS NOT NULL AND l1_embedding IS NULL`) will skip the
	// session forever.
	//
	// Pipeline A's encoding worker tick reaches here with the session
	// still active: GetSession returns ended_at = nil, the upsert
	// keeps ended_at NULL, and the next tick (or eventual SessionEnd)
	// will refresh it. SessionEnd reaches here with ended_at already
	// stamped via Sietch.MarkEnded, so the row carries the timestamp.
	sess, err := c.Sietch.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("get session: %w", err)
	}
	// Defensive defaults: if sietch ever hands back a partially-zero
	// row (e.g. a session with no events), still emit something the
	// chapterhouse session-row schema accepts. UserID + StartedAt are
	// the only NOT NULL fields; pull them from the first pending event
	// when sietch left them zero.
	if sess.ID == "" {
		sess.ID = sessionID
	}
	if sess.UserID == "" {
		sess.UserID = pending[0].UserID
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = pending[0].CreatedAt
	}
	sess.EventCount = len(pending)
	if _, _, err := c.Chapterhouse.IngestEpisodic(ctx, sess, pending); err != nil {
		return 0, fmt.Errorf("ingest: %w", err)
	}
	last := pending[len(pending)-1].ID
	if err := c.Sietch.SetWatermark(ctx, sessionID, last); err != nil {
		return 0, fmt.Errorf("advance watermark: %w", err)
	}
	return len(pending), nil
}

// needsEmbedding reports whether an event should carry an embedding
// but doesn't yet (recorded while the embedder was unreachable).
func needsEmbedding(ev Event) bool {
	return len(ev.Embedding) == 0 && ev.Text != nil && *ev.Text != ""
}
