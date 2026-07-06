"""ghola attempt #2 backend — drives the local ghola HTTP service at :7421.

Indexing path (per LongMemEval session):
    POST /v1/session_start  ->  N x POST /v1/record  ->  POST /v1/session_end

ghola creates its own UUID at session_start; LongMemEval session ids are
external strings. The mapping (external_id -> ghola_uuid) is persisted to a
JSON sidecar under results/ so a retrieve invocation in a separate process
can recover it and translate hit session_ids back for scoring.

Retrieval pipeline:
    Stage 1   pull a wide pool from /v1/recall (sietch + episodic + semantic)
    Stage 1A/B  split hits by tier, dedupe to one (max-)score per session
    Stage 1F  Reciprocal Rank Fusion across tiers (rank-based; immune to
              score-scale mismatches between cosine over events and cosine
              over mneme centroids).
    Stage 2   cross-encoder rerank top RERANK_TOPK candidates
    Stage 3   score fusion: weighted sum of normalized RRF + reranker scores

Ablation knobs (env vars):
    INCLUDE_SEMANT=0     drop semantic tier from Stage 1
    RERANK_ENABLE=0      skip Stage 2 (returns RRF top-K directly)
    RERANK_WEIGHT=0..1   Stage-3 fusion weight (0=RRF only, 1=reranker only,
                         default 0.5)
    RRF_K=60             RRF constant; smaller k makes top ranks dominate
    GHOLA_V2_DELEGATE=1  trust ghola's pipeline end-to-end; skip the local
                         cross-encoder and the in-process Stage 1F/2/3
                         math, take whatever ghola returns. Used to gate
                         the recall-pipeline-productionization PR (the
                         bench acceptance gate that ghola itself drives
                         the same pipeline as the bench reference).

The semantic tier expects mneme rows in semantic.mnemes (workspace-scoped)
populated by either mentat Stage C clustering (production path) or the
HDBSCAN sidecar in scripts/cluster_l1_sidecar.py (one-off probe). With no
mneme rows the semantic tier returns zero hits and INCLUDE_SEMANT becomes
moot.
"""

from __future__ import annotations

import json
import logging
import os
import sys
import time
import uuid
from abc import ABC, abstractmethod
from pathlib import Path
from typing import Any

import httpx

logger = logging.getLogger(__name__)


# MemoryBackend is inlined here (was `from backends.base import MemoryBackend`
# in the longmemeval-ghola fork). Keeping it local means this file drops
# into any LongMemEval clone without a separate import dance, and the
# main ghola repo doesn't have to vendor the upstream `base.py`.
class MemoryBackend(ABC):
    """Abstract base for retrieval backends in the LongMemEval harness."""

    name: str = "base"

    @abstractmethod
    def setup(self, workspace_id: str, config: dict) -> None: ...

    @abstractmethod
    def index_session(
        self, session_id: str, turns: list[dict], timestamp: str
    ) -> None: ...

    @abstractmethod
    def retrieve(self, query: str, top_k: int = 10) -> list[dict]: ...

    @abstractmethod
    def reset(self) -> None: ...

    @abstractmethod
    def stats(self) -> dict: ...


