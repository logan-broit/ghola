# Blog Redesign: Building Memory That Thinks

Simplex v0.5 specification for restructuring the pg_ghola blog preview into a cohesive portfolio piece and development notebook.

## Context

The current blog preview at `~/.openclaw/workspace/projects/blog/preview/` has three disconnected pages (pg_ghola explainer, GRPO training, benchmark eval) with duplicated design systems, stale hardcoded data, and no narrative coherence. The benchmark eval page grew organically through 16 samsara iterations, accumulating InfoBox after InfoBox.

The redesign consolidates everything into a hub-and-spoke structure telling one story: building a memory system for AI agents that works like a brain, not a filing cabinet.

## Audience

Dual purpose: portfolio piece for external viewers + personal development notebook. Clean narrative spine for browsing, enough depth for working reference.

## Architecture

Hub-and-spoke. Hub page tells the story in 30 seconds. Spoke pages go deep on individual topics. Visitors can browse the pitch or click through to engineering details.

---

DATA: SpokePage
  route: string
  title: string
  content: string, summary of what the page covers

DATA: LandscapeTier
  name: string
  description: string
  examples: list of systems with their approach

DATA: Primitive
  name: string
  neuroscience_analog: string, one sentence
  computation: string, one sentence
  why_it_matters: string, one sentence

DATA: CapabilityRow
  system: string
  strengthens_with_use: boolean
  detects_contradictions: boolean
  forgets_gracefully: boolean
  reorganizes_offline: boolean
  confidence_evolution: boolean

---

## Pages

FUNCTION: hub_page(/) -> rendered page

  RULES:
    - Hero section: title "Building Memory That Thinks", subtitle about filing cabinets vs brains
    - Problem cards (3): no persistence, static storage, no lifecycle
    - What pg_ghola does: one sentence per primitive, visual showing this isn't store-and-retrieve
    - Navigation cards: one per spoke page with title + one-line hook
    - Key numbers: real stats from the live system (mneme count, workers, pathways)
    - No benchmark tables, no code snippets, no war stories on this page
    - Links to spoke pages for all depth

  DONE_WHEN:
    - A visitor understands the pitch in 30 seconds
    - Every spoke page is reachable from the hub
    - No duplicated content from spoke pages

---

FUNCTION: landscape_page(/landscape) -> rendered page

  Title: "How AI Agents Remember"

  RULES:
    - Opens with the blank-slate problem
    - Surveys the landscape in four tiers:
      - Tier 1 (No memory): ChatGPT preferences, CLAUDE.md, static files
      - Tier 2 (Store and retrieve): MemPalace (verbatim ChromaDB, 96.6%), Mem0 (extract-and-store), Supermemory (memory-as-a-service). Equal weight, search is the only intelligence.
      - Tier 3 (Structured memory): Zep (temporal knowledge graph), Cognee (entity/relationship extraction), Hindsight (4 memory networks + reflection). Relationships matter.
      - Tier 4 (Living memory): pg_ghola. Strengthens through use, detects contradictions, consolidates offline, decays, evolves confidence. Only system with all biological processes.
    - Capability comparison table (architectural, not benchmark scores):
      columns: strengthens with use, detects contradictions, forgets gracefully, reorganizes offline, confidence evolution
    - No defensive tone about benchmark scores
    - Sources cited for all systems referenced

  DONE_WHEN:
    - Visitor understands the landscape and where pg_ghola fits
    - The differentiation is clear without claiming pg_ghola is "better at retrieval"
    - Every system mentioned has accurate, sourced information

  SOURCES:
    - MemPalace: github.com/MemPalace/mempalace (96.6% raw LongMemEval, ChromaDB, verbatim storage)
    - Mem0: mem0.ai (extract-and-store, SOC2/HIPAA, 49.0% LongMemEval)
    - Zep: zep.ai (temporal knowledge graph, 63.8% LongMemEval)
    - Letta: letta.com (OS-inspired tiered memory: core/archival/recall)
    - Cognee: cognee.ai (knowledge graph extraction, ECL pipeline, $7.5M seed)
    - Hindsight: vectorize.io (TEMPR + CARA, 91.4% LongMemEval)
    - Supermemory: supermemory.ai (memory-as-a-service, 85.4% LongMemEval)
    - Mastra: mastra.ai (observational memory, 94.9% with gpt-5-mini)
    - OMEGA: omegamax.co (95.4% LongMemEval)

---

