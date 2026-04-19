# Longitudinal Eval Design

Simplex v0.5 specification for evaluating pg_ghola as the memory substrate
of an AI agent across simulated time. This is the scoreboard that reflects
pg_ghola's actual purpose; LongMemEval-S R@5 is a floor check, not a
measurement of what the system is for.

## Framing

pg_ghola's seven cognitive primitives (Hebbian learning, contradiction
detection, sleep consolidation, temporal decay, confidence evolution,
thalamic gating, multi-pathway retrieval) are invisible on cold benchmarks.
LongMemEval indexes 19,195 sessions and immediately queries 500 questions;
it exercises exactly one primitive (semantic similarity via HNSW scan) and
ignores the other six. The system's raison d'etre -- memory that evolves,
learns, forgets, and differentiates -- cannot appear in that measurement.

The research literature on memory-as-architecture (Engram, L3, SCONE,
LongCat, RWKV, Byte Latent Transformer) converges on a shared insight:
memory is measured by its effect on the downstream task, not by a
standalone retrieval metric. Engram's published wins are +3-5pp on MMLU
and BBH, not +5pp on cold retrieval. L3 reports inference-time behavior
improvements. SCONE reports per-token perplexity gains. Memory, when it
works, makes the rest of the system better.

This eval applies that framing to pg_ghola. We measure the difference an
agent makes on a task suite when it has pg_ghola as its memory substrate
vs no memory, vs a static file, vs plain pgvector, vs pg_ghola at
different simulated ages. The cognitive primitives get to DO WORK over
time; we measure whether that work translates to better agent behavior.

## What this eval is NOT

- Not another retrieval benchmark. LongMemEval exists; we use it as a
  floor check (pg_ghola should not regress there), not as the scoreboard.
- Not a test of embedding quality. Phase 2 committed to isolated per-turn
  encoding and parked embedding experiments; those can resume later if
  longitudinal evidence indicates encoding is the bottleneck.
- Not a test of the embedding model. Qwen3-Embedding-0.6B is pinned.
  Future encoder swaps are a separate question.
- Not real-time. Running actual weeks of agent activity is too slow for
  iteration. Time gets compressed via interaction volume as a proxy.
- Not a production deployment. The agent is a controlled simulator, not a
  shipping product. Findings inform production design but are distinct.

---

## Core Question

Does an agent equipped with pg_ghola at "week N" (where "week N" means
BOTH N * volume_per_week interactions have been absorbed AND those
interactions are temporally spread across N simulated weeks in the
database via backdated timestamps, so the temporal primitives can
actually fire) outperform:

1. The same agent with no memory (context window only)
2. The same agent with a static CLAUDE.md-style memory file
3. The same agent with plain pgvector (same embedding model, no cognitive
   scoring)
4. The same agent with pg_ghola at "week 0" (cold, no usage history)

on a fixed, held-out probe task suite, measured at checkpoints
corresponding to weeks 0, 1, 2, 4, 8.

## Sacred / not sacred

- Sacred: seven cognitive primitives. The eval's job is to surface their
  effects, not to sidestep them.
- Sacred: pg_ghola as the memory substrate. We measure pg_ghola, not a
  replacement architecture.
- Not sacred: any specific task, metric, or simulator choice. Iterate.
- Not sacred: which agent model we use for simulation. Pick something
  cheap enough to run many interactions (e.g. a local Gemma, Qwen3-4B).

---

DATA: Probe
  id: string, stable identifier
  category: string, one of
    factual-recall         | fact stated once, queried later
    multi-hop-synthesis    | answer requires combining 2+ memories
    contradiction-detect   | a later memory contradicts an earlier one
    confidence-discriminate| some memories confirmed, some disputed
    staleness-detect       | outdated fact should have been superseded
    association-recall     | query matches via Hebbian, not direct similarity
    pattern-abstraction    | answer requires consolidation / generalization
    denial                 | correct answer is "I don't know / no memory"
  setup_interactions: list of Interaction, the events that must occur before
                      the probe is administered
  query: string, the question asked at probe time
  expected_answer: string or null, what correct recall looks like
  expected_primitive_signals: list, which cognitive primitives should
    observably contribute (e.g., "confidence<0.3", "hebbian_boost>0.1",
    "contradiction_flag=true")
  probe_week: int, earliest "week" at which this probe is valid
              (setup_interactions must have happened)

