# Memory Policy Layer — pg_ghola + MemFactory Integration

## The Gap

pg_ghola provides the **storage and scoring primitives**: ACT-R activation, Hebbian
associations, Bayesian confidence, Ebbinghaus decay. These are the physics of memory.

What's missing is the **policy**: when to extract a memory from conversation, what to
store, when to retrieve, when to forget. Currently this is hardcoded into Chapterhouse's
MCP tools (the agent calls `remember` or `recall` explicitly), which means:

- The agent decides when to remember based on prompt engineering, not learned behavior
- No reward signal feeds back into memory quality
- Extraction is all-or-nothing (store the whole text vs nothing)
- Retrieval is fire-once (no iterative refinement)
- No mechanism to learn that some memories are more useful than others

## MemFactory's Architecture

MemFactory (arxiv 2603.29493) provides exactly the missing layer:

```
Environment Input
      │
      ▼
  Extractor ──── "What should I remember from this?"
      │           (learned policy, not hardcoded rules)
      ▼
  Updater ────── "Merge, overwrite, or discard vs existing memory?"
      │           (handles contradictions, deduplication)
      ▼
  Retriever ──── "What's relevant to this task right now?"
      │           (learned query formulation, multi-hop)
      ▼
Environment Output
```

Each module can be:
- **Rule-based** (naive extractors, cosine retrievers)
- **Learnable** (RL-trained via GRPO with environment rewards)

The key innovation: GRPO trains the memory policies using **task completion as reward**.
Good memory operations → better task outcomes → policy improves.

## Integration Design

### pg_ghola as the Storage Backend

Replace MemFactory's default backends (Milvus, Neo4j) with pg_ghola:

| MemFactory Component | Default Backend | pg_ghola Replacement |
|---------------------|-----------------|---------------------|
| Vector storage | Milvus | `pg_ghola.mnemes` (pgvector HNSW) |
| Graph storage | Neo4j | `pg_ghola.associations` (typed edges) |
| Retrieval | Cosine similarity | `pg_ghola.recall()` (full cognitive pipeline) |
| Update | Overwrite/append | `pg_ghola.bayesian_update()` + `mark_supersedes()` |
| Scoring | None (static) | ACT-R + Ebbinghaus + Hebbian + Bayesian |

### New Module: CognitiveRetriever

A MemFactory retriever that wraps `pg_ghola.recall()`:

```python
@MODULE_REGISTRY.register("cognitive_retriever")
class CognitiveRetriever(BaseModule):
    """
    Retriever backed by pg_ghola's cognitive recall pipeline.

    Unlike naive cosine retrieval, this considers:
    - Temporal recency (ACT-R activation)
    - Usage patterns (Hebbian co-activation)
    - Reliability (Bayesian confidence)
    - Spacing effects (Ebbinghaus decay)
    """
    def retrieve(self, query, context, memory_state):
        results = pg_ghola_recall(
            workspace_id=context.workspace_id,
            query_text=query,
            query_embedding=self.embed(query),
            limit=self.top_k,
            min_confidence=self.confidence_threshold,
        )
        # Feed retrieved results back as co-activation signal
        pg_ghola_record_co_activation(
            workspace_id=context.workspace_id,
            mneme_ids=[r.id for r in results],
            scores=[r.score for r in results],
        )
        return results
```

### New Module: CognitiveUpdater

Handles the merge/overwrite/discard decision using pg_ghola primitives:

```python
@MODULE_REGISTRY.register("cognitive_updater")
class CognitiveUpdater(BaseModule):
    """
    Updater that uses pg_ghola's contradiction detection and
    Bayesian confidence to decide how to integrate new memories.
    """
    def update(self, new_memory, existing_memories, context):
        # Check for contradictions
        contradictions = pg_ghola_check_contradictions(new_memory.id)

        if contradictions:
            for c in contradictions:
                if c.similarity > 0.95:
                    # Near-duplicate → supersede
                    pg_ghola_mark_supersedes(new_memory.id, c.mneme_b)
                else:
                    # Genuine contradiction → flag for resolution
                    pg_ghola_flag_contradictions(new_memory.id)

        # Confirm retrieval quality from reward signal
        if context.reward > 0:
            pg_ghola_confirm_recall(context.retrieved_mneme_ids)
```