FUNCTION: architecture_page(/architecture) -> rendered page

  Title: "The Primitives"

  RULES:
    - Each primitive gets a card with neuroscience analog, computation, why it matters
    - Primitives: Hebbian learning, contradiction detection, consolidation/clustering,
      temporal decay, confidence evolution, thalamic gating, four retrieval pathways
    - System architecture section: pg_ghola as Postgres extension, three background workers
      (Consolidation, Contradiction, Gating), Chapterhouse MCP server
    - Multi-model/multi-agent vision: how multiple AI tools connect to the same memory
    - Multi-tenant/organizational vision: the senior-dev-in-a-box concept
    - No code blocks on this page -- architecture diagrams and descriptions only

  DONE_WHEN:
    - Visitor understands what each primitive does and why it exists
    - The system architecture is clear
    - The multi-tenant vision is conveyed

---

FUNCTION: field_notes_page(/field-notes) -> rendered page

  Title: "Field Notes"

  RULES:
    - The honest engineering story, woven chronologically or thematically
    - Sections:
      - Docker war stories (5 builds, glibc, SIGILL, AVX-512 cross-compilation)
      - Eval-driven development (samsara loop concept, iteration methodology)
      - Key discoveries from 16 iterations:
        - Concept enrichment with user-turn text (+11.2pp, biggest single win)
        - Pool purity matters more than pool size (3 reverted pool-expansion attempts)
        - Embedding non-determinism (TEI CPU float32 differs between runs)
        - Cold benchmarks can't measure biological primitives
        - The benchmark destroyed our real memories (TRUNCATE lesson)
        - Retrieval-time tuning has diminishing returns after encoding-time fixes
        - vLLM vs sentence-transformers: same model, different engine, different embeddings
      - Honest takeaways and where the project goes next
    - Iteration history table with R@5, what was tried, kept/reverted
    - WarStoryCard component for narrative entries
    - InfoBox for key insights

  DONE_WHEN:
    - Reader gets the "real story" of building this, not a polished press release
    - Failures are documented as honestly as successes
    - Each discovery has enough context to be useful as a reference

---

FUNCTION: eval_page(/eval) -> rendered page

  Title: "Measuring Memory"

  RULES:
    - Reframed from "benchmark results" to "how do you evaluate a living memory system"
    - Sections:
      - What cold benchmarks measure: retrieval accuracy on static data
      - LongMemEval results: pg_ghola's numbers, the leaderboard for context
      - What benchmarks miss: 5 of 7 primitives invisible on cold data
        (Hebbian, gating, contradiction, consolidation, confidence)
      - The right evaluation: longitudinal measurement over weeks of real usage
      - Methodology notes: clean benchmark protocol, variance budget, embedding pinning
    - Raw results tables and samsara iteration data for reference
    - Leaderboard comparison table (MemPalace, Mastra, Hindsight, Stella, Contriever, BM25, pgvector, pg_ghola)

  DONE_WHEN:
    - Reader understands why 27.5% R@5 is not the whole story
    - The case for longitudinal evaluation is clear
    - Raw data is available for those who want it

---

## Shared Design System

CONSTRAINT: single_design_system
  All pages share one design system. No duplicated COLORS, Badge, Card, etc.
  Extract shared components into a design-system.tsx file.
  Dark theme (bg #06080c), Inter font, JetBrains Mono for code.
  Consistent color semantics: accent (amber), blue (info), green (success), red (failure/warning).

CONSTRAINT: no_hardcoded_data
  Benchmark data, iteration history, and system counts should be defined in a
  separate data.ts file, not inline in components. Makes updates easy.

CONSTRAINT: shared_navigation
  All pages share a consistent top navigation bar with links to all spoke pages
  and a home link. Sticky, blurred background. Current page highlighted.

CONSTRAINT: grpo_dormant
  CognitiveMemoryTraining.tsx (/training) is not linked from the hub or navigation.
  The file stays in the repo but is not part of the active navigation.
  Can be re-linked when GRPO work resumes.

---

## File Structure

```
src/
  design-system.tsx    -- shared COLORS, Badge, Card, Grid, InfoBox, etc.
  data.ts              -- benchmark results, iteration history, landscape data
  App.tsx              -- router
  HubPage.tsx          -- /
  LandscapePage.tsx    -- /landscape
  ArchitecturePage.tsx -- /architecture
  FieldNotesPage.tsx   -- /field-notes
  EvalPage.tsx         -- /eval
  CognitiveMemoryTraining.tsx  -- /training (dormant, not in nav)
```
