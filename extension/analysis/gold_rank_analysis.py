#!/usr/bin/env python3
"""Analyze where gold mnemes rank in the semantic pathway.

For each failing query, computes:
  1. The cosine similarity rank of the gold mneme in the full embedding space
  2. Whether the gold mneme would be in the top-K pool for various K values

This tells us whether the retrieval bottleneck is:
  - Pool size (gold at rank 31-100 = increase pool would help)
  - Embedding quality (gold at rank 1000+ = encoding changes needed)
  - Scoring (gold in pool but ranked low = scoring formula issue)

Usage:
  python3 analysis/gold_rank_analysis.py results/latest.jsonl [--sample N]
"""

import json
import sys
import subprocess
import urllib.request
import urllib.error
from collections import defaultdict
from pathlib import Path

EMBED_URL = "http://192.168.2.6:8082/v1/embeddings"
EMBED_MODEL = "Qwen/Qwen3-Embedding-0.6B"
DATASET_PATH = Path.home() / "longmemeval-ghola/data/longmemeval_s_cleaned.json"
WORKSPACE_ID = "00000000-0000-0000-0000-000000000001"

def psql(query: str) -> str:
    """Run a psql query on the benchmark database."""
    result = subprocess.run(
        ["kubectl", "exec", "-n", "ch-system", "memory-db-1", "--",
         "psql", "-U", "postgres", "-d", "memories", "-t", "-A", "-c", query],
        capture_output=True, text=True, timeout=60
    )
    return result.stdout.strip()

