"""seeding-eval — eval harness for the pg_recall seeding pipeline.

Phases (full plan in docs/plans/2026-05-02-eval-harness-design.md):
- extract: GitHub API → JSON cache → bundle JSONL
- cases: build held-out eval cases from the cache
- eval: query ghola recall, compute H1/H2/H3 metrics
"""

__version__ = "0.1.0"
