#!/usr/bin/env python3
"""Diagnose FTS matching patterns for failing queries.

For a sample of failing queries, checks:
  1. Does the gold mneme match the FTS filter (@@ plainto_tsquery)?
  2. How many semantic top-30 candidates ALSO match FTS?
  3. What's the FTS rank of gold vs competitors?

This tells us whether FTS is helping, hurting, or irrelevant.

Usage:
  python3 analysis/fts_diagnostic.py results/latest.jsonl [--sample N]
"""

import json
import sys
import subprocess
import urllib.request
from collections import defaultdict
from pathlib import Path

EMBED_URL = "http://192.168.2.6:8082/v1/embeddings"
EMBED_MODEL = "Qwen/Qwen3-Embedding-0.6B"
DATASET_PATH = Path.home() / "longmemeval-ghola/data/longmemeval_s_cleaned.json"
WORKSPACE_ID = "00000000-0000-0000-0000-000000000001"


def psql(query: str) -> str:
    result = subprocess.run(
        ["kubectl", "exec", "-n", "ch-system", "memory-db-1", "--",
         "psql", "-U", "postgres", "-d", "memories", "-t", "-A", "-c", query],
        capture_output=True, text=True, timeout=120
    )
    return result.stdout.strip()


def get_embedding(text: str) -> list[float]:
    payload = json.dumps({"model": EMBED_MODEL, "input": text}).encode()
    req = urllib.request.Request(
        EMBED_URL, data=payload,
        headers={"Content-Type": "application/json"}, method="POST"
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.loads(resp.read())
    return data["data"][0]["embedding"]


def main():
    results_path = Path(sys.argv[1])
    if not results_path.is_absolute():
        results_path = Path.home() / "longmemeval-ghola" / results_path

    sample_n = 30
    if "--sample" in sys.argv:
        idx = sys.argv.index("--sample")
        sample_n = int(sys.argv[idx + 1])

    with open(DATASET_PATH) as f:
        dataset = json.load(f)
    gold_map = {q["question_id"]: q for q in dataset}

    results = []
    with open(results_path) as f:
        for line in f:
            if line.strip():
                results.append(json.loads(line))

    # Get failing queries
    failing = []
    for r in results:
        gold_sessions = set(gold_map[r["question_id"]]["answer_session_ids"])
        result_sessions = {res["session_id"] for res in r["results"][:5]}
        if not (gold_sessions & result_sessions):
            failing.append(r)

    # Sample proportionally
    by_cat = defaultdict(list)
    for r in failing:
        by_cat[r["question_type"]].append(r)
    sampled = []
    for cat, qs in sorted(by_cat.items()):
        n = max(1, round(sample_n * len(qs) / len(failing)))
        sampled.extend(qs[:n])
    failing = sampled[:sample_n]

    print(f"Analyzing FTS patterns for {len(failing)} failing queries...\n")

    # Track patterns
    stats = defaultdict(lambda: {"gold_fts_match": 0, "gold_no_fts": 0,
                                  "competitors_with_fts": [], "gold_found": 0,
                                  "total": 0, "gold_in_pool": 0,
                                  "gold_outranked_by_fts": 0})

    for i, r in enumerate(failing):
        qid = r["question_id"]
        query = r["query"]
        cat = r["question_type"]
        gold_sessions = gold_map[qid]["answer_session_ids"]

        try:
            emb = get_embedding(query)
        except Exception as e:
            continue

        emb_str = "[" + ",".join(str(x) for x in emb) + "]"
        escaped_query = query.replace("'", "''")
        gold_tag_list = ",".join(f"'session:{s}'" for s in gold_sessions)

        # Single query: get gold mneme info + count of FTS-matching competitors in top-30
        diag_query = f"""
        WITH query_emb AS (SELECT '{emb_str}'::vector AS emb),
        semantic_top30 AS (
            SELECT id, tags,
                   (1.0 - (embedding <=> (SELECT emb FROM query_emb)))::float8 AS cosine_sim,
                   ts_rank(search_vector, plainto_tsquery('english', '{escaped_query}'))::float8 AS fts_rank,
                   (search_vector @@ plainto_tsquery('english', '{escaped_query}')) AS fts_match
            FROM ghola.mnemes
            WHERE workspace_id = '{WORKSPACE_ID}' AND state = 'active'
            ORDER BY embedding <=> (SELECT emb FROM query_emb)
            LIMIT 30
        ),
        gold_info AS (
            SELECT id,
                   (1.0 - (embedding <=> (SELECT emb FROM query_emb)))::float8 AS cosine_sim,
                   ts_rank(search_vector, plainto_tsquery('english', '{escaped_query}'))::float8 AS fts_rank,
                   (search_vector @@ plainto_tsquery('english', '{escaped_query}')) AS fts_match
            FROM ghola.mnemes
            WHERE workspace_id = '{WORKSPACE_ID}' AND state = 'active'
              AND tags && ARRAY[{gold_tag_list}]::text[]
            ORDER BY (1.0 - (embedding <=> (SELECT emb FROM query_emb)))::float8 DESC
            LIMIT 1
        )
        SELECT
            (SELECT count(*) FROM semantic_top30 WHERE fts_match) AS pool_fts_count,
            (SELECT count(*) FROM semantic_top30) AS pool_total,
            (SELECT fts_match FROM gold_info LIMIT 1) AS gold_fts_match,
            (SELECT cosine_sim FROM gold_info LIMIT 1) AS gold_cosine,
            (SELECT fts_rank FROM gold_info LIMIT 1) AS gold_fts_rank,
            (SELECT CASE WHEN (SELECT id FROM gold_info LIMIT 1) IN (SELECT id FROM semantic_top30) THEN 1 ELSE 0 END) AS gold_in_pool,
            (SELECT count(*) FROM semantic_top30
             WHERE fts_match
               AND cosine_sim > COALESCE((SELECT cosine_sim FROM gold_info LIMIT 1), 1.0)) AS fts_competitors_above_gold;
        """

        try:
            result = psql(diag_query)
        except Exception:
            continue

        if not result:
            continue

        parts = result.split("|")
        if len(parts) < 7:
            continue

        pool_fts_count = int(parts[0].strip()) if parts[0].strip() else 0
        pool_total = int(parts[1].strip()) if parts[1].strip() else 0
        gold_fts = parts[2].strip() == "t" if parts[2].strip() else None
        gold_cosine = float(parts[3].strip()) if parts[3].strip() else None
        gold_fts_rank = float(parts[4].strip()) if parts[4].strip() else None
        gold_in_pool = int(parts[5].strip()) if parts[5].strip() else 0
        fts_above = int(parts[6].strip()) if parts[6].strip() else 0

        s = stats[cat]
        s["total"] += 1
        if gold_cosine is not None:
            s["gold_found"] += 1
            if gold_fts:
                s["gold_fts_match"] += 1
            else:
                s["gold_no_fts"] += 1
            s["competitors_with_fts"].append(pool_fts_count)
            if gold_in_pool:
                s["gold_in_pool"] += 1
            if not gold_fts and fts_above > 0:
                s["gold_outranked_by_fts"] += 1

        if (i+1) % 10 == 0:
            print(f"  [{i+1}/{len(failing)}] {cat}: pool_fts={pool_fts_count}/30, "
                  f"gold_fts={gold_fts}, gold_in_pool={'Y' if gold_in_pool else 'N'}, "
                  f"fts_above_gold={fts_above}")

    # Summary
    print("\n" + "=" * 70)
    print("FTS DIAGNOSTIC SUMMARY")
    print("=" * 70)

    total_found = 0
    total_gold_fts = 0
    total_gold_no_fts = 0
    total_in_pool = 0
    total_outranked = 0
    all_fts_counts = []

    for cat in sorted(stats.keys()):
        s = stats[cat]
        total_found += s["gold_found"]
        total_gold_fts += s["gold_fts_match"]
        total_gold_no_fts += s["gold_no_fts"]
        total_in_pool += s["gold_in_pool"]
        total_outranked += s["gold_outranked_by_fts"]
        all_fts_counts.extend(s["competitors_with_fts"])

        if s["gold_found"] == 0:
            continue

        avg_pool_fts = sum(s["competitors_with_fts"]) / len(s["competitors_with_fts"]) if s["competitors_with_fts"] else 0
        print(f"\n{cat} (n={s['total']}, gold_found={s['gold_found']}):")
        print(f"  Gold has FTS match:    {s['gold_fts_match']}/{s['gold_found']} ({100*s['gold_fts_match']/s['gold_found']:.0f}%)")
        print(f"  Gold in semantic pool: {s['gold_in_pool']}/{s['gold_found']} ({100*s['gold_in_pool']/s['gold_found']:.0f}%)")
        print(f"  Avg FTS matches in pool: {avg_pool_fts:.1f}/30")
        print(f"  Gold outranked by FTS-boosted non-gold: {s['gold_outranked_by_fts']}/{s['gold_found']}")

    if total_found > 0:
        avg_all = sum(all_fts_counts) / len(all_fts_counts) if all_fts_counts else 0
        print(f"\nOVERALL (n={total_found}):")
        print(f"  Gold has FTS match:    {total_gold_fts}/{total_found} ({100*total_gold_fts/total_found:.0f}%)")
        print(f"  Gold NO FTS match:     {total_gold_no_fts}/{total_found} ({100*total_gold_no_fts/total_found:.0f}%)")
        print(f"  Gold in semantic pool: {total_in_pool}/{total_found} ({100*total_in_pool/total_found:.0f}%)")
        print(f"  Avg FTS matches in pool: {avg_all:.1f}/30")
        print(f"  Gold outranked by FTS-boosted: {total_outranked}/{total_found}")
        print()
        print("KEY INSIGHT:")
        if total_gold_no_fts > total_gold_fts:
            print(f"  {100*total_gold_no_fts/total_found:.0f}% of gold mnemes LACK FTS match.")
            print("  They lose to FTS-boosted competitors with similar cosine similarity.")
            print("  Fix: improve encoding to make gold mnemes FTS-matchable.")
        else:
            print(f"  {100*total_gold_fts/total_found:.0f}% of gold mnemes HAVE FTS match.")
            print("  FTS isn't the differentiator. Issue is semantic similarity compression.")
            print("  Fix: improve embedding quality or add new discriminative signals.")


if __name__ == "__main__":
    main()