def get_embedding(text: str) -> list[float]:
    """Get embedding from the local embedding server."""
    payload = json.dumps({"model": EMBED_MODEL, "input": text}).encode()
    req = urllib.request.Request(
        EMBED_URL, data=payload,
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.loads(resp.read())
    return data["data"][0]["embedding"]

def main():
    if len(sys.argv) < 2:
        print("Usage: gold_rank_analysis.py <results.jsonl> [--sample N]")
        sys.exit(1)

    results_path = Path(sys.argv[1])
    if not results_path.is_absolute():
        results_path = Path.home() / "longmemeval-ghola" / results_path

    sample_n = None
    if "--sample" in sys.argv:
        idx = sys.argv.index("--sample")
        sample_n = int(sys.argv[idx + 1])

    # Load dataset for gold sessions
    with open(DATASET_PATH) as f:
        dataset = json.load(f)
    gold_map = {q["question_id"]: q for q in dataset}

    # Load benchmark results
    results = []
    with open(results_path) as f:
        for line in f:
            if line.strip():
                results.append(json.loads(line))

    # Identify failing queries (gold session NOT in top 5)
    failing = []
    hitting = []
    for r in results:
        gold_sessions = set(gold_map[r["question_id"]]["answer_session_ids"])
        result_sessions = {res["session_id"] for res in r["results"][:5]}
        if gold_sessions & result_sessions:
            hitting.append(r)
        else:
            failing.append(r)

    print(f"Total queries: {len(results)}")
    print(f"Hitting R@5: {len(hitting)} ({100*len(hitting)/len(results):.1f}%)")
    print(f"Failing R@5: {len(failing)} ({100*len(failing)/len(results):.1f}%)")
    print()

    # Sample failing queries for analysis
    if sample_n and sample_n < len(failing):
        # Sample proportionally from each category
        by_cat = defaultdict(list)
        for r in failing:
            by_cat[r["question_type"]].append(r)
        sampled = []
        for cat, qs in sorted(by_cat.items()):
            n = max(1, round(sample_n * len(qs) / len(failing)))
            sampled.extend(qs[:n])
        failing = sampled[:sample_n]

    print(f"Analyzing {len(failing)} failing queries...")
    print()

    # For each failing query, find gold mneme's semantic rank
    rank_by_cat = defaultdict(list)
    pool_thresholds = [30, 50, 100, 200, 500]

    for i, r in enumerate(failing):
        qid = r["question_id"]
        query = r["query"]
        cat = r["question_type"]
        gold_sessions = gold_map[qid]["answer_session_ids"]

        # Get query embedding
        try:
            emb = get_embedding(query)
        except Exception as e:
            print(f"  [{i+1}/{len(failing)}] {qid}: embedding error: {e}")
            continue

        emb_str = "[" + ",".join(str(x) for x in emb) + "]"

        # Find gold mnemes via session tags (format: session:<gold_session_id>)
        gold_tag_list = ",".join(f"'session:{s}'" for s in gold_sessions)

        rank_query = f"""
        WITH gold_mnemes AS (
            SELECT id, embedding,
                   (1.0 - (embedding <=> '{emb_str}'::vector))::float8 AS gold_cosine
            FROM ghola.mnemes
            WHERE workspace_id = '{WORKSPACE_ID}'
              AND state = 'active'
              AND tags && ARRAY[{gold_tag_list}]::text[]
        ),
        best_gold AS (
            SELECT id, gold_cosine FROM gold_mnemes ORDER BY gold_cosine DESC LIMIT 1
        )
        SELECT bg.id::text, bg.gold_cosine,
               (SELECT count(*) FROM ghola.mnemes
                WHERE workspace_id = '{WORKSPACE_ID}'
                  AND state = 'active'
                  AND (1.0 - (embedding <=> '{emb_str}'::vector)) > bg.gold_cosine
               ) AS rank_above
        FROM best_gold bg;
        """

        try:
            result = psql(rank_query)
        except Exception as e:
            print(f"  [{i+1}/{len(failing)}] {qid}: query error: {e}")
            continue

        if not result:
            rank_by_cat[cat].append(("not_found", None, None))
            if (i+1) % 10 == 0 or i == 0:
                print(f"  [{i+1}/{len(failing)}] {cat}: {qid} - gold mneme NOT FOUND")
            continue

        parts = result.split("|")
        mneme_id = parts[0].strip()
        cosine = float(parts[1].strip())
        rank_above = int(parts[2].strip())
        semantic_rank = rank_above + 1  # 1-indexed

        rank_by_cat[cat].append(("found", semantic_rank, cosine))
        if (i+1) % 10 == 0 or i == 0:
            print(f"  [{i+1}/{len(failing)}] {cat}: {qid} - semantic rank {semantic_rank}, cosine {cosine:.4f}")

    # Summary
    print()
    print("=" * 70)
    print("GOLD MNEME SEMANTIC RANK DISTRIBUTION (failing queries only)")
    print("=" * 70)

    all_ranks = []
    for cat in sorted(rank_by_cat.keys()):
        entries = rank_by_cat[cat]
        found = [(r, c) for status, r, c in entries if status == "found"]
        not_found = sum(1 for status, _, _ in entries if status == "not_found")

        if not found:
            print(f"\n{cat}: {len(entries)} failing, {not_found} gold not found")
            continue

        ranks = [r for r, c in found]
        cosines = [c for r, c in found]
        all_ranks.extend(ranks)

        print(f"\n{cat} ({len(entries)} failing, {not_found} not found):")
        print(f"  Semantic rank: median={sorted(ranks)[len(ranks)//2]}, "
              f"mean={sum(ranks)/len(ranks):.0f}, "
              f"min={min(ranks)}, max={max(ranks)}")
        print(f"  Cosine sim:    median={sorted(cosines)[len(cosines)//2]:.4f}, "
              f"mean={sum(cosines)/len(cosines):.4f}")

        for k in pool_thresholds:
            in_pool = sum(1 for r in ranks if r <= k)
            print(f"  In top-{k:>3d}: {in_pool}/{len(ranks)} ({100*in_pool/len(ranks):.0f}%)")

    if all_ranks:
        print(f"\nOVERALL ({len(all_ranks)} found):")
        all_ranks.sort()
        print(f"  Semantic rank: median={all_ranks[len(all_ranks)//2]}, "
              f"mean={sum(all_ranks)/len(all_ranks):.0f}, "
              f"min={min(all_ranks)}, max={max(all_ranks)}")
        for k in pool_thresholds:
            in_pool = sum(1 for r in all_ranks if r <= k)
            print(f"  In top-{k:>3d}: {in_pool}/{len(all_ranks)} ({100*in_pool/len(all_ranks):.0f}%)")

        # Histogram
        print(f"\n  Rank distribution:")
        buckets = [(1, 10), (11, 30), (31, 50), (51, 100), (101, 200),
                   (201, 500), (501, 1000), (1001, 5000), (5001, 19000)]
        for lo, hi in buckets:
            count = sum(1 for r in all_ranks if lo <= r <= hi)
            bar = "#" * count
            print(f"    {lo:>5d}-{hi:>5d}: {count:>3d} {bar}")

if __name__ == "__main__":
    main()