class GholaV2Backend(MemoryBackend):
    """Memory backend driving ghola attempt #2's HTTP API."""

    name = "ghola_v2"

    # Stage staging: bench-side workspace-per-question scoping. The
    # retrieve harness calls set_current_question() before each
    # retrieve() so we can derive a deterministic workspace_id from
    # the question's id (uuid5 over a stable namespace). The backfill
    # script writes session_workspaces rows for that derived workspace
    # ahead of time.
    _NAMESPACE = uuid.UUID("11111111-2222-3333-4444-555555555555")

    # Stable workspace_id used at index time (when there's no
    # per-question scope yet) and for mneme preloading. uuid5 over the
    # same namespace, so the value is reproducible across machines /
    # invocations / repos. Sessions land in this workspace at
    # session_start; the post-index backfill script then adds
    # per-question (session_id, workspace_for_question(qid)) rows so
    # retrieve can scope to the per-question slice.
    _BENCH_DEFAULT_WORKSPACE = uuid.UUID("ebdd07e7-728f-5b89-a4c4-910032e12f97")

    @classmethod
    def workspace_for_question(cls, question_id: str) -> uuid.UUID:
        return uuid.uuid5(cls._NAMESPACE, question_id)

    def set_current_question(self, question_id: str) -> None:
        self._current_question_id = question_id
        self._current_workspace = str(self.workspace_for_question(question_id))
        # Cache temporal context for the current question, when the
        # temporal filter is enabled. Both the question_date and the
        # per-haystack-session date map come from the dataset record;
        # we look them up once per question and stash for retrieve()
        # to consume without re-iterating the dataset every call.
        if self._temporal_filter and self._dataset_by_qid:
            rec = self._dataset_by_qid.get(question_id)
            if rec is not None:
                self._current_question_date = rec.question_date
                self._current_haystack_dates = dict(zip(
                    rec.haystack_session_ids, rec.haystack_dates,
                ))
            else:
                self._current_question_date = ""
                self._current_haystack_dates = {}

    def __init__(self) -> None:
        self._client: httpx.Client | None = None
        self._url: str = ""
        self._user_id: str = ""
        self._workspace_id: str = ""
        self._granularity: str = "session"
        self._indexed_count: int = 0
        self._session_count: int = 0
        # external (LongMemEval) session_id -> ghola session UUID. Populated
        # in index_session(); persisted to _map_path on every session_start
        # so retrieve in a separate process can recover the mapping.
        self._session_map: dict[str, str] = {}
        self._map_path: Path | None = None
        # mneme_id -> [ghola session UUIDs in this cluster]
        # Populated from semantic.mnemes at setup() time. Semantic-tier
        # recall hits carry mneme_id with no session_id, so we expand
        # each mneme to its member sessions client-side.
        self._mneme_members: dict[str, list[str]] = {}
        # Stage-2 reranker. Loaded in setup() if RERANK_ENABLE != "0".
        # Cross-encoder over (query, chunk) pairs; we pull top-N from
        # ghola's recall, score each candidate's text chunks, take the
        # max per session and re-sort.
        self._reranker = None
        self._rerank_topk: int = 50
        self._chunk_chars: int = 1500
        # external session_id -> list of text chunks (for reranking)
        self._session_chunks: dict[str, list[str]] = {}
        # Temporal-filter state. Disabled by default; set TEMPORAL_FILTER=1
        # to enable. When on, post-retrieval results are filtered by
        # haystack date if the question contains a high-confidence
        # temporal anchor (see backends/temporal_filter.py).
        self._temporal_filter: bool = False
        self._dataset_by_qid: dict | None = None
        self._current_question_date: str = ""
        self._current_haystack_dates: dict[str, str] = {}

    # -- lifecycle --------------------------------------------------------

    def setup(self, workspace_id: str, config: dict) -> None:
        # workspace_id arg kept for MemoryBackend interface compatibility
        # but ignored: workspace_ids are resolved per-question via
        # set_current_question(). At index time (no per-question scope),
        # sessions land in _BENCH_DEFAULT_WORKSPACE, then the backfill
        # script adds per-question rows for retrieve.
        self._workspace_id = str(self._BENCH_DEFAULT_WORKSPACE)
        self._granularity = config.get("granularity", "session")

        # TEMPORAL_FILTER=1 enables a post-retrieval date filter for
        # questions with explicit relative dates ("10 days ago", "last
        # Saturday", etc.). Loads the dataset once into a qid -> record
        # map so retrieve() can pull haystack_dates without re-reading.
        # Disabled by default — see backends/temporal_filter.py for
        # rationale (false positives are worse than false negatives).
        if os.environ.get("TEMPORAL_FILTER", "0") == "1":
            from data_loader import load_dataset
            dataset_name = os.environ.get("RERANK_DATASET", "s")
            self._temporal_filter = True
            records = load_dataset(dataset_name)
            self._dataset_by_qid = {r.question_id: r for r in records}
            logger.info(
                "TEMPORAL_FILTER=1 -> loaded %d question records for date-aware post-filter",
                len(self._dataset_by_qid),
            )

        self._url = os.environ.get("GHOLA_URL", "http://localhost:7421").rstrip("/")
        self._user_id = os.environ.get(
            "GHOLA_USER_ID", "00000000-0000-0000-0000-000000000001"
        )

        # ghola listens on localhost only by default; loopback HTTP is fine.
        self._client = httpx.Client(
            headers={"Content-Type": "application/json"},
            timeout=60.0,
        )

        # Quick health probe — fail fast if the service is down.
        resp = self._client.get(f"{self._url}/health")
        resp.raise_for_status()

        # Persist external-session-id -> ghola-UUID mapping across
        # invocations: index and retrieve are separate `python run.py`
        # processes, so an in-memory dict alone gets discarded between
        # stages. The pgvector backend dodges this by storing the
        # LongMemEval session_id directly in its table; we don't have an
        # equivalent column on episodic.sessions, so we stash a JSON
        # sidecar under results/.
        #
        # The map is global (not workspace-scoped) — workspace_ids are
        # resolved per-question now, but the external-id <-> ghola-UUID
        # mapping is one-to-one regardless of which workspace the
        # session also belongs to.
        from config import RESULTS_DIR
        RESULTS_DIR.mkdir(parents=True, exist_ok=True)
        self._map_path = RESULTS_DIR / "ghola_v2_session_map.json"
        if self._map_path.exists():
            self._session_map = json.loads(self._map_path.read_text())
            logger.info(
                "GholaV2Backend loaded %d session mappings from %s",
                len(self._session_map), self._map_path,
            )

        # Pull mneme_id -> member ghola-session-UUIDs for the
        # workspace from postgres directly. Cheap (one query) and the
        # alternative — fetching mneme metadata per recall hit — would
        # add a round trip per query.
        try:
            import psycopg
            db_url = os.environ.get(
                "DATABASE_URL",
                "postgresql://memory_api:dev@localhost:5432/memories",
            )
            with psycopg.connect(db_url, autocommit=True) as c:
                with c.cursor() as cur:
                    cur.execute(
                        "SELECT id::text, member_ids::text "
                        "FROM semantic.mnemes WHERE workspace_id = %s::uuid",
                        (self._workspace_id,),
                    )
                    for mid, members_txt in cur.fetchall():
                        # member_ids::text is "{uuid1,uuid2,...}"
                        members = members_txt.strip("{}").split(",") if members_txt and members_txt != "{}" else []
                        self._mneme_members[mid] = [m.strip() for m in members if m.strip()]
            logger.info(
                "GholaV2Backend loaded %d mnemes (avg members %.1f)",
                len(self._mneme_members),
                (sum(len(v) for v in self._mneme_members.values()) /
                 max(1, len(self._mneme_members))),
            )
        except Exception as e:
            logger.warning("could not preload mnemes (semantic tier may be unused): %s", e)

        # Stage-2 cross-encoder reranker. Loaded once and reused per query.
        # Disable with RERANK_ENABLE=0 to fall back to embedding-only ranking
        # for ablation runs.
        # GHOLA_V2_DELEGATE=1 lets ghola own the pipeline; the local
        # reranker becomes dead weight (and the multi-GB model load is a
        # waste of cold-start time).
        delegate = os.environ.get("GHOLA_V2_DELEGATE", "0") == "1"
        if delegate:
            logger.info("GHOLA_V2_DELEGATE=1 -> skipping local cross-encoder; ghola owns the pipeline")
        if not delegate and os.environ.get("RERANK_ENABLE", "1") != "0":
            from sentence_transformers import CrossEncoder
            model_name = os.environ.get("RERANK_MODEL", "BAAI/bge-reranker-base")
            device = os.environ.get("RERANK_DEVICE", "cpu")
            self._rerank_topk = int(os.environ.get("RERANK_TOPK", "50"))
            self._chunk_chars = int(os.environ.get("RERANK_CHUNK_CHARS", "1500"))
            logger.info(
                "loading cross-encoder %s on %s (top_k=%d, chunk=%d chars)",
                model_name, device, self._rerank_topk, self._chunk_chars,
            )
            self._reranker = CrossEncoder(model_name, device=device, max_length=512)

            # Build external_session_id -> [chunks] map from the dataset.
            # The dataset is the source of truth for session text; we
            # don't need to round-trip through chapterhouse to score.
            from data_loader import load_dataset
            dataset_name = os.environ.get("RERANK_DATASET", "s")
            t0 = time.time()
            sessions: dict[str, str] = {}
            for record in load_dataset(dataset_name):
                for sid, turns in zip(record.haystack_session_ids, record.haystack_sessions):
                    if sid not in sessions:
                        # Concat with role prefixes so the cross-encoder
                        # sees the conversational shape.
                        sessions[sid] = "\n".join(
                            f"{t.get('role','user')}: {t.get('content','')}"
                            for t in turns
                        )
            for sid, text in sessions.items():
                if len(text) <= self._chunk_chars:
                    self._session_chunks[sid] = [text]
                else:
                    self._session_chunks[sid] = [
                        text[i:i + self._chunk_chars]
                        for i in range(0, len(text), self._chunk_chars)
                    ]
            n_chunks = sum(len(v) for v in self._session_chunks.values())
            logger.info(
                "loaded %d session texts as %d chunks in %.1fs",
                len(self._session_chunks), n_chunks, time.time() - t0,
            )

        logger.info(
            "GholaV2Backend ready: url=%s user=%s workspace=%s granularity=%s",
            self._url, self._user_id, workspace_id, self._granularity,
        )

    # -- indexing ----------------------------------------------------------

    def index_session(
        self, session_id: str, turns: list[dict], timestamp: str
    ) -> None:
        if self._granularity != "session":
            raise ValueError(
                f"ghola_v2 only supports 'session' granularity today; got {self._granularity!r}"
            )

        # Encode the LongMemEval session id in source_device so the round-trip
        # through chapterhouse is recoverable. ghola will create its own UUID;
        # we keep the mapping in memory for retrieval-time dedup.
        source_device = f"longmemeval:{session_id}"
        agent_kind = "benchmark"

        # Ghola server now requires workspace_id (or cwd) on session_start
        # (pr/session-workspaces-ingest, commit 31325c8). Prefer the
        # per-question scope set by the retrieve harness; fall back to the
        # bench's default workspace so the index path (which seeds before
        # any question is in scope) keeps working.
        ws = getattr(self, "_current_workspace", "") or self._workspace_id
        start_resp = self._post("/v1/session_start", {
            "user_id": self._user_id,
            "workspace_id": ws,
            "agent_kind": agent_kind,
            "source_device": source_device,
        })
        ghola_session_id = start_resp["session"]["id"]
        self._session_map[session_id] = ghola_session_id
        # Persist on every session_start so a mid-run crash leaves a usable
        # mapping for whatever finished. JSON dump is fast (<10ms for tens
        # of thousands of entries on a tmpfs/NVMe path).
        if self._map_path is not None:
            self._map_path.write_text(json.dumps(self._session_map))

        max_chars = int(os.environ.get("EMBED_MAX_CHARS", "0"))
        for turn in turns:
            role = turn.get("role", "user")
            content = turn.get("content", "")
            ev_type = role if role in ("user", "assistant", "system") else "user"
            # Initial truncation; the loop below halves further on
            # context-length errors from melange (mirrors pgvector backend).
            text = content
            if max_chars > 0 and len(text) > max_chars:
                text = text[:max_chars]
            for _ in range(10):
                try:
                    self._post("/v1/record", {
                        "session_id": ghola_session_id,
                        "user_id": self._user_id,
                        "event": {
                            "type": ev_type,
                            "role": role,
                            "text": text,
                            "raw_event": _raw_event(role, text, timestamp),
                        },
                    })
                    break
                except RuntimeError as e:
                    msg = str(e).lower()
                    if "context length" in msg or "maximum context" in msg or "please reduce" in msg:
                        if len(text) <= 1:
                            raise
                        text = text[: max(1, len(text) // 2)]
                        continue
                    raise
            else:
                # Exhausted halve-retries without breaking. Surface
                # rather than silently dropping the event.
                raise RuntimeError(
                    f"record failed after 10 halve-retries "
                    f"(final text len={len(text)} for {role!r} turn)"
                )
            self._indexed_count += 1

        self._post("/v1/session_end", {"session_id": ghola_session_id})
        self._session_count += 1

    # -- retrieval ---------------------------------------------------------

    def retrieve(self, query: str, top_k: int = 10) -> list[dict]:
        # Stage 1: pull a wide candidate pool from ghola's recall fan-out.
        # ghola's semantic-tier branch fires only when both
        # IncludeSemant=true and Workspace != "" (core.go:215).
        recall_limit = (
            max(top_k * 10, self._rerank_topk * 4)
            if self._reranker is not None
            else top_k * 8
        )
        # BENCH_RECALL_LIMIT overrides the computed default. Used to
        # ablate the candidate pool size sent to ghola (and therefore
        # to truthsayer's rerank). Lower values reduce rerank latency
        # at the cost of giving the reranker less material to reorder.
        if (env_limit := os.environ.get("BENCH_RECALL_LIMIT")):
            try:
                recall_limit = int(env_limit)
            except ValueError:
                pass
        # The HDBSCAN sidecar's mneme clusters are too coarse to be a
        # useful Stage-1 ranker on this corpus — fairly voting them in
        # via RRF dilutes the episodic signal more than it helps. Make
        # semantic toggleable so we can ablate it cleanly until PR4
        # ships proper clustering.
        include_semant = os.environ.get("INCLUDE_SEMANT", "1") != "0"
        payload = {
            "user_id": self._user_id,
            "query_text": query,
            "workspace": getattr(self, "_current_workspace", "") or self._workspace_id,
            "limit": recall_limit,
            "include_sietch": True,
            "include_episode": True,
            "include_semant": include_semant,
            # Opt in to per-stage timings — the bench wants the
            # diagnostic breakdown (stages/retrieve.py writes it into
            # each result line). Production agent callers (Claude via
            # MCP) leave this off to keep the response body lean.
            "include_timings": True,
        }
        # BENCH_SETTLE forwards the settle mode verbatim. Post default-on flip
        # (2026-07-06) the mapping changed:
        #   unset/empty -> field omitted -> SERVER default (now channel@0.40).
        #   BENCH_SETTLE=off -> explicit opt-out, the true pre-P4 baseline.
        #   BENCH_SETTLE=expand|channel -> P4 spreading activation (fixed-point
        #     settle over the association graph), config A / config B.
        # A measurement run must set BENCH_SETTLE explicitly so it never silently
        # rides the server default. BENCH_ACTIVATION_WEIGHT sets channel-mode
        # fusion weight; when unset the server default applies. The server
        # validates both (bad values fail the run loudly, which a bench wants).
        if (settle := os.environ.get("BENCH_SETTLE", "").strip()):
            payload["settle"] = settle
            if (env_w := os.environ.get("BENCH_ACTIVATION_WEIGHT", "").strip()):
                payload["activation_weight"] = float(env_w)
        out = self._post("/v1/recall", payload)
        # Fail loud on degraded recall. core.Recall now degrades tier-by-tier
        # (a stage timing out or erroring drops its contribution) instead of
        # failing the whole request; the response carries a `degraded` list of
        # stage names, omitted when empty. A silently-degraded recall lowers
        # benchmark scores with no signal, so abort the run naming the stages
        # and the offending question/query. GHOLA_ALLOW_DEGRADED=1 downgrades
        # this to a single loud stderr warning (debug escape hatch only — a
        # scored run must never set it).
        _check_degraded(out, getattr(self, "_current_question_id", None), query)

        hits = out.get("hits", []) if isinstance(out, dict) else []
        # Stash per-recall timings (server-side per-stage wall-clock, ms)
        # so stages/retrieve.py can write them into the result JSONL for
        # offline aggregation. Side-channel via instance attr keeps the
        # MemoryBackend.retrieve return signature unchanged.
        self._last_timings = out.get("timings", {}) if isinstance(out, dict) else {}
        rev_map = {v: k for k, v in self._session_map.items()}

        # GHOLA_V2_DELEGATE=1: ghola did Stage 1F/2/3 itself. Trust the
        # response order; just dedup by external session_id and emit
        # top_k. Each ghola hit is per-session post-fusion, so dedup is
        # a defensive belt-and-braces (semantic-tier mneme rows can
        # legitimately appear without a session_id and are skipped).
        if os.environ.get("GHOLA_V2_DELEGATE", "0") == "1":
            seen: set[str] = set()
            # When the temporal filter is on, build a wider buffer
            # (recall already pulled recall_limit hits — use that whole
            # pool, not just top_k) so post-filter has room to
            # rerank/drop without starving. Without temporal filter,
            # cap at top_k as before.
            buffer_size = recall_limit if self._temporal_filter else top_k
            out_hits: list[dict] = []
            for h in hits:
                ghola_sid = h.get("session_id")
                if not ghola_sid:
                    continue
                ext_sid = rev_map.get(ghola_sid)
                if ext_sid is None or ext_sid in seen:
                    continue
                seen.add(ext_sid)
                out_hits.append({
                    "session_id": ext_sid,
                    "score": float(h.get("score", 0.0)),
                    "rank": len(out_hits) + 1,
                })
                if len(out_hits) >= buffer_size:
                    break

            # Optional post-filter: if the question has a high-confidence
            # temporal anchor, restrict to sessions whose haystack date
            # falls in the parsed window. Falls back to unfiltered if
            # the filter empties the list. See backends/temporal_filter.py.
            if self._temporal_filter:
                from backends.temporal_filter import (
                    parse_temporal_window, post_filter_by_window,
                )
                window = parse_temporal_window(query, self._current_question_date)
                if window is not None:
                    out_hits = post_filter_by_window(
                        out_hits, self._current_haystack_dates, window,
                    )

            # Re-rank after filter and truncate to user-requested top_k.
            for i, h in enumerate(out_hits[:top_k]):
                h["rank"] = i + 1
            return out_hits[:top_k]

        # Stage 1A/1B: split hits by tier, dedupe to one score per
        # external session_id. Score scales differ across tiers so we
        # keep them apart and merge by RANK below, not by score.
        per_tier: dict[str, dict[str, float]] = {"episodic": {}, "semantic": {}}
        for h in hits:
            tier = h.get("tier") or ""
            score = float(h.get("score", 0.0))
            if tier == "semantic":
                mneme_id = h.get("id") or h.get("mneme_id")
                if not mneme_id:
                    continue
                for ghola_sid in self._mneme_members.get(mneme_id, []):
                    ext_sid = rev_map.get(ghola_sid)
                    if ext_sid is None:
                        continue
                    cur = per_tier["semantic"].get(ext_sid, -1.0)
                    if score > cur:
                        per_tier["semantic"][ext_sid] = score
                continue
            ghola_sid = h.get("session_id")
            if not ghola_sid:
                continue
            ext_sid = rev_map.get(ghola_sid)
            if ext_sid is None:
                continue
            target = per_tier.setdefault(tier or "episodic", {})
            cur = target.get(ext_sid, -1.0)
            if score > cur:
                target[ext_sid] = score

        # Convert each tier's score-dict to a rank-dict (1 = best).
        per_tier_rank: dict[str, dict[str, int]] = {}
        for tier, scores in per_tier.items():
            per_tier_rank[tier] = {
                sid: r for r, (sid, _) in enumerate(
                    sorted(scores.items(), key=lambda kv: kv[1], reverse=True),
                    start=1,
                )
            }

        # Stage 1 fuse: Reciprocal Rank Fusion across tiers. Score is
        # rank-based, so episodic's 0.47 cosine and semantic's 0.47
        # mneme-centroid don't compete on the same axis — each tier
        # contributes a vote based on where the document landed in
        # *its* ranking. k=60 is the literature default.
        rrf_k = int(os.environ.get("RRF_K", "60"))
        candidates_set: set[str] = set()
        for d in per_tier.values():
            candidates_set.update(d.keys())
        rrf_scores: dict[str, float] = {}
        for sid in candidates_set:
            rrf_scores[sid] = sum(
                1.0 / (rrf_k + per_tier_rank[t].get(sid, 10**6))
                for t in per_tier_rank
            )
        rrf_ranked = sorted(rrf_scores.items(), key=lambda kv: kv[1], reverse=True)

        # Ablation knob: if reranker disabled, return RRF top-K directly.
        # Compare this to the old max-score-across-tiers approach to
        # measure RRF's contribution in isolation.
        if self._reranker is None:
            return [
                {"session_id": s, "score": sc, "rank": i + 1}
                for i, (s, sc) in enumerate(rrf_ranked[:top_k])
            ]

        # Stage 2: cross-encoder rerank top RERANK_TOPK candidates.
        candidates = rrf_ranked[: self._rerank_topk]
        pairs: list[tuple[str, str]] = []
        owners: list[str] = []
        for ext_sid, _ in candidates:
            for chunk in self._session_chunks.get(ext_sid, []):
                pairs.append((query, chunk))
                owners.append(ext_sid)
        if not pairs:
            return [
                {"session_id": s, "score": sc, "rank": i + 1}
                for i, (s, sc) in enumerate(rrf_ranked[:top_k])
            ]

        rerank_raw = self._reranker.predict(pairs)
        rerank_per_session: dict[str, float] = {}
        for ext_sid, s in zip(owners, rerank_raw):
            f = float(s)
            cur = rerank_per_session.get(ext_sid, -1e9)
            if f > cur:
                rerank_per_session[ext_sid] = f

        # Stage 3: score fusion. Normalize each signal to [0,1] then
        # weighted sum. RERANK_WEIGHT=0 -> RRF only; =1.0 -> reranker
        # fully replaces RRF; default 0.5 = equal weight. The fusion
        # is the safety net for the bge-reranker-base failure mode where
        # all candidates score near zero and ordering becomes noise.
        rerank_weight = float(os.environ.get("RERANK_WEIGHT", "0.5"))
        rrf_weight = 1.0 - rerank_weight
        # Both denominators guard against an empty-or-zero distribution
        # so the divisions below are always safe.
        rrf_max = max((rrf_scores[s] for s, _ in candidates), default=1.0) or 1.0
        rerank_max = max(rerank_per_session.values(), default=1.0) or 1.0

        final: dict[str, float] = {}
        for sid, _ in candidates:
            rrf_norm = rrf_scores[sid] / rrf_max
            rerank_norm = rerank_per_session.get(sid, 0.0) / rerank_max
            final[sid] = rrf_weight * rrf_norm + rerank_weight * rerank_norm

        final_ranked = sorted(final.items(), key=lambda kv: kv[1], reverse=True)
        return [
            {"session_id": s, "score": sc, "rank": i + 1}
            for i, (s, sc) in enumerate(final_ranked[:top_k])
        ]

    # -- cleanup -----------------------------------------------------------

    def reset(self) -> None:
        # ghola exposes no bulk-purge endpoint today and chapterhouse's
        # /v1/episodic/forget operates on event_ids. The cheap path is to
        # forget the events for sessions this instance indexed.
        if self._client is None or not self._session_map:
            return
        # Without a query that returns event ids by session, the only honest
        # reset is "use a fresh user_id or fresh workspace next run." Log and
        # leave; document this in the bench plan.
        logger.warning(
            "ghola_v2 reset is a no-op — re-run with a fresh user_id or "
            "workspace_id (or truncate episodic.sessions/events manually). "
            "%d sessions tracked this run.",
            len(self._session_map),
        )
        self._session_map.clear()
        self._indexed_count = 0
        self._session_count = 0

    # -- stats -------------------------------------------------------------

    def stats(self) -> dict:
        return {
            "indexed": self._indexed_count,
            "sessions": self._session_count,
            "tracked_session_ids": len(self._session_map),
            "granularity": self._granularity,
            "url": self._url,
            "user_id": self._user_id,
        }

    # -- HTTP helpers ------------------------------------------------------

    def _post(self, path: str, body: dict[str, Any]) -> Any:
        if self._client is None:
            raise RuntimeError("Backend not set up; call setup() first")
        for attempt in range(5):
            resp = self._client.post(f"{self._url}{path}", json=body)
            if resp.status_code == 503 or resp.status_code == 502:
                wait = min(2 ** attempt * 0.5, 10.0)
                logger.warning("ghola %s -> %d, retry in %.1fs", path, resp.status_code, wait)
                time.sleep(wait)
                continue
            if resp.status_code >= 400:
                raise RuntimeError(
                    f"ghola {path} -> {resp.status_code}: {resp.text[:300]}"
                )
            try:
                return resp.json()
            except Exception:
                return resp.text
        raise RuntimeError(f"ghola {path} retried out")


def _check_degraded(out: Any, question_id: str | None, query: str) -> None:
    """Abort (or warn) when a recall response reports degraded stages.

    `out` is the parsed /v1/recall JSON. RecallResult carries `degraded`
    (a list of stage names) only when a tier degraded; the field is omitted
    when recall ran clean, so an absent/empty value is the happy path. A
    degraded recall during a scored run silently lowers R@k, hence the loud
    failure. GHOLA_ALLOW_DEGRADED=1 swaps the RuntimeError for a one-line
    stderr warning so the degradation can be inspected without aborting.
    """
    if not isinstance(out, dict):
        return
    degraded = out.get("degraded")
    if not degraded:
        return
    stages = ", ".join(str(s) for s in degraded)
    where = question_id or "<no question in scope>"
    detail = (
        f"recall degraded (stages: {stages}) on question {where} "
        f"query={query[:120]!r}"
    )
    if os.environ.get("GHOLA_ALLOW_DEGRADED") == "1":
        print(
            f"WARNING: {detail} -- continuing because GHOLA_ALLOW_DEGRADED=1; "
            f"scores from this run are NOT trustworthy",
            file=sys.stderr,
            flush=True,
        )
        return
    raise RuntimeError(
        f"{detail}. A degraded recall lowers benchmark scores with no "
        f"signal; set GHOLA_ALLOW_DEGRADED=1 to override (debug only)."
    )


def _raw_event(role: str, text: str, timestamp: str) -> dict:
    """Minimal raw_event payload — chapterhouse stores this verbatim."""
    return {
        "role": role,
        "content": text,
        "ts": timestamp,
        "src": "longmemeval-bench",
    }