### Reward Function: Memory-Aware Task Completion

The RL reward should capture whether memory helped:

```python
@ENV_REGISTRY.register("cognitive_memory_env")
class CognitiveMemoryEnv(MemoryBankEnv):
    def compute_reward(self, predictions, ground_truths, ...):
        # Base reward: task accuracy
        accuracy_reward = compute_accuracy(predictions, ground_truths)

        # Memory efficiency: did we avoid recomputation?
        # Higher reward if answer used retrieved facts vs hallucinated
        grounding_reward = compute_grounding(predictions, retrieved_memories)

        # Token efficiency: fewer tokens for same accuracy = better
        token_reward = compute_token_efficiency(predictions)

        # Cognitive load: penalize unnecessary retrievals
        retrieval_cost = -0.01 * num_retrievals

        return accuracy_reward + 0.3 * grounding_reward + 0.1 * token_reward + retrieval_cost
```

## Aletheia Pattern: Write → Verify → Revise

Aletheia (arxiv 2602.10177) demonstrates a complementary pattern:
iterative generation with verification and revision using tool use.

For memory-augmented agents, this maps to:

```
1. RETRIEVE relevant memories
2. GENERATE response using memories as context
3. VERIFY response against stored facts (contradiction detection)
4. REVISE if contradictions found (update memories, regenerate)
5. CONFIRM useful memories (Bayesian evidence → confidence boost)
```

pg_ghola provides the primitives for steps 1, 3, 4, and 5.
The policy layer (MemFactory) learns when and how aggressively to do each.

## Implementation Phases

### Phase 1: Benchmark Current State (now)
- Run the benchmark suite we just built
- Establish baseline: no_memory vs cognitive recall
- Measure token efficiency, accuracy, grounding

### Phase 2: Active Confidence Loop
- Instrument Chapterhouse to call `confirm_recall` when memories are used
- Implement `reject_recall` for hallucinated or wrong memories
- Watch confidence scores diverge from the 0.5 default
- Re-run benchmark, compare

### Phase 3: MemFactory Integration
- Build pg_ghola storage adapter for MemFactory
- Implement CognitiveRetriever and CognitiveUpdater modules
- Train extraction/retrieval policies on our actual usage data
- Compare learned policies vs rule-based (current)

### Phase 4: Self-Improving Memory
- RL training loop: agent tasks → memory operations → task rewards → policy update
- pg_ghola's Hebbian associations provide a natural exploration signal
- Confidence scores become the stored value estimate
- The system learns what's worth remembering

## Hardware Considerations

MemFactory recommends A800 80G for GRPO training. We have an RTX PRO 6000 (96GB VRAM)
which is more than sufficient. Training smaller models (Qwen3-1.7B or 4B) for the
memory policy while using Opus/Sonnet for the actual task reasoning could work well:

- **Policy model** (local, 4B): decides when/what to extract, retrieve, update
- **Reasoning model** (API, Opus): does the actual thinking with retrieved context
- **Scoring engine** (pg_ghola): provides the cognitive primitives

This separates the fast/cheap policy decisions from the expensive reasoning.

## Key Insight

pg_ghola makes memory *behave* like biological memory (decay, strengthening, association).
MemFactory makes an agent *learn to use* memory effectively. Together they close the loop:

```
Agent acts → generates memories → pg_ghola scores/ranks them → agent retrieves →
task outcome → RL reward → policy improves → agent makes better memory decisions
```

The database isn't just a store. It's a cognitive substrate that the policy layer
learns to exploit.
```
