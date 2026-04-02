# pg_ghola Cognitive Memory Benchmark

## What We're Testing

The core hypothesis: **retrieving stored insights beats recomputing them from scratch.**

This isn't a ranking quality benchmark (though we measure that too). It's a
computational efficiency test: can an LLM leverage stored memories to answer
questions faster, cheaper, and more accurately than reasoning from raw context?

## Test Design

### Three Conditions

1. **No Memory (baseline)** — LLM answers from system prompt + raw question only.
   No access to any stored facts. Must reason from scratch.

2. **Naive Retrieval** — Pure vector similarity search (cosine distance, no
   cognitive scoring). What you'd get from a plain pgvector query.

3. **Cognitive Recall** — Full pg_ghola pipeline: vector + FTS + ACT-R temporal
   decay + Hebbian association boost + Bayesian confidence weighting.

### What We Measure

| Metric | How | Why |
|--------|-----|-----|
| **Answer accuracy** | Human-judged correctness (1-5 scale) | Does memory help get the right answer? |
| **Factual grounding** | Count of verifiable claims sourced from memory vs hallucinated | Does memory reduce hallucination? |
| **Token efficiency** | Input + output tokens per answer | Does memory reduce compute cost? |
| **Latency** | Wall-clock time to answer | Is recall faster than re-derivation? |
| **Reasoning depth** | Does the answer build on prior analysis or re-derive? | Is the system accumulating knowledge? |
| **Recall quality** | nDCG, precision@k, rank correlation | Are the right memories surfacing? |

### Test Queries

Queries are designed around knowledge that actually exists in the memory store
(37 memories from real Nyx usage). Categories:

**Infrastructure Troubleshooting** (experiential memories should help)
- "DNS is broken in the cluster again. What should I check?"
- "Prowlarr indexers are showing as unavailable. What's the fix?"
- "How do I deploy a new pg_recall version to the CNPG cluster?"

**System Configuration** (factual memories should help)
- "What's the NUC's IP address and what's running on it?"
- "How is the media server stack organized?"
- "What are the Tailscale IPs for my devices?"

**Project Context** (accumulated knowledge should help)
- "What cognitive models does pg_recall implement?"
- "What was in the latest Switch intelligence brief?"
- "How does the multi-agent setup work?"

**Compositional Reasoning** (multiple memories must compose)
- "If DNS breaks again, what's the full recovery sequence including Prowlarr?"
- "Design a monitoring alert for the failure modes we've seen before."
- "What would need to change to deploy pg_ghola 0.0.1 on the NUC?"

## Running

```bash
# Snapshot current state (before any changes)
python3 tests/benchmark/run_benchmark.py --snapshot baseline

# After changes, run comparison
python3 tests/benchmark/run_benchmark.py --snapshot after-change --compare baseline
```

## File Structure

```
tests/benchmark/
├── README.md              # This file
├── queries.json           # Test queries with expected knowledge
├── run_benchmark.py       # Main benchmark runner
├── evaluate.py            # Scoring and comparison
└── snapshots/             # Stored results per run
```
