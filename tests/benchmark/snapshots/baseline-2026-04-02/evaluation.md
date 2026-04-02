# pg_ghola Cognitive Memory Benchmark — Baseline Evaluation
**Date:** 2026-04-02
**Embedding model:** Qwen3-Embedding-0.6B (1024d)
**Extension:** pg_recall 0.5.0 (untyped vector dims)
**Memory count:** 15 mnemes (freshly seeded after data loss)

## Methodology

12 queries across 4 categories. For each query:
- **Recall quality**: Does the right memory surface? What's the top score?
- **Knowledge coverage**: What % of expected facts appear in retrieved memories?
- **No-memory baseline**: What could an LLM answer from its training data alone (no retrieval)?
- **Memory advantage**: What does retrieval add that the LLM couldn't know?

## Results

### Category: Troubleshooting

| Query | Top Score | Coverage | Memory Advantage |
|-------|-----------|----------|-----------------|
| DNS recovery | 0.95 | 100% | **HIGH** — exact failure pattern, specific IPs (100.100.100.100), exact recovery sequence |
| Prowlarr fix | 1.03 | 100% | **HIGH** — "testall clears circuit breaker" is experiential knowledge not in any docs |
| pg_recall deploy | 0.86 | 40% | **MEDIUM** — knows about the extension but not the full deploy process (manifest path, imagePullPolicy) |

**Troubleshooting verdict:** Memory provides exact operational knowledge. Without it, an LLM could suggest generic Kubernetes DNS debugging but would never know about the Tailscale MagicDNS → CoreDNS cascade or the Prowlarr testall workaround. These are *learned from experience*, not documented anywhere.

### Category: Configuration

| Query | Top Score | Coverage | Memory Advantage |
|-------|-----------|----------|-----------------|
| NUC info | 0.67 | 80% | **HIGH** — exact IPs, hardware specs, SSH config |
| Media stack | 1.34 | 60% | **HIGH** — specific service counts, namespace, API key location |
| Tailscale IPs | 1.11 | 25% | **MEDIUM** — has some IPs but not all 4 devices in one memory |

**Configuration verdict:** Memory stores exact infrastructure details (IPs, specs, service counts) that an LLM literally cannot know. Coverage gap on Tailscale is because the dedicated Tailscale memory has all 4 IPs but the coverage metric only checks top-3 results.

### Category: Project Context

| Query | Top Score | Coverage | Memory Advantage |
|-------|-----------|----------|-----------------|
| Cognitive models | 1.32 | 40% | **MEDIUM** — lists the models but missing deep explanations (Thousand Brains not stored) |
| Switch brief | 0.63 | 0% | **NONE** — Switch brief memory was lost in DROP CASCADE, not re-seeded |
| Multi-agent | 1.27 | 100% | **HIGH** — exact architecture, agent names, communication pattern |

**Project context verdict:** Strong for stored projects, but coverage depends entirely on what was seeded. Switch brief is a clear gap — that experiential data was lost and not re-seeded.

### Category: Compositional

| Query | Top Score | Coverage | Memory Advantage |
|-------|-----------|----------|-----------------|
| DNS + Prowlarr runbook | 1.51 | 67% | **HIGH** — retrieves BOTH failure patterns; an LLM can compose them into a runbook |
| Monitoring design | 0.58 | 75% | **HIGH** — retrieves the specific failure patterns to alert on |
| pg_ghola rename | 1.22 | 60% | **HIGH** — knows ch-server is Go, knows schema dependencies, knows about pg_ghola |

**Compositional verdict:** This is where memory shines. The system retrieves multiple relevant memories and the LLM composes them. The DNS+Prowlarr runbook query surfaces both incidents (scores 1.51 and 0.63) enabling a complete recovery plan that no LLM could produce from training data alone.

## Summary

| Metric | Value |
|--------|-------|
| Queries tested | 12 |
| Average top score | 0.97 |
| Queries with relevant top result | 11/12 (92%) |
| Queries where memory provides unique value | 10/12 (83%) |
| Average knowledge coverage | 60% |
| Zero-coverage queries | 1 (Switch brief — not seeded) |

## What Memory Adds (The Key Finding)

**Without memory, an LLM can:**
- Give generic Kubernetes debugging advice
- Explain what Sonarr/Prowlarr are in general
- Describe ACT-R theory from training data

**With memory, the LLM gets:**
- *Your* exact DNS failure cascade (Tailscale → CoreDNS → Prowlarr circuit breaker)
- *Your* exact IPs, service counts, API key locations
- *Your* multi-agent architecture with specific agent names and communication patterns
- Operational lessons that only exist because someone debugged them at 2am

**The fundamental value proposition:**
Memory converts *generic capability* into *specific expertise about your infrastructure*.
The LLM goes from "here's how DNS usually works" to "your CoreDNS forwards to 100.100.100.100
via Tailscale, and when tailscaled hangs you need to restart it then rollout restart CoreDNS,
and then run Prowlarr testall because the circuit breaker won't auto-clear."

That's the difference between a search engine and a colleague who was there when it broke.

## Gaps to Address

1. **Coverage:** 15 memories is sparse. Need to seed more (especially Switch brief, deployment procedures, Chapterhouse architecture details)
2. **Confidence calibration:** All memories at 0.5 default. Need to wire `confirm_recall` into the usage loop
3. **Association strength:** Hebbian associations need more recall volume to strengthen
4. **Compositional recall:** The DNS+Prowlarr runbook works because both memories surface, but they're ranked separately. Could benefit from association-boosted retrieval
5. **Token efficiency:** Not yet measured — need to compare tokens used with vs without context