DATA: Interaction
  kind: string, one of
    remember    | store a memory (calls RememberWithTurns)
    recall      | perform a recall query (exercises retrieval pathways)
    feedback    | confirm or refute a prior recall (exercises confidence)
    contradict  | insert a memory that conflicts with an existing one
    idle_tick   | trigger consolidation worker (no new data, simulate time)
  payload: dict, kind-specific parameters
  timestamp: simulated time (monotonically increasing; "week" derives from
             a threshold function)

DATA: AgentHarness
  memory_backend: string, one of
    none | claude-md | pgvector | pg-ghola
  agent_model: string, identifier of the LLM driving the agent
                (e.g. Qwen3-4B, gemma-4-31B, claude-haiku)
  task_prompt: string, the system prompt framing the agent's role
  memory_access_mode: string, how the agent queries memory
    (e.g. tool-call, RAG-prepend, embedded-context)

DATA: CheckpointMeasurement
  week: int, which compressed-time checkpoint
  backend: string, memory_backend identifier
  probe_results: list of ProbeResult
  aggregate: dict, computed metrics (success_rate, tokens_per_task,
             contradiction_detection_rate, confidence_calibration_error, ...)

DATA: ProbeResult
  probe_id: string
  week: int
  backend: string
  predicted_answer: string
  correct: boolean, per a grader function
  primitive_signals: dict, observed values of the expected primitive signals
  tokens_used: int, prompt + completion tokens for this probe
  latency_ms: int
  notes: string

DATA: LongitudinalRun
  run_id: string, e.g. "longi-2026-04-18-qwen3-4b"
  agent_model: string
  backends: list of string, which backends participated
  checkpoints: list of int, e.g. [0, 1, 2, 4, 8]
  probe_set_id: string, references a pinned probe set for reproducibility
  interaction_volume_per_week: int, how many interactions = 1 "week"
  results: list of CheckpointMeasurement
  seed: int, RNG seed for reproducibility

---

CONSTRAINT: simulated_time_is_two_dimensional
  "Week N" is a pair: interaction VOLUME (N * volume_per_week events
  have occurred) AND wall-clock SPREAD (those events are temporally
  distributed across N simulated weeks in the database). Interaction
  volume alone is insufficient: if 500 interactions happen in a real
  30-second Python loop, every mneme has last_access within that
  30-second window, the Ebbinghaus decay returns ~1.0 for everything,
  and the consolidation worker thinks the entire workspace is "fresh."
  The temporal primitives are invisible unless the timestamps on the
  mnemes actually span the weeks we claim they span.

CONSTRAINT: simulated_time_via_backdated_timestamps
  The simulator maintains a `sim_now` clock (datetime). It advances
  per-interaction by a configured delta (default: volume_per_week
  interactions cover 7 real days, so delta = 7*24*60/volume_per_week
  minutes). Every mneme insertion uses `sim_now` as `created_at` and
  `last_access`, not `now()`. This produces the temporal spread the
  primitives expect.

  Implementation mechanism (simplest viable):
    - After Chapterhouse inserts a mneme at sim_now, the simulator
      issues `UPDATE mnemes SET created_at = $sim_now, last_access =
      $sim_now WHERE id = $parent_id` and the matching UPDATE on any
      sub_mnemes. (Chapterhouse itself does not need to know about
      simulated time; the override is purely simulator-side.)
    - For recall: the simulator can either (a) set `sim_now` by
      updating the clock dependency everything sees, or (b) just let
      recall use real `now()` since backdated last_access values
      produce correct `now() - last_access` deltas. (b) is simpler
      and sufficient; adopt it unless we find a reason to override
      recall's clock.

CONSTRAINT: consolidation_fires_synchronously_at_checkpoints
  The consolidation worker runs on wall-clock intervals (hourly decay,
  6-hourly archival, daily rebalance) in production. During eval runs
  it is PAUSED (worker_stats.state = 'paused') to prevent interference
  with the simulator's clock. At each checkpoint boundary (week 0 -> 1,
  1 -> 2, etc.), the simulator explicitly fires the consolidation
  routines in order, against backdated data, so their outputs
  (pruned associations, archived dormants, expired working mnemes,
  rebalanced clusters) reflect the simulated-time state.

  Required (or to-be-verified) SQL surface area:
    - `ghola.decay_associations(as_of timestamptz)` -- apply hourly
      decay as if `as_of` were the current time
    - `ghola.prune_weak_associations(threshold float8)` -- already
      time-independent
    - `ghola.archive_dormant(as_of timestamptz, max_age_days int,
      confidence_ceiling float8)` -- archive mnemes whose last_access
      is older than `as_of - max_age_days` with low confidence
    - `ghola.expire_working(as_of timestamptz)` -- clear working
      mnemes past their expires_at relative to `as_of`

  If these routines do not currently accept an `as_of` override, adding
  one is a precondition for the longitudinal eval. Background-worker
  callsites continue to pass `now()` by default, preserving production
  behavior.

