#!/usr/bin/env python3
"""Analyze variance across multiple benchmark runs.

Prints run-to-run consistency metrics: pairwise Jaccard, stable hit set,
per-category variance, and identifies queries that flip between runs.

Usage:
    python3 analysis/variance_report.py results/run1.jsonl results/run2.jsonl [results/run3.jsonl ...]
"""

import json
import sys
from itertools import combinations
from pathlib import Path


def load_hits(path: str, k: int = 5) -> tuple[dict, set]:
    """Load results and return (per-category counts, hit query IDs)."""
    cats = {}
    hit_qids = set()
    with open(path) as f:
        for line in f:
            d = json.loads(line)
            cat = d["question_type"]
            qid = d["question_id"]
            gt = set(d["ground_truth_sessions"])
            retrieved = [r["session_id"] for r in d.get("results", [])[:k]]
            cats.setdefault(cat, {"hit": 0, "total": 0})
            cats[cat]["total"] += 1
            if gt & set(retrieved):
                cats[cat]["hit"] += 1
                hit_qids.add(qid)
    return cats, hit_qids


def main():
    if len(sys.argv) < 3:
        print(f"Usage: {sys.argv[0]} <run1.jsonl> <run2.jsonl> [run3.jsonl ...]")
        sys.exit(1)

    paths = sys.argv[1:]
    runs = []
    for p in paths:
        cats, hits = load_hits(p)
        total = sum(c["total"] for c in cats.values())
        n_hits = sum(c["hit"] for c in cats.values())
        runs.append({
            "path": Path(p).name,
            "cats": cats,
            "hits": hits,
            "r5": 100 * n_hits / total if total else 0,
            "n_hits": n_hits,
            "total": total,
        })

    # Overall R@5 per run
    print("=== R@5 per run ===")
    for i, r in enumerate(runs):
        print(f"  Run {i+1}: {r['r5']:5.1f}% ({r['n_hits']}/{r['total']}) -- {r['path']}")

    r5_values = [r["r5"] for r in runs]
    mean_r5 = sum(r5_values) / len(r5_values)
    spread = max(r5_values) - min(r5_values)
    print(f"  Mean: {mean_r5:.1f}%, Spread: {spread:.1f}pp")

    # Pairwise Jaccard
    print("\n=== Pairwise Jaccard (hit set overlap) ===")
    jaccards = []
    for (i, ri), (j, rj) in combinations(enumerate(runs), 2):
        union = ri["hits"] | rj["hits"]
        inter = ri["hits"] & rj["hits"]
        jac = len(inter) / len(union) if union else 1.0
        jaccards.append(jac)
        print(f"  Run {i+1} vs {j+1}: {jac:.3f} (stable={len(inter)}, union={len(union)})")
    mean_jac = sum(jaccards) / len(jaccards) if jaccards else 0
    print(f"  Mean Jaccard: {mean_jac:.3f}")

    # Stable core (hits in ALL runs)
    all_hits = [r["hits"] for r in runs]
    stable_core = set.intersection(*all_hits) if all_hits else set()
    any_hit = set.union(*all_hits) if all_hits else set()
    print(f"\n=== Stability ===")
    print(f"  Stable core (hit in all {len(runs)} runs): {len(stable_core)}")
    print(f"  Hit in any run: {len(any_hit)}")
    print(f"  Stability ratio: {len(stable_core)/len(any_hit):.3f}" if any_hit else "  (no hits)")

    # Per-category variance
    all_cats = sorted(set(c for r in runs for c in r["cats"]))
    print(f"\n=== Per-category R@5 ===")
    header = f"{'Category':<30}" + "".join(f"{'R'+str(i+1):>8}" for i in range(len(runs))) + f"{'Mean':>8}{'Spread':>8}"
    print(header)
    print("-" * len(header))
    for cat in all_cats:
        vals = []
        parts = []
        for r in runs:
            c = r["cats"].get(cat, {"hit": 0, "total": 0})
            pct = 100 * c["hit"] / c["total"] if c["total"] else 0
            vals.append(pct)
            parts.append(f"{pct:>7.1f}%")
        m = sum(vals) / len(vals)
        s = max(vals) - min(vals)
        print(f"{cat:<30}" + "".join(parts) + f"{m:>7.1f}%{s:>7.1f}pp")


if __name__ == "__main__":
    main()
