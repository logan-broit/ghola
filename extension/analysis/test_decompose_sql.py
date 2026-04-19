#!/usr/bin/env python3
"""Test whether query decomposition finds answer sessions that full-query FTS misses.

For each multi-event temporal query:
1. Check if full query matches answer session(s) via plainto_tsquery AND conjunction
2. Check if decomposed sub-queries match answer session(s)
3. Report: how many answer sessions are found ONLY via decomposition

This is a direct ground-truth test -- no scoring, no variance, just "does the FTS match?"
"""

import json
import re
import subprocess
import sys

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


def run_sql(query_text):
    """Run SQL query and return stripped output."""
    cmd = [
        "kubectl", "exec", "-n", "ch-system", "memory-db-1", "--",
        "psql", "-U", "postgres", "-d", "memories", "-t", "-A", "-c", query_text
    ]
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    return result.stdout.strip()


def check_fts_match(query_text, session_ids):
    """Check if plainto_tsquery matches any of the given answer sessions."""
    if not session_ids:
        return set()
    escaped = query_text.replace("'", "''")
    sid_list = ",".join(f"'{s}'" for s in session_ids)

    sql = f"""
    SELECT m.session_id::text
    FROM ghola.mnemes m
    JOIN ghola.mnemes tag ON tag.session_id = m.session_id
        AND tag.workspace_id = m.workspace_id
    WHERE m.workspace_id = '00000000-0000-0000-0000-000000000001'
      AND m.state = 'active'
      AND m.search_vector @@ plainto_tsquery('english', '{escaped}')
      AND EXISTS (
          SELECT 1 FROM ghola.mnemes t
          WHERE t.workspace_id = '00000000-0000-0000-0000-000000000001'
            AND t.tags @> ARRAY['bench_00000000']
            AND t.concept LIKE '%' || m.session_id::text || '%'
      )
    """
    # Simpler approach: just check which answer session_ids match
    # The answer_session_ids are the session identifiers used in the benchmark
    # They appear as tags or in the concept field
    # Let's use a more direct approach
    sql = f"""
    SELECT concept
    FROM ghola.mnemes
    WHERE workspace_id = '00000000-0000-0000-0000-000000000001'
      AND state = 'active'
      AND search_vector @@ plainto_tsquery('english', '{escaped}')
    LIMIT 5
    """
    return run_sql(sql)


def main():
    with open(DATA_PATH) as f:
        data = json.load(f)

    temporal = [q for q in data if q["question_type"] == "temporal-reasoning"]

    # First, understand how session_ids map between benchmark and database
    # Check a known answer session
    sample = temporal[0]
    print(f"Sample query: {sample['question'][:80]}...")
    print(f"Answer session IDs: {sample['answer_session_ids']}")

    # Check what mneme concepts look like for answer sessions
    for answer_sid in sample["answer_session_ids"][:2]:
        escaped_sid = answer_sid.replace("'", "''")
        result = run_sql(f"""
            SELECT id::text, concept, left(content, 80)
            FROM ghola.mnemes
            WHERE workspace_id = '00000000-0000-0000-0000-000000000001'
              AND (concept LIKE '%{escaped_sid}%' OR tags @> ARRAY['{escaped_sid}'])
            LIMIT 3
        """)
        if result:
            print(f"\n  Mneme for {answer_sid}:")
            for line in result.split('\n')[:2]:
                print(f"    {line[:150]}")
        else:
            # Try matching on session_id field
            result = run_sql(f"""
                SELECT id::text, concept, session_id::text
                FROM ghola.mnemes
                WHERE workspace_id = '00000000-0000-0000-0000-000000000001'
                  AND session_id::text LIKE '%{escaped_sid}%'
                LIMIT 3
            """)
            if result:
                print(f"\n  Mneme for {answer_sid} (via session_id):")
                for line in result.split('\n')[:2]:
                    print(f"    {line[:150]}")
            else:
                print(f"\n  NOT FOUND: {answer_sid}")

    # Now test FTS matching: does the full query match answer sessions?
    print("\n\n=== FTS Match Test (10 multi-event temporal queries) ===\n")

    full_match_count = 0
    decomp_match_count = 0
    decomp_only_count = 0
    tested = 0

    for q in temporal:
        subs = decompose_query(q["question"])
        if len(subs) < 2:
            continue
        if tested >= 15:
            break
        tested += 1

        question = q["question"]
        answer_sids = q["answer_session_ids"]
        escaped_q = question.replace("'", "''")

        # Find mnemes for answer sessions (use concept field matching)
        answer_mneme_ids = []
        for asid in answer_sids:
            escaped_asid = asid.replace("'", "''")
            result = run_sql(f"""
                SELECT id::text FROM ghola.mnemes
                WHERE workspace_id = '00000000-0000-0000-0000-000000000001'
                  AND concept LIKE '%{escaped_asid}%'
                LIMIT 1
            """)
            if result:
                answer_mneme_ids.append(result.strip())

        if not answer_mneme_ids:
            continue

        id_list = ",".join(f"'{mid}'::uuid" for mid in answer_mneme_ids)

        # Test full query FTS match
        full_match = run_sql(f"""
            SELECT count(*) FROM ghola.mnemes
            WHERE id IN ({id_list})
              AND search_vector @@ plainto_tsquery('english', '{escaped_q}')
        """)

        # Test each sub-query FTS match
        sub_matches = []
        for sq in subs:
            escaped_sq = sq.replace("'", "''")
            sub_match = run_sql(f"""
                SELECT count(*) FROM ghola.mnemes
                WHERE id IN ({id_list})
                  AND search_vector @@ plainto_tsquery('english', '{escaped_sq}')
            """)
            sub_matches.append(int(sub_match or "0"))

        full_hit = int(full_match or "0") > 0
        any_sub_hit = any(m > 0 for m in sub_matches)

        print(f"Q: {question[:100]}")
        print(f"  Subs: {subs}")
        print(f"  Answer mnemes: {len(answer_mneme_ids)}")
        print(f"  Full query FTS match: {full_match}/{len(answer_mneme_ids)}")
        print(f"  Sub-query FTS matches: {sub_matches}")
        if any_sub_hit and not full_hit:
            print(f"  >>> DECOMP ONLY: sub-query finds what full query misses!")
            decomp_only_count += 1
        print()

        if full_hit:
            full_match_count += 1
        if any_sub_hit:
            decomp_match_count += 1

    print(f"\n=== Summary ({tested} multi-event temporal queries) ===")
    print(f"Full query FTS hits: {full_match_count}/{tested}")
    print(f"Any sub-query FTS hits: {decomp_match_count}/{tested}")
    print(f"Decomp-only hits (sub matches, full doesn't): {decomp_only_count}/{tested}")


if __name__ == "__main__":
    main()