CONSTRAINT: contradiction_worker_also_paused
  The contradiction worker's async behavior (polling the
  contradiction_queue) is similarly paused during eval. Contradiction
  detection is invoked synchronously when the simulator inserts a
  contradicting mneme (either by firing the contradiction-check
  routine directly, or by draining the queue in a controlled loop).
  This keeps contradiction signals deterministic across runs.

CONSTRAINT: probe_set_is_held_out
  The probe set is constructed in advance, pinned with an id, and NEVER
  used in the setup_interactions of any probe. The agent cannot learn
  the probes from setup. Probes are administered cold at each checkpoint,
  with NO memory of prior probe administrations bleeding into the
  memory store (probe interactions are read-only from pg_ghola's
  perspective, or performed in a sandbox workspace).

CONSTRAINT: backends_share_identical_interactions
  For a given run, ALL backends receive the identical sequence of
  setup_interactions and the identical probe administrations. This is
  the A/B control: the only difference between backends is what they
  do with the data they receive.

CONSTRAINT: agent_model_is_constant
  Within a single LongitudinalRun, the agent LLM is pinned by version
  and dtype. Cross-model comparisons are a separate run. The memory
  backend is the independent variable; the agent is held constant.

CONSTRAINT: primitive_signals_are_observable
  Every cognitive primitive must be instrumentable. pg_ghola already
  exposes confidence, hebbian_boost, activation in RecallResult;
  contradiction_flag and consolidation activity require added
  instrumentation (not yet implemented). This constraint forces us to
  add that instrumentation so we can actually measure primitive-level
  contributions, not just top-line success rate.

CONSTRAINT: simulated_time_is_reproducible
  Given (run_id, seed, interaction_volume_per_week, probe_set_id), the
  run MUST produce identical CheckpointMeasurement values modulo
  model-side non-determinism (which should be bounded by the
  variance budget from the samsara loop, >2pp).

CONSTRAINT: longmemeval_r5_is_a_floor_check_only
  Every longitudinal run MUST also produce a LongMemEval-S R@5
  measurement at week 0 as a sanity check. Regression vs the current
  iter 9 baseline (27.5%) or iter 16 baseline (post-Phase 2 number)
  halts the run pending investigation. This is NOT the primary metric;
  it is the floor.

---

FUNCTION: generate_probe_set(seed, category_mix) -> list of Probe

  RULES:
    - Produce a deterministic probe set for the given seed.
    - category_mix specifies counts per probe category
      (e.g. {factual-recall: 30, multi-hop-synthesis: 20,
             contradiction-detect: 15, confidence-discriminate: 10,
             staleness-detect: 10, association-recall: 10,
             pattern-abstraction: 5, denial: 10}).
    - Total probe count is sum of category_mix values (~110 in the
      default mix).
    - Each probe's setup_interactions are self-contained; later probes
      may share setup with earlier probes (to reduce total interaction
      volume) but the probe's correctness must not depend on another
      probe's administration.
    - Probes span at least 3 domains (e.g. technical, personal,
      operational) to avoid single-domain overfitting.
    - Setup interactions include distractors: the ratio of
      distractor-interactions to setup-interactions is at least 5:1,
      so the agent's memory fills with noise the way production does.

  DONE_WHEN:
    - Output can be serialized to JSONL (one Probe per line).
    - A hash of the serialized output is recorded as probe_set_id.
    - re-running with the same seed produces the same probe set (byte-for-byte).

  EXAMPLES:
    generate_probe_set(seed=42, category_mix={...default...})
      -> ~110 Probe objects, probe_set_id="probes-2026-04-v1-<sha256>"

  ERRORS:
    - category_mix contains unknown category -> fail with the bad name
    - category_mix total is 0 -> fail with "empty probe set"

---

