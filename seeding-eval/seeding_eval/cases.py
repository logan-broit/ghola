"""EvalCase dataclass + deterministic 80/20 held-out split + case builder.

The split is content-derived (sha256 of issue_id) so re-running the
eval against the same corpus yields the same held-out set across
Python versions, processes, and machines. Python's built-in hash() is
randomized per-process and unsuitable here.

build_cases() turns the cached extracts (B5/B6) into EvalCase records.
UUIDs MUST match the bundle writer (B7): the eval compares chapterhouse
recall hits against EvalCase.ground_truth_event_ids, so any drift from
B7's id scheme means every recall miscompares silently. We import the
namespaces from .bundle and reuse the same key strings.
"""
from __future__ import annotations

import hashlib
import logging
import uuid
from dataclasses import dataclass
from datetime import datetime
from typing import TypedDict

from .bundle import NS_EVENT, NS_SESSION
from .eras import era_for
from .modules import primary_bucket

logger = logging.getLogger(__name__)


HELD_OUT_FRACTION_PERCENT = 20  # tunable; default = 20% held-out

# Cap on issue body included in query_text. Deliberately *shorter* than
# bundle._BODY_CAP (8000): the issue body is itself one of the corpus
# events, so a query that includes the full body collapses the
# "find the resolving PR/commit" task into "find the most-similar text"
# — the issue trivially returns itself with cosine ≈ 1.0. Capping at 500
# chars forces recall to do real bridging work from the issue's *gist*
# to the resolving fix.
QUERY_BODY_CAP = 500


def is_held_out(issue_id: str) -> bool:
    """Return True if `issue_id` falls in the held-out 20% bucket.

    Bucketing: int(sha256(id).hexdigest(), 16) % 100. Values in
    [0, HELD_OUT_FRACTION_PERCENT) are held out.
    """
    bucket = int(hashlib.sha256(issue_id.encode()).hexdigest(), 16) % 100
    return bucket < HELD_OUT_FRACTION_PERCENT


@dataclass(frozen=True)
class EvalCase:
    """One eval question — derived from a single issue thread.

    All collection fields are tuples (not lists) so EvalCase is hashable
    and immutable. Per-case state during the eval lives in dicts keyed
    on case_id; never mutate the case itself.
    """
    case_id: str
    issue_id: str
    thread_session_id: str
    query_text: str
    era: str
    ground_truth_event_ids: tuple[str, ...]
    module_path_buckets: tuple[str, ...]
    held_out: bool


class CachedExtracts(TypedDict):
    """Shape of the cache-on-disk after B5+B6.

    Keys mirror the four cache files written by extract.py:
        issues:  list of issue dicts (B5)
        links:   {issue_number_str: {"pr": int|None, "commits": [...], "files": [...]}}
        prs:     {pr_number_str: pr_record}
        commits: {commit_sha: commit_record}
    """
    issues: list[dict]
    links: dict[str, dict]
    prs: dict[str, dict]
    commits: dict[str, dict]


def _parse_iso(ts: str) -> datetime:
    """Parse an ISO-8601 timestamp into an aware UTC datetime.

    Accepts both ``Z`` and ``+00:00`` suffixes — same shape we accept in
    the bundle writer, since both forms appear in the cache (PyGithub
    emits ``+00:00``; hand-curated fixtures often use ``Z``).
    """
    if ts.endswith("Z"):
        ts = ts[:-1] + "+00:00"
    return datetime.fromisoformat(ts)


def build_cases(extracts: CachedExtracts, *, repo: str) -> list[EvalCase]:
    """Construct EvalCase instances from cached extract data.

    For each issue:
      - Skip if orphan (links[issue_num].pr is None) — log + count, don't crash.
      - Resolve ground-truth event IDs:
          * the resolving PR event (uuid5(NS_EVENT, f"{repo}/pr/{pr_num}"))
          * each resolving commit event (uuid5(NS_EVENT, f"{repo}/commit/{sha}"))
        These MUST equal the IDs B7 writes to the bundle, since the eval
        compares recall hits against this set.
      - Resolve module buckets:
          * primary_bucket(files) for the PR (one tuple element)
          * primary_bucket(commit.files) for each commit (one element each)
          * dedupe + stable (first-seen) order
      - Resolve era from issue.created_at via era_for().
      - query_text = title + "\\n\\n" + body[:QUERY_BODY_CAP]
      - held_out = is_held_out(issue_number_str)

    Bot-author filtering happens at the *event* level in the bundle
    writer; cases are about the issue's *content* as a query, so we
    don't re-apply that filter here. An issue authored by a bot can
    still ask a real question.

    Returns the case list in input order. Orphan count is logged at
    INFO so the operator gets a "how many threads survived" signal
    without baking it into the return shape.
    """
    cases: list[EvalCase] = []
    n_orphan = 0

    for issue in extracts["issues"]:
        issue_num = str(issue["number"])
        link = extracts["links"].get(issue_num)
        if not link or link.get("pr") is None:
            n_orphan += 1
            logger.debug("orphan issue dropped: #%s", issue_num)
            continue

        pr_num = str(link["pr"])
        if pr_num not in extracts["prs"]:
            # Link points at a PR we don't have a record for — same
            # conservative treatment as build_bundle: don't fabricate.
            logger.warning(
                "issue #%s links to PR #%s but PR record is missing — dropping",
                issue_num,
                pr_num,
            )
            n_orphan += 1
            continue

        commit_shas = list(link.get("commits", []) or [])

        # Ground-truth event IDs — key strings MUST match bundle._stable_event_id.
        gt_ids: list[str] = [
            str(uuid.uuid5(NS_EVENT, f"{repo}/pr/{pr_num}")),
        ]
        for sha in commit_shas:
            gt_ids.append(str(uuid.uuid5(NS_EVENT, f"{repo}/commit/{sha}")))

        # Module buckets — primary per entity, deduped, first-seen order.
        bucket_seq: list[str] = []
        if link.get("files"):
            bucket_seq.append(primary_bucket(link["files"]))
        for sha in commit_shas:
            commit = extracts["commits"].get(sha)
            if commit and commit.get("files"):
                bucket_seq.append(primary_bucket(commit["files"]))
        seen: set[str] = set()
        deduped_buckets: tuple[str, ...] = tuple(
            b for b in bucket_seq if not (b in seen or seen.add(b))
        )

        # Era from issue created_at — era_for requires aware datetime.
        era = era_for(_parse_iso(issue["created_at"]))

        # query_text = title + body excerpt. body is normalized to "" by
        # B5, so we don't need to defend against None — but guard anyway
        # for hand-curated fixtures.
        body_cap = (issue.get("body") or "")[:QUERY_BODY_CAP]
        query_text = f"{issue['title']}\n\n{body_cap}".rstrip()

        cases.append(
            EvalCase(
                case_id=f"case-{repo}#{issue_num}",
                issue_id=issue_num,
                thread_session_id=str(
                    uuid.uuid5(NS_SESSION, f"{repo}#{issue_num}")
                ),
                query_text=query_text,
                era=era,
                ground_truth_event_ids=tuple(gt_ids),
                module_path_buckets=deduped_buckets,
                held_out=is_held_out(issue_num),
            )
        )

    if n_orphan:
        logger.info(
            "build_cases: dropped %d orphan threads (no resolving PR)",
            n_orphan,
        )
    return cases


__all__ = [
    "HELD_OUT_FRACTION_PERCENT",
    "QUERY_BODY_CAP",
    "EvalCase",
    "CachedExtracts",
    "is_held_out",
    "build_cases",
]
