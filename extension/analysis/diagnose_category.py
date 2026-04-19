#!/usr/bin/env python3
"""Diagnose recall failures for a benchmark category.

Compares benchmark expected answers against actual recall results.
Prints a compact summary -- keeps raw data out of LLM context.

Usage:
    python3 analysis/diagnose_category.py --results <results.jsonl> --category <name>
    python3 analysis/diagnose_category.py --results latest --category single-session-user
"""

import argparse
import json
import subprocess
import sys
from pathlib import Path
from collections import Counter


def find_latest_results():
    """Find the most recent results file."""
    results_dir = Path.home() / "longmemeval-ghola" / "results"
    if not results_dir.exists():
        print(f"Results directory not found: {results_dir}", file=sys.stderr)
        sys.exit(1)
    files = sorted(results_dir.glob("ghola_mcp_s_*.jsonl"), key=lambda p: p.stat().st_mtime)
    if not files:
        print("No results files found", file=sys.stderr)
        sys.exit(1)
    return files[-1]


def load_results(path):
    """Load JSONL results file."""
    results = []
    with open(path) as f:
        for line in f:
            if line.strip():
                results.append(json.loads(line))
    return results


def query_db(sql):
    """Run a SQL query against the memories database."""
    cmd = [
        "kubectl", "exec", "-n", "ch-system", "memory-db-1", "--",
        "psql", "-U", "postgres", "-d", "memories", "-t", "-A", "-c", sql
    ]
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    return result.stdout.strip()


def main():
    parser = argparse.ArgumentParser(description="Diagnose recall failures by category")
    parser.add_argument("--results", default="latest", help="Results JSONL file or 'latest'")
    parser.add_argument("--category", required=True, help="Category to analyze")
    parser.add_argument("--top-n", type=int, default=5, help="Number of example failures to show")
    args = parser.parse_args()

    # Load results
    if args.results == "latest":
        results_path = find_latest_results()
    else:
        results_path = Path(args.results)
    print(f"Results file: {results_path.name}")

    results = load_results(results_path)
    category_results = [r for r in results if r.get("category") == args.category]

    if not category_results:
        print(f"No results for category '{args.category}'")
        print(f"Available: {sorted(set(r.get('category', '?') for r in results))}")
        sys.exit(1)

    # Compute stats
    total = len(category_results)
    hits_at = {k: 0 for k in [1, 5, 10]}
    ranks = []
    failures = []

    for r in category_results:
        rank = r.get("rank")
        if rank is not None and rank > 0:
            ranks.append(rank)
            for k in hits_at:
                if rank <= k:
                    hits_at[k] += 1
        else:
            failures.append(r)

    # Summary
    print(f"\n=== {args.category} ({total} queries) ===")
    print(f"R@1: {hits_at[1]}/{total} ({100*hits_at[1]/total:.1f}%)")
    print(f"R@5: {hits_at[5]}/{total} ({100*hits_at[5]/total:.1f}%)")
    print(f"R@10: {hits_at[10]}/{total} ({100*hits_at[10]/total:.1f}%)")

    if ranks:
        print(f"Hits: median rank {sorted(ranks)[len(ranks)//2]}, "
              f"mean {sum(ranks)/len(ranks):.1f}")

    print(f"Failures: {len(failures)}/{total}")

    if not failures:
        print("All queries hit -- nothing to diagnose.")
        return

    # Analyze failure patterns
    print(f"\n--- Top {min(args.top_n, len(failures))} failures ---")
    for i, r in enumerate(failures[:args.top_n]):
        query = r.get("query", "?")[:100]
        answer_ids = r.get("answer_session_ids", r.get("expected", "?"))
        print(f"\n  [{i+1}] Q: {query}")
        print(f"      Expected: {answer_ids}")

        # Check if answer is in the DB
        if isinstance(answer_ids, list) and answer_ids:
            aid = answer_ids[0]
            db_check = query_db(
                f"SELECT concept FROM ghola.mnemes "
                f"WHERE id::text LIKE '%{aid[:8]}%' LIMIT 1"
            )
            if db_check:
                print(f"      In DB: yes (concept: {db_check[:80]})")
            else:
                print(f"      In DB: not found")

    # Pattern analysis
    print(f"\n--- Failure patterns ---")
    query_lengths = [len(r.get("query", "")) for r in failures]
    print(f"Query length: median {sorted(query_lengths)[len(query_lengths)//2]}, "
          f"mean {sum(query_lengths)/len(query_lengths):.0f}")

    # Check for common query prefixes
    prefixes = Counter()
    for r in failures:
        q = r.get("query", "")
        first_words = " ".join(q.split()[:3])
        prefixes[first_words] += 1
    print(f"Common query starts: {prefixes.most_common(5)}")


if __name__ == "__main__":
    main()