FUNCTION: simulate_week(agent, memory_backend, interactions) -> WeekResult

  RULES:
    - Administer each Interaction to (agent + memory_backend) in order.
    - For kind=remember: agent calls RememberWithTurns (or the backend's
      equivalent); memory_backend persists it.
    - For kind=recall: agent calls the memory_backend's recall function,
      receives a list of candidates, formulates a response.
    - For kind=feedback: agent calls the backend's confirm/refute API
      (pg_ghola has confirm_recall; for baselines, this may be a no-op).
    - For kind=contradict: insert a memory whose content conflicts with
      an existing memory; record the injected contradiction for later
      detection measurement.
    - For kind=idle_tick: advance simulated time by the tick's duration;
      pg_ghola's consolidation worker is allowed to run; for baselines,
      no-op.
    - Record per-interaction metrics: tokens, latency, primitive signals.

  DONE_WHEN:
    - Every interaction has been administered exactly once.
    - Per-interaction metrics are recorded in the WeekResult.
    - For pg_ghola: co_activation_queue, contradiction_queue, and
      gating_queue have drained (or their state is recorded for
      diagnostic purposes).

  EXAMPLES:
    simulate_week(agent=qwen3-4b, memory_backend=pg-ghola,
                  interactions=[...500...])
      -> WeekResult with 500 per-interaction metrics

  ERRORS:
    - Interaction kind unsupported by backend -> record as a skip,
      continue (e.g., contradiction on claude-md is a no-op)
    - DB / agent error mid-week -> record, halt, surface for operator

  NOT_ALLOWED:
    - Skip interactions silently
    - Read probe_set during week simulation (probes are strictly held out)

---

FUNCTION: administer_probes(agent, memory_backend, probes, week) -> list of ProbeResult

  RULES:
    - For each probe valid at this week (probe.probe_week <= week):
      - Administer the probe query via the agent's standard task flow.
      - Agent may query memory_backend; record recall candidates and
        primitive signals.
      - Grade the agent's response with a grader function
        (exact-match for factual-recall; LLM-judge or rubric for
        multi-hop-synthesis; boolean flag for contradiction-detect; etc.)
      - Record ProbeResult.
    - Probe administration MUST be side-effect-free with respect to the
      memory store: the probe query is read-only from pg_ghola's
      perspective. Co-activation events from probes should NOT strengthen
      associations. Implementation: use a dedicated read-only workspace
      or a recall mode that skips the co_activation_queue enqueue.

  DONE_WHEN:
    - Every valid probe has a ProbeResult.
    - Grader outputs are recorded with confidence / rubric detail.
    - The memory store state after probe administration matches state
      before probe administration (modulo access_count, which may be
      reset or isolated; implementation choice).

  EXAMPLES:
    administer_probes(agent, pg-ghola, probes[:110], week=4)
      -> 110 ProbeResult objects

  ERRORS:
    - Grader fails on a probe -> record as error, continue
    - Agent timeout on a probe -> record with notes, continue

  NOT_ALLOWED:
    - Mutate the memory store via probes (no co_activation enqueue,
      no access_count bump)
    - Let probe setup bleed into the next week's interactions

---

FUNCTION: run_longitudinal(config) -> LongitudinalRun

  RULES:
    - Initialize agent (pinned model, dtype, system prompt).
    - For each backend in config.backends:
      - Initialize backend in a clean state (workspace empty for
        pg-ghola / pgvector; empty CLAUDE.md for claude-md; no memory
        for none).
      - For week in [0, 1, 2, 4, 8]:
        - If week > 0: simulate the interactions scheduled for weeks
          (prev_week, week].
        - administer_probes(agent, backend, probes, week)
        - Record CheckpointMeasurement.
    - After all backends complete, record LongMemEval-S R@5 for
      pg-ghola at week 0 as a floor check.
    - Serialize the LongitudinalRun to disk.

  DONE_WHEN:
    - LongitudinalRun file exists with CheckpointMeasurement for every
      (backend, week) pair.
    - Floor check value is recorded.
    - probe_set_id recorded for future reproducibility.
    - Per-primitive signal distributions summarized.

  EXAMPLES:
    run_longitudinal(config)
      -> results/longi/run-2026-04-18-qwen3-4b.json
      -> includes pg-ghola@week0/1/2/4/8, pgvector@week0/1/2/4/8,
         claude-md@week0/1/2/4/8, none@week0/1/2/4/8
      -> floor check: 27.5% R@5 (or whatever iter 16 established)

  ERRORS:
    - Floor check regresses by >variance_budget -> halt, flag
    - Agent model unavailable -> fail at initialization
    - Backend initialization fails -> halt, surface

