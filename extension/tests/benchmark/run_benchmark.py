#!/usr/bin/env python3
"""
pg_ghola Cognitive Memory Benchmark Runner

Tests whether retrieving stored memories improves reasoning compared to
computing from scratch. Three conditions:
  1. no_memory    — LLM with no external context
  2. naive_vector — pure cosine similarity retrieval (no cognitive scoring)
  3. cognitive    — full pg_ghola recall pipeline

Usage:
  python3 run_benchmark.py --snapshot baseline
  python3 run_benchmark.py --snapshot after-change --compare baseline
"""

import argparse
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

SCRIPT_DIR = Path(__file__).parent
QUERIES_FILE = SCRIPT_DIR / "queries.json"
SNAPSHOTS_DIR = SCRIPT_DIR / "snapshots"

CHAPTERHOUSE_URL = "https://chapterhouse.thesgc.internal/mcp/stateless"

# LLM config — uses Anthropic API via claude CLI for consistency
# Override with env vars if needed
LLM_MODEL = os.environ.get("BENCHMARK_MODEL", "claude-sonnet-4-20250514")
LLM_MAX_TOKENS = int(os.environ.get("BENCHMARK_MAX_TOKENS", "1024"))

SYSTEM_PROMPT_BASE = """You are an AI assistant. Answer the question as accurately
and concisely as possible. If you don't know something, say so — don't guess."""

SYSTEM_PROMPT_WITH_CONTEXT = """You are an AI assistant with access to a memory system.
The following memories have been retrieved as potentially relevant context.
Use them to answer the question accurately and concisely. Cite specific memories
when they inform your answer. If the memories don't cover something, say so."""


def get_api_key():
    """Load Chapterhouse API key from secrets."""
    secrets_path = Path.home() / ".openclaw" / "secrets.json"
    if not secrets_path.exists():
        # Try workspace copy
        secrets_path = Path.home() / ".openclaw" / "workspace" / ".openclaw" / "secrets.json"
    with open(secrets_path) as f:
        secrets = json.load(f)
    return secrets["values"]["chapterhouse/apiKey"]


def ch_call(tool_name: str, arguments: dict) -> str:
    """Call a Chapterhouse MCP tool, return the text content."""
    api_key = get_api_key()
    payload = {
        "jsonrpc": "2.0",
        "method": "tools/call",
        "params": {"name": tool_name, "arguments": arguments},
        "id": 1,
    }
    result = subprocess.run(
        [
            "curl", "-sk", "-X", "POST", CHAPTERHOUSE_URL,
            "-H", "Content-Type: application/json",
            "-H", f"Authorization: Bearer {api_key}",
            "-d", json.dumps(payload),
        ],
        capture_output=True, text=True, timeout=30,
    )
    resp = json.loads(result.stdout)
    return resp["result"]["content"][0]["text"]


def recall_cognitive(query: str, limit: int = 10) -> list[dict]:
    """Full cognitive recall via Chapterhouse MCP."""
    text = ch_call("recall", {"query": query, "limit": limit})
    return parse_recall_results(text)


def recall_naive_vector(query: str, limit: int = 10) -> list[dict]:
    """
    Naive vector search — call recall but we'll parse results.
    Ideally we'd query pgvector directly, but going through ch-server
    with a note that this uses the full pipeline. For a proper naive
    baseline, we'd need a direct SQL query.

    TODO: Add a direct pgvector-only SQL path for true naive comparison.
    For now, we use recall results but strip cognitive metadata to simulate
    what a naive system would surface (same candidates, just cosine-ranked).
    """
    # Use same recall but note: this isn't truly naive.
    # The real benchmark should add a direct vector query endpoint.
    text = ch_call("recall", {"query": query, "limit": limit})
    results = parse_recall_results(text)
    # Re-sort by content_match only (strip temporal/confidence influence)
    # This approximates naive vector ranking
    return results  # TODO: re-rank by cosine only when we add raw scores


def parse_recall_results(text: str) -> list[dict]:
    """Parse Chapterhouse recall output into structured results."""
    import re
    results = []
    # Format: [uuid] [scope] (score=X.XX) content...
    pattern = r'\[([a-f0-9-]+)\] \[(\w+)\] \(score=([\d.]+)\) (.+?)(?=\n\[|$)'
    for match in re.finditer(pattern, text, re.DOTALL):
        results.append({
            "id": match.group(1),
            "scope": match.group(2),
            "score": float(match.group(3)),
            "content": match.group(4).strip(),
        })
    return results


