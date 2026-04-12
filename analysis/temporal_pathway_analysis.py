#!/usr/bin/env python3
"""Analyze temporal-reasoning queries and content_dates coverage.

Prints compact summary of:
1. What temporal cues queries contain (explicit dates, month names, relative time)
2. How many answer sessions have content_dates populated
3. Date overlap between query cues and answer session dates
4. Opportunity sizing for a temporal retrieval pathway
"""

import json
import re
import subprocess
import sys
from collections import Counter

DATA_PATH = "/home/loganb/longmemeval-ghola/data/longmemeval_s_cleaned.json"

MONTH_NAMES = {
    "january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
    "july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
    "jan": 1, "feb": 2, "mar": 3, "apr": 4, "jun": 6,
    "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}
SEASON_NAMES = {"spring", "summer", "fall", "autumn", "winter"}
DAY_NAMES = {"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
RELATIVE_TERMS = {"ago", "before", "after", "between", "last", "first", "recent", "earlier", "later", "prior", "next", "previous"}
TEMPORAL_QUANTIFIERS = {"days", "weeks", "months", "years", "hours"}


def extract_explicit_dates(text):
    """Extract YYYY-MM-DD or YYYY/MM/DD dates from text."""
    pattern = r'\b(\d{4})[/-](\d{2})[/-](\d{2})\b'
    return re.findall(pattern, text)


def classify_temporal_cues(question):
    """Classify what types of temporal cues a question contains."""
    q_lower = question.lower()
    words = set(re.findall(r'\b\w+\b', q_lower))
    cues = set()

    if extract_explicit_dates(question):
        cues.add("explicit_date")

    for m in MONTH_NAMES:
        if m in words:
            cues.add("month_name")
            break

    for s in SEASON_NAMES:
        if s in words:
            cues.add("season")
            break

    for d in DAY_NAMES:
        if d in words:
            cues.add("day_name")
            break

    for r in RELATIVE_TERMS:
        if r in words:
            cues.add("relative_time")
            break

    for t in TEMPORAL_QUANTIFIERS:
        if t in words:
            cues.add("time_unit")
            break

    # Year references (4-digit years in text without full date)
    year_pattern = r'\b(20[12]\d)\b'
    if re.search(year_pattern, question) and "explicit_date" not in cues:
        cues.add("year_only")

    # Ordinal time refs: "first", "second", "third" + event
    if any(w in words for w in {"first", "second", "third", "last", "earliest", "latest", "most"}):
        cues.add("ordinal_time")

    return cues


def parse_haystack_date(date_str):
    """Parse 'YYYY/MM/DD (Day) HH:MM' to YYYY-MM-DD."""
    m = re.match(r'(\d{4})/(\d{2})/(\d{2})', date_str)
    if m:
        return f"{m.group(1)}-{m.group(2)}-{m.group(3)}"
    return None


def query_content_dates():
    """Get content_dates for all mnemes from the database."""
    cmd = [
        "kubectl", "exec", "-n", "ch-system", "memory-db-1", "--",
        "psql", "-U", "postgres", "-d", "memories", "-t", "-A", "-c",
        """SELECT m.session_id::text,
                  array_to_string(m.content_dates, ',') as dates
           FROM ghola.mnemes m
           WHERE m.content_dates IS NOT NULL
             AND array_length(m.content_dates, 1) > 0
             AND m.workspace_id = '00000000-0000-0000-0000-000000000001'"""
    ]
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        print(f"DB query failed: {result.stderr[:200]}", file=sys.stderr)
        return {}

    session_dates = {}
    for line in result.stdout.strip().split('\n'):
        if '|' not in line:
            continue
        parts = line.split('|', 1)
        session_id = parts[0].strip()
        dates_str = parts[1].strip()
        if dates_str:
            # Parse timestamptz values to YYYY-MM-DD
            dates = set()
            for d in dates_str.split(','):
                d = d.strip()
                m = re.match(r'(\d{4}-\d{2}-\d{2})', d)
                if m:
                    dates.add(m.group(1))
            if dates:
                session_dates[session_id] = dates
    return session_dates


def main():
    with open(DATA_PATH) as f:
        data = json.load(f)

    temporal = [q for q in data if q["question_type"] == "temporal-reasoning"]
    print(f"=== Temporal Reasoning Analysis ({len(temporal)} queries) ===\n")

    # 1. Classify temporal cue types
    cue_counts = Counter()
    cue_combos = Counter()
    no_cues = 0
    for q in temporal:
        cues = classify_temporal_cues(q["question"])
        for c in cues:
            cue_counts[c] += 1
        if cues:
            cue_combos[frozenset(cues)] += 1
        else:
            no_cues += 1

    print("1. Temporal cue types in queries:")
    for cue, count in cue_counts.most_common():
        print(f"   {cue:20s} {count:3d}/{len(temporal)} ({100*count/len(temporal):.0f}%)")
    print(f"   {'no_cues':20s} {no_cues:3d}/{len(temporal)}")

    # 2. Answer session dates from haystack_dates
    print("\n2. Answer session date coverage:")
    answers_with_dates = 0
    answers_total = 0
    answer_dates_map = {}  # question_id -> set of answer session dates
    for q in temporal:
        answer_ids = set(q["answer_session_ids"])
        haystack_ids = q["haystack_session_ids"]
        haystack_dates = q["haystack_dates"]

        q_answer_dates = set()
        for sid, dstr in zip(haystack_ids, haystack_dates):
            if sid in answer_ids:
                d = parse_haystack_date(dstr)
                if d:
                    q_answer_dates.add(d)

        answers_total += len(answer_ids)
        if q_answer_dates:
            answers_with_dates += len(answer_ids)
            answer_dates_map[q["question_id"]] = q_answer_dates

    print(f"   Answer sessions with parseable dates: {answers_with_dates}/{answers_total}")
    print(f"   Questions with answer dates: {len(answer_dates_map)}/{len(temporal)}")

    # 3. Check content_dates in DB
    print("\n3. Database content_dates coverage:")
    session_dates = query_content_dates()
    print(f"   Mnemes with content_dates: {len(session_dates)}")

    # 4. Check if answer sessions have content_dates
    # Map answer_session_ids to actual mneme session_ids
    # The benchmark session_ids may differ from DB session_ids
    # Let's check the overlap
    if session_dates:
        # Get all answer session IDs from temporal queries
        all_answer_sids = set()
        for q in temporal:
            all_answer_sids.update(q["answer_session_ids"])

        # Check if any answer session IDs match DB session IDs
        # DB session_ids are UUIDs, benchmark ones are short strings
        print(f"   Answer session ID format samples: {list(all_answer_sids)[:3]}")
        db_sid_samples = list(session_dates.keys())[:3]
        print(f"   DB session ID format samples: {db_sid_samples}")

    # 5. Analyze date range distribution
    print("\n4. Answer date distribution:")
    all_answer_dates = set()
    for dates in answer_dates_map.values():
        all_answer_dates.update(dates)

    if all_answer_dates:
        sorted_dates = sorted(all_answer_dates)
        print(f"   Unique answer dates: {len(sorted_dates)}")
        print(f"   Range: {sorted_dates[0]} to {sorted_dates[-1]}")

        # Month distribution
        month_dist = Counter()
        for d in sorted_dates:
            month_dist[d[:7]] += 1  # YYYY-MM
        print(f"   Months spanned: {len(month_dist)}")
        print(f"   Top months: {month_dist.most_common(5)}")

    # 6. Key question: do queries reference dates that match their answer sessions?
    print("\n5. Query-to-answer date matching potential:")
    queries_with_extractable_dates = 0
    queries_date_matches_answer = 0

    for q in temporal:
        explicit = extract_explicit_dates(q["question"])
        if explicit:
            queries_with_extractable_dates += 1
            # Check if any extracted date matches answer session date
            q_dates = {f"{y}-{m}-{d}" for y, m, d in explicit}
            if q["question_id"] in answer_dates_map:
                if q_dates & answer_dates_map[q["question_id"]]:
                    queries_date_matches_answer += 1

    print(f"   Queries with explicit dates: {queries_with_extractable_dates}/{len(temporal)}")
    print(f"   Of those, date matches answer: {queries_date_matches_answer}/{queries_with_extractable_dates}")

    # 7. Month-name based matching potential
    print("\n6. Month/year cue matching potential:")
    month_matchable = 0
    for q in temporal:
        q_lower = q["question"].lower()
        words = set(re.findall(r'\b\w+\b', q_lower))

        q_months = set()
        for mname, mnum in MONTH_NAMES.items():
            if mname in words:
                q_months.add(mnum)

        q_years = set(re.findall(r'\b(20[12]\d)\b', q["question"]))

        if q_months and q_years and q["question_id"] in answer_dates_map:
            # Check if any answer date has matching month+year
            for ad in answer_dates_map[q["question_id"]]:
                ad_month = int(ad[5:7])
                ad_year = ad[:4]
                if ad_month in q_months and ad_year in q_years:
                    month_matchable += 1
                    break

    print(f"   Queries with month+year cue matching answer: {month_matchable}/{len(temporal)}")

    # 8. What about the question_date field?
    print("\n7. Question date -> answer date proximity:")
    within_window = Counter()
    for q in temporal:
        qd = parse_haystack_date(q["question_date"])
        if not qd or q["question_id"] not in answer_dates_map:
            continue
        from datetime import datetime
        qdt = datetime.strptime(qd, "%Y-%m-%d")
        for ad in answer_dates_map[q["question_id"]]:
            adt = datetime.strptime(ad, "%Y-%m-%d")
            delta = abs((qdt - adt).days)
            if delta <= 7:
                within_window["<=7d"] += 1
            elif delta <= 30:
                within_window["<=30d"] += 1
            elif delta <= 90:
                within_window["<=90d"] += 1
            else:
                within_window[">90d"] += 1
            break  # one per query

    print(f"   Distribution: {dict(within_window)}")

    # Summary
    print("\n=== SUMMARY ===")
    print(f"Total temporal queries: {len(temporal)}")
    print(f"Queries with explicit dates in question text: {queries_with_extractable_dates}")
    print(f"Queries with month+year cues: {month_matchable}")
    print(f"DB mnemes with content_dates: {len(session_dates)}")
    print(f"\nOpportunity: A temporal pathway matching extracted dates against")
    print(f"content_dates can potentially help {queries_with_extractable_dates} queries (explicit)")
    print(f"and {month_matchable} queries (month+year). Many of these currently fail")
    print(f"because semantic/lexical pathways miss the temporal context.")


if __name__ == "__main__":
    main()