---

## Task Suite (probe categories, concrete scenarios)

### factual-recall
Single-turn fact stated in setup, queried later.
- "My Comcast account PIN is 8237."
- "The server's LAN IP is 10.0.1.42."
- "Dr. Evans is my grant co-PI."

### multi-hop-synthesis
Answer requires combining two or more memories.
- Memory 1: "Priya is the storage team lead."
- Memory 2: "The storage team is migrating to NVMe drives."
- Probe: "Who is leading the NVMe migration?"

### contradiction-detect
A setup interaction contradicts an earlier one; the agent should flag or
update rather than stating both.
- Memory 1 (week 1): "My dog's name is Daisy."
- Memory 2 (week 3): "My dog's name is Riley."
- Probe (week 4): "What's my dog's name?" -> expect "Riley" with a
  note that Daisy was previously stated.

### confidence-discriminate
Multiple memories about the same topic; some confirmed repeatedly, some
contradicted once. Confidence-weighted retrieval should prefer the
well-confirmed memory.
- High-confidence: "The prod DB is Postgres 18" (confirmed 5x)
- Low-confidence: "The prod DB is Postgres 17" (stated once, contradicted
  by the above)
- Probe: "Which Postgres version is prod running?" -> 18

### staleness-detect
A fact is stated, later superseded, and the stale version should no longer
dominate.
- Week 1: "Our embedding model is sentence-transformers/all-MiniLM-L6-v2."
- Week 3: "We switched to Qwen3-Embedding-0.6B."
- Probe week 4: "Which embedding model are we using?" -> Qwen3, NOT MiniLM

### association-recall
Probe query that doesn't directly match a stored memory's content but
co-occurred with the answer in prior interactions. Only Hebbian
associations can surface this.
- Week 1: Recall about "K8s pod scheduling" returns memory A.
- Week 1: Recall about "pod scheduling" returns memory A and memory B
  (co-activated).
- Weeks 2-3: Repeated co-activation of A and B via recall.
- Probe week 4: A specific query about "pod scheduling" that doesn't
  textually match B, but which B is now Hebbian-associated with ->
  expect B in the top results via hebbian_boost > 0.

### pattern-abstraction
Many specific memories; probe asks the general pattern. Consolidation
should have produced a summary-tier memory.
- Setup: 10 specific bug reports, all rooted in "forgot to await the
  async call."
- Probe week 4: "What's a recurring bug pattern in our codebase?"
  -> expect the general pattern surfaced, not one specific bug.

### denial
No setup relevant to the probe exists. Correct behavior is to refuse to
fabricate.
- Probe: "What did I say about Quantum flux capacitors on Tuesday?"
- Expected: "No memory of that topic."
- Failure mode: backend returns irrelevant high-cosine matches, agent
  confabulates.

---

## Metrics

For each (backend, week) cell, compute:

**Success rate (primary)**
  success_rate = n_correct_probes / n_probes
  Weighted by probe category (e.g. contradiction-detect correct requires
  BOTH the right answer AND the contradiction-flag signal).

**Category-specific hit rate**
  Per-category success rate. Identifies which primitives are / aren't
  working.

**Primitive signal prevalence**
  For each probe where the expected_primitive_signals fired:
    - confidence range observed
    - hebbian_boost > threshold frequency
    - contradiction_flag true positive / false positive rate
    - consolidation-driven tier changes observed

**Token efficiency**
  mean tokens_used per probe. pg_ghola should reduce token cost over
  time as consolidation prunes clutter from the retrieval pool.

**Latency**
  p50, p95 recall latency. Tracks whether index growth / HNSW scans
  degrade with memory size.

**Curve shape (the headline)**
  For each metric, the trajectory over week 0 -> 1 -> 2 -> 4 -> 8 for
  each backend. pg_ghola's hypothesis: success_rate starts comparable
  to pgvector at week 0 and DIVERGES UPWARD over weeks; baselines stay
  flat or degrade.

**Floor check**
  LongMemEval-S R@5 at week 0, recorded alongside results. Not the
  primary signal but a regression canary.

---

## Infrastructure