def call_llm(system_prompt: str, user_message: str) -> dict:
    """
    Call an LLM and capture response + token usage.
    Uses the Anthropic API directly via curl for token counting.
    """
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")
    if not api_key:
        # Try to read from common locations
        for path in [
            Path.home() / ".anthropic" / "api_key",
            Path.home() / ".config" / "anthropic" / "api_key",
        ]:
            if path.exists():
                api_key = path.read_text().strip()
                break

    if not api_key:
        return {
            "response": "[SKIPPED: No ANTHROPIC_API_KEY set]",
            "input_tokens": 0,
            "output_tokens": 0,
            "latency_ms": 0,
            "model": LLM_MODEL,
        }

    payload = {
        "model": LLM_MODEL,
        "max_tokens": LLM_MAX_TOKENS,
        "system": system_prompt,
        "messages": [{"role": "user", "content": user_message}],
    }

    start = time.monotonic()
    result = subprocess.run(
        [
            "curl", "-sk", "-X", "POST", "https://api.anthropic.com/v1/messages",
            "-H", "Content-Type: application/json",
            "-H", f"x-api-key: {api_key}",
            "-H", "anthropic-version: 2023-06-01",
            "-d", json.dumps(payload),
        ],
        capture_output=True, text=True, timeout=120,
    )
    elapsed = (time.monotonic() - start) * 1000

    resp = json.loads(result.stdout)

    return {
        "response": resp.get("content", [{}])[0].get("text", "[error]"),
        "input_tokens": resp.get("usage", {}).get("input_tokens", 0),
        "output_tokens": resp.get("usage", {}).get("output_tokens", 0),
        "latency_ms": round(elapsed),
        "model": resp.get("model", LLM_MODEL),
    }


def format_memories_as_context(memories: list[dict]) -> str:
    """Format recall results as context for the LLM."""
    if not memories:
        return "(No relevant memories found)"
    lines = []
    for i, m in enumerate(memories, 1):
        lines.append(f"Memory {i} (relevance: {m['score']:.2f}):\n{m['content']}")
    return "\n\n".join(lines)


def run_query(query_obj: dict, condition: str) -> dict:
    """Run a single query under one condition."""
    query_text = query_obj["query"]
    result = {
        "query_id": query_obj["id"],
        "category": query_obj["category"],
        "condition": condition,
        "query": query_text,
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }

    if condition == "no_memory":
        llm_result = call_llm(SYSTEM_PROMPT_BASE, query_text)
        result["memories"] = []
        result["memory_count"] = 0

    elif condition == "naive_vector":
        memories = recall_naive_vector(query_text, limit=5)
        context = format_memories_as_context(memories)
        prompt = f"{query_text}\n\n---\nRetrieved context:\n{context}"
        llm_result = call_llm(SYSTEM_PROMPT_WITH_CONTEXT, prompt)
        result["memories"] = memories
        result["memory_count"] = len(memories)

    elif condition == "cognitive":
        memories = recall_cognitive(query_text, limit=5)
        context = format_memories_as_context(memories)
        prompt = f"{query_text}\n\n---\nRetrieved context:\n{context}"
        llm_result = call_llm(SYSTEM_PROMPT_WITH_CONTEXT, prompt)
        result["memories"] = memories
        result["memory_count"] = len(memories)

    else:
        raise ValueError(f"Unknown condition: {condition}")

    result.update(llm_result)

    # Auto-score: check how many expected knowledge items appear in response
    expected = query_obj.get("expected_knowledge", [])
    response_lower = llm_result["response"].lower()
    hits = sum(1 for k in expected if any(
        term.lower() in response_lower
        for term in k.split(" ")[:3]  # Check first 3 words as rough match
    ))
    result["expected_knowledge_count"] = len(expected)
    result["knowledge_hits_rough"] = hits

    return result


def run_benchmark(snapshot_name: str, conditions: list[str] = None):
    """Run the full benchmark suite."""
    if conditions is None:
        conditions = ["no_memory", "cognitive"]

    with open(QUERIES_FILE) as f:
        queries = json.load(f)["queries"]

    snapshot_dir = SNAPSHOTS_DIR / snapshot_name
    snapshot_dir.mkdir(parents=True, exist_ok=True)

    results = {
        "snapshot": snapshot_name,
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "model": LLM_MODEL,
        "query_count": len(queries),
        "conditions": conditions,
        "results": [],
    }

    total = len(queries) * len(conditions)
    i = 0
    for query_obj in queries:
        for condition in conditions:
            i += 1
            print(f"[{i}/{total}] {query_obj['id']} / {condition}...", end=" ", flush=True)
            try:
                result = run_query(query_obj, condition)
                results["results"].append(result)
                tokens = result["input_tokens"] + result["output_tokens"]
                print(f"done ({tokens} tokens, {result['latency_ms']}ms)")
            except Exception as e:
                print(f"ERROR: {e}")
                results["results"].append({
                    "query_id": query_obj["id"],
                    "condition": condition,
                    "error": str(e),
                })

    # Save results
    output_file = snapshot_dir / "results.json"
    with open(output_file, "w") as f:
        json.dump(results, f, indent=2)
    print(f"\nResults saved to {output_file}")

    # Generate summary
    generate_summary(results, snapshot_dir)

    return results


