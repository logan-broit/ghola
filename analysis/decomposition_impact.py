#!/usr/bin/env python3
"""Analyze decomposition impact on specific queries.

Compares hit rates for multi-event (decomposed) vs single-event queries
within the same benchmark run, across multiple runs.
"""

import json
import re
import sys
from collections import Counter

DATA_PATH = "/home/loganb/longmemeval-ghola/data/longmemeval_s_cleaned.json"

FRAME_WORDS = {
    "how", "many", "what", "which", "when", "where", "who", "did", "does", "do",
    "was", "were", "is", "are", "has", "have", "had",
    "days", "day", "weeks", "week", "months", "month", "years", "year",
    "ago", "passed", "since", "before", "after", "between", "lasted", "spent",
    "long", "recently", "latest", "last", "first", "second", "third",
    "happened", "event", "order", "earlier", "later",
    "the", "a", "an", "my", "i", "me", "and", "or", "of", "in", "on", "at",
    "to", "for", "with", "from", "by", "that", "this", "it", "its",
}


def split_on_connectors(text):
    result = []
    remaining = text
    idx = remaining.find(" between ")
    if idx >= 0:
        before = remaining[:idx].strip()
        remaining = remaining[idx + len(" between "):]
        if before:
            result.append(before)
    for conn in [" and ", " or ", " versus ", " vs "]:
        parts = []
        rest = remaining
        while conn in rest:
            idx = rest.find(conn)
            before = rest[:idx].strip()
            if before:
                parts.append(before)
            rest = rest[idx + len(conn):]
        if parts:
            if rest.strip():
                parts.append(rest.strip())
            result.extend(parts)
            remaining = ""
            break
    if remaining.strip():
        result.append(remaining.strip())
    return result if result else [text.strip()]


def decompose_query(query):
    lower = query.lower()
    segments = []
    for part in re.split(r'[,?]', lower):
        segments.extend(split_on_connectors(part))
    sub_queries = []
    for seg in segments:
        words = re.findall(r'\b\w+\b', seg)
        content = [w for w in words if w not in FRAME_WORDS]
        if len(content) >= 2:
            sub_queries.append(" ".join(content))
    seen = set()
    result = []
    for sq in sub_queries:
        if sq not in seen:
            seen.add(sq)
            result.append(sq)
    return result


def load_results(path):
    """Load benchmark results, return dict of question_id -> (retrieved_sids, ground_truth)."""
    results = {}
    with open(path) as f:
        for line in f:
            entry = json.loads(line)
            qid = entry.get("question_id")
            # Extract session IDs from results
            raw_results = entry.get("results", [])
            session_ids = [r["session_id"] for r in raw_results if "session_id" in r]
            gt = set(entry.get("ground_truth_sessions", []))
            results[qid] = (session_ids, gt)
    return results


def main():
    if len(sys.argv) < 2:
        print("Usage: python decomposition_impact.py <result1.jsonl> [result2.jsonl ...]")
        sys.exit(1)

    with open(DATA_PATH) as f:
        data = json.load(f)

    # Classify queries
    multi_event_qids = set()
    single_event_qids = set()
    for q in data:
        subs = decompose_query(q["question"])
        if len(subs) >= 2:
            multi_event_qids.add(q["question_id"])
        else:
            single_event_qids.add(q["question_id"])

    # Build answer map
    answer_map = {q["question_id"]: set(q["answer_session_ids"]) for q in data}
    category_map = {q["question_id"]: q["question_type"] for q in data}

    print(f"Multi-event queries: {len(multi_event_qids)}")
    print(f"Single-event queries: {len(single_event_qids)}")
    print()

    for result_path in sys.argv[1:]:
        results = load_results(result_path)
        run_name = result_path.split("/")[-1]
        print(f"=== {run_name} ===")

        for category in ["temporal-reasoning", "multi-session", "single-session-assistant",
                         "knowledge-update", "single-session-user", "single-session-preference"]:
            cat_qids = {qid for qid, cat in category_map.items() if cat == category}

            multi_hits_5 = 0
            multi_total = 0
            single_hits_5 = 0
            single_total = 0

            for qid in cat_qids:
                if qid not in results:
                    continue
                retrieved_sids, gt = results[qid]
                top5 = set(retrieved_sids[:5])
                hit = bool(top5 & gt)

                if qid in multi_event_qids:
                    multi_total += 1
                    if hit:
                        multi_hits_5 += 1
                else:
                    single_total += 1
                    if hit:
                        single_hits_5 += 1

            if multi_total > 0 or single_total > 0:
                mr5 = f"{100*multi_hits_5/multi_total:.1f}%" if multi_total > 0 else "n/a"
                sr5 = f"{100*single_hits_5/single_total:.1f}%" if single_total > 0 else "n/a"
                print(f"  {category:30s} multi-event R@5: {mr5:>6s} ({multi_hits_5}/{multi_total})"
                      f"  single-event R@5: {sr5:>6s} ({single_hits_5}/{single_total})")

        print()


if __name__ == "__main__":
    main()