### What needs to exist (greenfield for this eval)

1. **Agent simulator** -- a small driver that takes an agent model, a
   memory backend, and an Interaction stream, and runs them. Prompts
   framed as "you are an assistant to a human user; use your memory to
   answer their questions." Reads from Interaction sequence, writes to
   memory, administers probes.

2. **Task suite library** -- code that constructs the probe set + setup
   interactions from the templates described in the Task Suite section.
   Seeded RNG for reproducibility. Distractor generator (filler
   interactions to dilute the memory store).

3. **Backend adapters** -- implementations of a common `MemoryBackend`
   interface for:
   - `none` (returns nothing)
   - `claude-md` (reads/writes a markdown file)
   - `pgvector` (same embedding model, no cognitive scoring, just cosine)
   - `pg-ghola` (full recall function)

4. **Probe grader** -- exact-match for factual-recall; rubric-based
   grader (possibly LLM-judge) for multi-hop-synthesis and pattern-
   abstraction; boolean flag for contradiction-detect / staleness-detect.

5. **Simulated time controller (TimeSimulator)** -- the central
   mechanism that gives the primitives room to fire. It owns a
   `sim_now` datetime, advances it per-interaction by a configured
   delta, and provides three services to the simulator:
     (a) `insert_at(mneme_id, sim_now)` -- UPDATE mnemes (and any
         sub_mnemes) to set created_at / last_access to sim_now after
         Chapterhouse's normal insert path has run. This is the
         backdating mechanism.
     (b) `advance_to(target_sim_now)` -- move the clock forward,
         triggering checkpoint-boundary events.
     (c) `fire_consolidation(as_of, kinds=[decay, prune, archive,
         expire])` -- synchronously invoke the consolidation routines
         with as_of = sim_now, in place of the paused background
         worker.
   Also pauses/resumes the consolidation, contradiction, and gating
   workers around eval runs (set worker_stats.state = 'paused').
   Without this utility the temporal primitives are effectively
   unmeasurable, regardless of how many interactions we run.

6. **Primitive signal instrumentation** -- add observability for:
   - contradiction_flag in recall results
   - consolidation worker events (tier changes, prune counts, archive
     counts per interval)
   - gating-driven filter effectiveness

7. **Results collector / dashboard** -- a JSON Lines results file per
   run, plus a simple Python script that produces the curves. Should
   feed into the blog's EvalPage in the long term.

### What can be reused

- pg_ghola's existing recall function (already exposes confidence,
  activation, hebbian_boost)
- Chapterhouse's RememberWithTurns (once Phase 2 lands)
- longmemeval-ghola's benchmark runner (for the floor check)
- Existing LLM inference infrastructure (vLLM, sentence-transformers)

### What we explicitly DON'T need to build

- A new database. pg_ghola runs on the existing CNPG Postgres.
- A new embedding model. Qwen3-Embedding-0.6B is pinned.
- A new orchestrator. Plain Python async with tenacity for retries.
- A new LLM. Use what's already running on the NUC (Gemma, Qwen3).

---

## Execution plan (ordered by dependency)

1. **Expose consolidation routines with as_of override**. Currently
   consolidation runs from the background worker against `now()`.
   Add SQL-callable versions of decay_associations, archive_dormant,
   expire_working that accept an optional `as_of timestamptz`. Add
   worker pause/resume via worker_stats.state. Same for the
   contradiction worker's check routine. This is the precondition for
   the TimeSimulator; without it we can't simulate time faithfully.

2. **Instrument primitives for observability**. Emit per-recall and
   per-consolidation events capturing contradiction flags, tier
   changes, prune counts, gating filter effectiveness. Touches
   gating_worker.rs, consolidation_worker.rs, contradiction.rs,
   recall.rs.

3. **Build the TimeSimulator utility**. Python, sits alongside the
   agent simulator. Owns sim_now, backdates inserts, fires
   consolidation synchronously, pauses/resumes workers. Unit-testable
   against a scratch workspace before any agent is wired in.

4. **Build the agent simulator skeleton**. Backend-agnostic; passes
   a canned Interaction sequence through and records metrics. Uses
   TimeSimulator for any time-dependent operations. Start with the
   `none` backend to exercise the loop end-to-end.

5. **Implement the 4 backend adapters**. `claude-md` is trivial,
   `pgvector` is almost trivial (single SQL), `pg-ghola` wraps the
   existing recall, `none` is a stub.