def generate_summary(results: dict, output_dir: Path):
    """Generate a human-readable summary of benchmark results."""
    lines = [
        f"# Benchmark: {results['snapshot']}",
        f"Date: {results['timestamp']}",
        f"Model: {results['model']}",
        f"Queries: {results['query_count']}",
        "",
        "## Per-Condition Summary",
        "",
    ]

    for condition in results["conditions"]:
        cond_results = [r for r in results["results"] if r.get("condition") == condition and "error" not in r]
        if not cond_results:
            continue

        total_input = sum(r["input_tokens"] for r in cond_results)
        total_output = sum(r["output_tokens"] for r in cond_results)
        avg_latency = sum(r["latency_ms"] for r in cond_results) / len(cond_results)
        knowledge_hits = sum(r.get("knowledge_hits_rough", 0) for r in cond_results)
        knowledge_total = sum(r.get("expected_knowledge_count", 0) for r in cond_results)

        lines.extend([
            f"### {condition}",
            f"- Queries: {len(cond_results)}",
            f"- Total tokens: {total_input + total_output:,} (input: {total_input:,}, output: {total_output:,})",
            f"- Avg latency: {avg_latency:.0f}ms",
            f"- Knowledge coverage (rough): {knowledge_hits}/{knowledge_total} ({100*knowledge_hits/max(knowledge_total,1):.0f}%)",
            "",
        ])

    lines.extend(["", "## Per-Query Results", ""])

    for query_id in dict.fromkeys(r.get("query_id") for r in results["results"]):
        query_results = [r for r in results["results"] if r.get("query_id") == query_id]
        lines.append(f"### {query_id}")
        for r in query_results:
            if "error" in r:
                lines.append(f"  **{r['condition']}**: ERROR — {r['error']}")
                continue
            mem_count = r.get("memory_count", 0)
            hits = r.get("knowledge_hits_rough", 0)
            expected = r.get("expected_knowledge_count", 0)
            lines.append(
                f"  **{r['condition']}**: {r['input_tokens']+r['output_tokens']} tokens, "
                f"{r['latency_ms']}ms, knowledge {hits}/{expected}, "
                f"{mem_count} memories retrieved"
            )
        lines.append("")

    summary_file = output_dir / "summary.md"
    with open(summary_file, "w") as f:
        f.write("\n".join(lines))
    print(f"Summary saved to {summary_file}")


def compare_snapshots(baseline: str, current: str):
    """Compare two snapshots and show deltas."""
    baseline_file = SNAPSHOTS_DIR / baseline / "results.json"
    current_file = SNAPSHOTS_DIR / current / "results.json"

    if not baseline_file.exists():
        print(f"Baseline snapshot not found: {baseline_file}")
        sys.exit(1)
    if not current_file.exists():
        print(f"Current snapshot not found: {current_file}")
        sys.exit(1)

    with open(baseline_file) as f:
        base = json.load(f)
    with open(current_file) as f:
        curr = json.load(f)

    print(f"\n{'='*60}")
    print(f"Comparison: {baseline} → {current}")
    print(f"{'='*60}\n")

    for condition in set(base.get("conditions", [])) & set(curr.get("conditions", [])):
        base_results = {r["query_id"]: r for r in base["results"]
                       if r.get("condition") == condition and "error" not in r}
        curr_results = {r["query_id"]: r for r in curr["results"]
                       if r.get("condition") == condition and "error" not in r}

        common_ids = set(base_results) & set(curr_results)
        if not common_ids:
            continue

        print(f"### {condition} ({len(common_ids)} queries)")

        # Token delta
        base_tokens = sum(base_results[qid]["input_tokens"] + base_results[qid]["output_tokens"]
                         for qid in common_ids)
        curr_tokens = sum(curr_results[qid]["input_tokens"] + curr_results[qid]["output_tokens"]
                         for qid in common_ids)
        delta_tokens = curr_tokens - base_tokens
        pct = 100 * delta_tokens / max(base_tokens, 1)
        print(f"  Tokens: {base_tokens:,} → {curr_tokens:,} ({delta_tokens:+,}, {pct:+.1f}%)")

        # Knowledge hits delta
        base_hits = sum(base_results[qid].get("knowledge_hits_rough", 0) for qid in common_ids)
        curr_hits = sum(curr_results[qid].get("knowledge_hits_rough", 0) for qid in common_ids)
        base_total = sum(base_results[qid].get("expected_knowledge_count", 0) for qid in common_ids)
        print(f"  Knowledge: {base_hits}/{base_total} → {curr_hits}/{base_total}")

        # Latency delta
        base_lat = sum(base_results[qid]["latency_ms"] for qid in common_ids) / len(common_ids)
        curr_lat = sum(curr_results[qid]["latency_ms"] for qid in common_ids) / len(common_ids)
        print(f"  Avg latency: {base_lat:.0f}ms → {curr_lat:.0f}ms")

        print()


def main():
    parser = argparse.ArgumentParser(description="pg_ghola Cognitive Memory Benchmark")
    parser.add_argument("--snapshot", required=True, help="Name for this benchmark run")
    parser.add_argument("--compare", help="Compare against this baseline snapshot")
    parser.add_argument("--conditions", nargs="+",
                       default=["no_memory", "cognitive"],
                       choices=["no_memory", "naive_vector", "cognitive"],
                       help="Conditions to test")
    parser.add_argument("--queries", nargs="+", help="Run only these query IDs")
    args = parser.parse_args()

    results = run_benchmark(args.snapshot, args.conditions)

    if args.compare:
        compare_snapshots(args.compare, args.snapshot)


if __name__ == "__main__":
    main()
