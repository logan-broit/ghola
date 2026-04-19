#!/usr/bin/env python3
"""Preview query decomposition on the temporal-reasoning category.

Shows how many queries get decomposed, what sub-queries look like,
and estimates the opportunity (how many single-event sub-queries
could match answer sessions that the full AND query misses).
"""

import json
import re
from collections import Counter

DATA_PATH = "/home/loganb/longmemeval-ghola/data/longmemeval_s_cleaned.json"

# Mirror QUERY_FRAME_WORDS from recall.rs
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
    """Mirror split_on_connectors from recall.rs"""
    result = []
    remaining = text

    # Split on " between " first
    idx = remaining.find(" between ")
    if idx >= 0:
        before = remaining[:idx].strip()
        remaining = remaining[idx + len(" between "):]
        if before:
            result.append(before)

    # Split remaining on connectors
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
    """Mirror decompose_query from recall.rs"""
    lower = query.lower()

    # Split on commas, question marks, then connectors
    segments = []
    for part in re.split(r'[,?]', lower):
        segments.extend(split_on_connectors(part))

    sub_queries = []
    for seg in segments:
        words = re.findall(r'\b\w+\b', seg)
        content = [w for w in words if w not in FRAME_WORDS]
        if len(content) >= 2:
            sub_queries.append(" ".join(content))

    # Dedup preserving order
    seen = set()
    result = []
    for sq in sub_queries:
        if sq not in seen:
            seen.add(sq)
            result.append(sq)

    return result


def main():
    with open(DATA_PATH) as f:
        data = json.load(f)

    print("=== Query Decomposition Preview ===\n")

    # Analyze all categories
    categories = {}
    for q in data:
        cat = q["question_type"]
        if cat not in categories:
            categories[cat] = []
        categories[cat].append(q)

    for cat, queries in sorted(categories.items()):
        decomposed_count = 0
        total_sub = 0
        multi_event = 0

        for q in queries:
            subs = decompose_query(q["question"])
            if subs:
                decomposed_count += 1
                total_sub += len(subs)
                if len(subs) >= 2:
                    multi_event += 1

        print(f"{cat} ({len(queries)} queries):")
        print(f"  Decomposed: {decomposed_count}/{len(queries)} ({100*decomposed_count/len(queries):.0f}%)")
        print(f"  Multi-event (2+ subs): {multi_event}/{len(queries)} ({100*multi_event/len(queries):.0f}%)")
        if decomposed_count > 0:
            print(f"  Avg sub-queries: {total_sub/decomposed_count:.1f}")
        print()

    # Deep dive on temporal-reasoning
    temporal = [q for q in data if q["question_type"] == "temporal-reasoning"]
    print("=== Temporal-Reasoning Deep Dive ===\n")

    # Show examples of multi-event decomposition
    print("Examples of multi-event decomposition:")
    shown = 0
    for q in temporal:
        subs = decompose_query(q["question"])
        if len(subs) >= 2 and shown < 5:
            print(f"\n  Q: {q['question'][:120]}")
            for i, s in enumerate(subs):
                print(f"    sub[{i}]: {s}")
            shown += 1

    # Show examples of single-event (no split)
    print(f"\n\nExamples of single-event (not split):")
    shown = 0
    for q in temporal:
        subs = decompose_query(q["question"])
        if len(subs) == 1 and shown < 3:
            print(f"\n  Q: {q['question'][:120]}")
            print(f"    sub: {subs[0]}")
            shown += 1

    # Show examples with no sub-queries
    print(f"\n\nExamples with no sub-queries:")
    shown = 0
    for q in temporal:
        subs = decompose_query(q["question"])
        if len(subs) == 0 and shown < 3:
            print(f"  Q: {q['question'][:120]}")
            shown += 1

    # Measure: how many temporal queries have answer_session_ids with 2+ sessions?
    print(f"\n\n=== Answer Session Distribution ===")
    answer_count = Counter()
    for q in temporal:
        n = len(q["answer_session_ids"])
        answer_count[n] += 1
    print("  Answers per query:", dict(answer_count))

    multi_answer = sum(1 for q in temporal if len(q["answer_session_ids"]) >= 2)
    print(f"  Multi-answer (need 2+ sessions): {multi_answer}/{len(temporal)} ({100*multi_answer/len(temporal):.0f}%)")


if __name__ == "__main__":
    main()
