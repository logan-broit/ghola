#!/usr/bin/env python3
"""Compare two benchmark runs side by side.

Prints a compact diff showing which categories improved/regressed
and which specific queries changed status (hit->miss, miss->hit).

Usage:
    python3 analysis/compare_runs.py --before <results.jsonl> --after <results.jsonl>
    python3 analysis/compare_runs.py --before results/run1.jsonl --after latest
"""

import argparse
import json
import sys
from pathlib import Path


def find_latest_results():
    results_dir = Path.home() / "longmemeval-ghola" / "results"
    files = sorted(results_dir.glob("ghola_mcp_s_*.jsonl"), key=lambda p: p.stat().st_mtime)
    return files[-1] if files else None


def load_results(path):
    results = {}
    with open(path) as f:
        for line in f:
            if line.strip():
                r = json.loads(line)
                qid = r.get("query_id", r.get("id", ""))
                results[qid] = r
    return results


def recall_at_k(results, k):
    by_cat = {}
    for r in results.values():
        cat = r.get("category", "unknown")
        if cat not in by_cat:
            by_cat[cat] = {"hit": 0, "total": 0}
        by_cat[cat]["total"] += 1
        rank = r.get("rank")
        if rank is not None and 0 < rank <= k:
            by_cat[cat]["hit"] += 1
    return by_cat


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--before", required=True)
    parser.add_argument("--after", default="latest")
    parser.add_argument("-k", type=int, default=5)
    args = parser.parse_args()

    before_path = Path(args.before) if args.before != "latest" else find_latest_results()
    after_path = Path(args.after) if args.after != "latest" else find_latest_results()

    before = load_results(before_path)
    after = load_results(after_path)

    print(f"Before: {before_path.name} ({len(before)} queries)")
    print(f"After:  {after_path.name} ({len(after)} queries)")

    # Category comparison
    b_cats = recall_at_k(before, args.k)
    a_cats = recall_at_k(after, args.k)

    all_cats = sorted(set(list(b_cats.keys()) + list(a_cats.keys())))

    print(f"\n{'Category':<30} {'Before R@{}'.format(args.k):>12} {'After R@{}'.format(args.k):>12} {'Delta':>8}")
    print("-" * 65)

    for cat in all_cats:
        b = b_cats.get(cat, {"hit": 0, "total": 0})
        a = a_cats.get(cat, {"hit": 0, "total": 0})
        b_pct = 100 * b["hit"] / b["total"] if b["total"] else 0
        a_pct = 100 * a["hit"] / a["total"] if a["total"] else 0
        delta = a_pct - b_pct
        marker = "+" if delta > 0 else "" if delta == 0 else ""
        print(f"{cat:<30} {b_pct:>10.1f}% {a_pct:>10.1f}% {marker}{delta:>+7.1f}pp")

    b_total = sum(c["hit"] for c in b_cats.values())
    a_total = sum(c["hit"] for c in a_cats.values())
    b_n = sum(c["total"] for c in b_cats.values())
    a_n = sum(c["total"] for c in a_cats.values())
    print("-" * 65)
    print(f"{'Overall':<30} {100*b_total/b_n:>10.1f}% {100*a_total/a_n:>10.1f}% {100*(a_total/a_n - b_total/b_n):>+7.1f}pp")

    # Per-query changes
    gained = []
    lost = []
    common_ids = set(before.keys()) & set(after.keys())

    for qid in common_ids:
        b_rank = before[qid].get("rank")
        a_rank = after[qid].get("rank")
        b_hit = b_rank is not None and 0 < b_rank <= args.k
        a_hit = a_rank is not None and 0 < a_rank <= args.k

        if a_hit and not b_hit:
            gained.append((after[qid].get("category", "?"),
                           after[qid].get("query", "?")[:80],
                           a_rank))
        elif b_hit and not a_hit:
            lost.append((before[qid].get("category", "?"),
                         before[qid].get("query", "?")[:80],
                         b_rank))

    if gained:
        print(f"\n--- Gained ({len(gained)} queries now hit at R@{args.k}) ---")
        for cat, q, rank in gained[:10]:
            print(f"  [{cat}] rank {rank}: {q}")

    if lost:
        print(f"\n--- Lost ({len(lost)} queries no longer hit at R@{args.k}) ---")
        for cat, q, rank in lost[:10]:
            print(f"  [{cat}] was rank {rank}: {q}")


if __name__ == "__main__":
    main()