6. **Construct the seed task suite**. Small (maybe 30 probes across
   5 categories) for the first iteration. Grow as failures surface.

7. **First longitudinal run**. pg_ghola vs pgvector vs none, at weeks
   0 and 4. Small probe set. Checks: does the machinery work? does
   pg_ghola show ANY divergence from baselines over simulated time?
   Verify: with properly backdated timestamps, ACT-R activation and
   Ebbinghaus retention values actually span the expected range
   (not all ~1.0).

8. **Analyze, iterate**. If pg_ghola doesn't diverge by week 4 on any
   primitive-tied category, that's the signal that the primitives
   aren't doing the work they're supposed to. Investigation follows.

9. **Grow the probe set**. Add multi-hop-synthesis, pattern-abstraction,
   association-recall. Add denial probes. Add staleness-detect.

10. **Add claude-md backend**. Gives us a third baseline point.

11. **Full longitudinal runs at weeks 0, 1, 2, 4, 8**. Publishable
    curves.

12. **Wire results into the blog's EvalPage**. The "Measuring Memory"
    page reframes around the longitudinal curves; cold R@5 becomes a
    floor check, not the headline.

---

## Out of Scope

- Real-time longitudinal runs (real weeks of real usage). Simulated time
  only for iteration speed.
- Cross-model comparisons (does pg_ghola help Claude more than Gemma?).
  Separate study.
- Human-in-the-loop probes. The agent simulator drives everything;
  humans construct the probe templates but do not participate at run
  time.
- Production deployment of the simulator. It's an eval tool, not a
  product.
- Direct comparisons to Mem0 / Zep / Letta / Hindsight / MemPalace.
  They each have their own benchmarks; replicating them all is a
  research project of its own. A future iteration may add their
  backends to the adapter list.
- Fine-grained confidence calibration analysis (reliability diagrams,
  etc.). Valuable but deferred; start with coarse success rates.

---

## Success criteria for the eval itself

The eval is "working" when:

1. A full run completes end-to-end without crashes across all 4
   backends at all 5 checkpoints.
2. pg_ghola@week0 is within variance budget of pgvector@week0 on
   factual-recall (they both use the same embedding; they should tie).
3. pg_ghola shows a DIFFERENT curve shape from pgvector over weeks;
   whether it's better or worse is a research question the eval
   surfaces, but a flat curve would indicate the primitives aren't
   doing measurable work.
4. At least three primitive signals (confidence, hebbian_boost,
   contradiction_flag) vary meaningfully between pg-ghola and
   pgvector across categories.
5. The floor check (LongMemEval R@5 @ week 0) matches the committed
   iter 16 baseline within variance budget.

Failure to hit (2) means there's a measurement bug. Failure to hit (3)
means pg_ghola's primitives are not doing the work we claim they do,
which is the most important question this eval can answer -- and the
reason it's worth building.

---

## References and framing

The decision to measure memory by downstream effect rather than cold
retrieval draws on recent memory-as-architecture literature:

- Engram / Conditional Memory via Scalable Lookup (arxiv 2601.07372):
  memory as a sparsity axis, measured by downstream task performance
  (MMLU +3.4, BBH +5.0, etc.), not by standalone retrieval accuracy.
- L3 / Large Lookup Layers (arxiv 2601.21461): static-routed embedding
  banks evaluated by inference-time behavior.
- Scaling Embeddings Outperforms Scaling Experts (arxiv 2601.21204,
  LongCat-Flash-Lite): embedding scaling as an orthogonal sparsity
  frontier with its own Pareto curve.
- SCONE / Scaling Embedding Layers (arxiv 2502.01637): precomputed
  embeddings as learnable model parameters, measured by per-token
  quality.
- Over-Tokenized Transformer (arxiv 2501.16975): vocabulary scaling
  has a distinct log-linear regime from compute scaling.
- Byte Latent Transformer (arxiv 2412.09871): dynamic patches adapt
  memory structure to content.
- RWKV: recurrent state as native memory primitive, evaluated by
  inference efficiency and downstream task performance.
- EmbeddingGemma architecture: on-device embedding model tuned for
  the runtime where the agent lives.

Common thesis: memory is a first-class architectural primitive whose
success is measured by its effect on the downstream task, not by
retrieval accuracy in isolation. pg_ghola should be evaluated the same
way.
