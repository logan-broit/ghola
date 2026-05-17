"""B7: turn the cached extracts into a GitHub-bundle JSONL the Go
``import-logs github`` adapter can ingest.

Wire format is locked — the Go adapter (internal/importlogs/adapters/
github/adapter.go) decodes these records via fixed JSON tags. Any drift
breaks ingest. See `bundleRecord`/`bundleSession`/`bundleEvent` there.

Design highlights:

- Stable UUIDs via uuid5(NS_*, key). Reruns produce identical session
  and event ids, so re-importing a regenerated bundle is idempotent on
  chapterhouse's side.
- Tags emitted as native JSON arrays (not JSON-encoded strings). The Go
  adapter re-encodes them as ``Metadata["tags"]`` when normalizing.
- Bot filter applied at event level: events authored by known bots are
  dropped from output, not silently included with a `bot` tag.
- Orphan threads (no resolving PR) are skipped at bundle-write time.
- Atomic write (tempfile + os.replace) so a Ctrl-C mid-write doesn't
  leave behind a half-flushed JSONL the next run treats as canonical.

Event ordering: chronological by ``created_at``. The design doc doesn't
pin this; we pin it here. Chronological matches how chapterhouse
processes events downstream and keeps the JSONL human-skimmable.
"""
from __future__ import annotations

import json
import logging
import os
import tempfile
import uuid
from collections.abc import Iterable, Iterator
from datetime import datetime, timezone
from pathlib import Path

from .eras import era_for
from .filters import is_bot
from .modules import primary_bucket

logger = logging.getLogger(__name__)

# Stable namespace UUIDs — never change these. They feed uuid5() and any
# rotation would orphan every previously-imported chapterhouse session.
NS_SESSION = uuid.UUID("8e3a6fbb-1a17-4cee-9edd-4b4c5f37c9b2")
NS_EVENT = uuid.UUID("c2d8e1d4-7a30-4f5e-9d1c-3a8b2e6f4d09")

# Body cap for issue/PR free-text. Real corpus has multi-thousand-line
# threads; 8000 is a sensible default that keeps records readable without
# truncating the typical issue. Easy to lift later if eval evidence shows
# we're losing signal.
_BODY_CAP = 8000

# Top-comment body cap inside issue events. Smaller than _BODY_CAP because
# the top comment is supplementary signal — even a representative quote
# preserves the lexical hooks (specific terms, user names) without
# bloating event content. Mirrors the cap in GHClient.issue_top_comment.
_TOP_COMMENT_CAP = 2000

# Merge-commit message cap inside PR events. Merge summaries are usually
# short, but squash-merge with a long commit body can run many KB; cap
# to bound the PR-event size.
_MERGE_MSG_CAP = 2000

# Cap on file-list rendering inside commit content. Past 20 the tail is
# summarized as "... and N more" so commit content stays scannable.
_FILES_CAP = 20


def _parse_iso(ts: str) -> datetime:
    """Parse an ISO-8601 timestamp into an aware UTC datetime.

    The cache stores PyGithub-emitted ISO strings (UTC, microsecond
    precision, no Z suffix — Python's ``isoformat()`` uses ``+00:00``).
    We accept both ``+00:00`` and ``Z`` so this also handles bundles
    re-read from disk after our own writer normalized to ``Z``.
    """
    if ts.endswith("Z"):
        ts = ts[:-1] + "+00:00"
    dt = datetime.fromisoformat(ts)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt


def _to_z(ts: str) -> str:
    """Normalize an ISO-8601 timestamp to ``Z`` suffix, preserving sub-
    second precision when present.

    The Go adapter parses ``time.Time`` from JSON via Go's RFC3339
    decoder — that requires a timezone designator (``Z`` or ``±HH:MM``).
    A naive timestamp (no tz) produces a parse error and the whole
    record is rejected. We harden against that here: naive input is
    treated as UTC and stamped ``Z``. Cache producers that emit naive
    timestamps (e.g., hand-curated test fixtures) round-trip cleanly
    through ingest instead of failing at the Go boundary.

    Sub-second digits round-trip unchanged because we never reformat
    the underlying numeric fields — only swap or append the tz suffix.
    """
    if ts.endswith("Z"):
        return ts
    if ts.endswith("+00:00"):
        return ts[:-6] + "Z"
    # No tz suffix at all → naive. Per the seeding-eval contract all
    # cached timestamps are UTC (PyGithub emits UTC), so coerce to Z.
    # Detect "naive" as: no offset and no Z. We check the last 6 chars
    # for an offset like ±HH:MM; absent that, we treat the input as
    # naive UTC.
    if len(ts) >= 6 and (ts[-6] in {"+", "-"}) and ts[-3] == ":":
        # Has a non-UTC offset; preserve as-is. era_for will still
        # parse it correctly because it's aware.
        return ts
    return ts + "Z"


def _truncate(text: str, cap: int) -> str:
    """Cap free-text body length. Single-byte counting; that's good
    enough for Western text and the cap is large enough that the rare
    multi-byte run won't push us into pathological-length territory."""
    if len(text) <= cap:
        return text
    return text[:cap]


def _stable_event_id(repo: str, kind: str, key: str | int) -> str:
    return str(uuid.uuid5(NS_EVENT, f"{repo}/{kind}/{key}"))


def _stable_session_id(repo: str, issue_number: int) -> str:
    return str(uuid.uuid5(NS_SESSION, f"{repo}#{issue_number}"))


def _build_issue_event(issue: dict, repo: str) -> dict | None:
    user = issue.get("user", "") or ""
    if user and is_bot(user):
        return None
    created_at = _to_z(issue["created_at"])
    title = issue.get("title", "") or ""
    body = _truncate(issue.get("body", "") or "", _BODY_CAP)
    content = f"issue: {title}\n\n{body}".rstrip()

    # Append the top-voted comment as a separated trailing block when
    # present. This is the per-issue signal lift the context-rich-events
    # experiment is testing: a maintainer's "+12" reply often carries the
    # actual diagnosis or repro that the LME embeds toward more strongly
    # than the original "is this broken?" issue body.
    top_comment = issue.get("top_comment")
    if top_comment:
        tc_user = (top_comment.get("user", "") or "").strip()
        tc_body = (top_comment.get("body", "") or "").strip()
        tc_plus_one = top_comment.get("plus_one_count", 0) or 0
        if tc_body:
            tc_body = _truncate(tc_body, _TOP_COMMENT_CAP)
            content = (
                f"{content}\n\n---\n\n"
                f"Top comment by @{tc_user} (+{tc_plus_one}):\n\n{tc_body}"
            )

    tags = [
        f"era:{era_for(_parse_iso(created_at))}",
        f"repo:{repo}",
        "type:issue",
    ]
    if user:
        tags.append(f"author:{user}")
    entities = [user] if user else []
    return {
        "id": _stable_event_id(repo, "issue", issue["number"]),
        "kind": "issue",
        "created_at": created_at,
        "content": content,
        "tags": tags,
        "entities": entities,
    }


def _build_pr_event(
    pr: dict,
    files: list[str],
    repo: str,
    *,
    commits_by_sha: dict[str, dict] | None = None,
) -> dict | None:
    user = pr.get("user", "") or ""
    if user and is_bot(user):
        return None
    # PR uses merged_at as the canonical timestamp: the pipeline only
    # consumes merged-resolving PRs, so merged_at is always set, and it
    # captures the "decision point" the eval cares about.
    merged_at = pr.get("merged_at")
    if not merged_at:
        # Defensive: shouldn't happen on the resolving-PR path, but we
        # don't want to emit a record with an empty timestamp — drop.
        logger.warning("PR #%s has no merged_at; dropping pr event", pr.get("number"))
        return None
    created_at = _to_z(merged_at)

    title = pr.get("title", "") or ""
    body = _truncate(pr.get("body", "") or "", _BODY_CAP)
    content = f"pr: {title}\n\n{body}".rstrip()

    # Append the merge commit message as a trailing block when we can
    # resolve it. The merge commit message often summarizes the change
    # tighter than the PR body — squash-merge in particular folds the
    # final commit message into the PR title + a curated summary the
    # author wrote at merge time. That's high-signal text the LME never
    # otherwise sees inside the PR event.
    merge_sha = pr.get("merge_commit_sha")
    if merge_sha and commits_by_sha:
        merge_commit = commits_by_sha.get(merge_sha)
        if merge_commit:
            merge_msg = (merge_commit.get("message", "") or "").strip()
            if merge_msg:
                merge_msg = _truncate(merge_msg, _MERGE_MSG_CAP)
                content = f"{content}\n\n---\n\nMerge: {merge_msg}"

    tags = [
        f"era:{era_for(_parse_iso(created_at))}",
        f"repo:{repo}",
        "type:pr",
    ]
    if user:
        tags.append(f"author:{user}")

    # Reviewer signal: union of `reviewers` (requested but possibly
    # never reviewed) and `approvers` (definitively approved). Dedup
    # preserves first-seen order, then we emit one tag per reviewer.
    # Bots filtered same as authors — a bot reviewer is no more signal
    # than a bot committer.
    seen: set[str] = set()
    reviewers: list[str] = []
    for r in list(pr.get("reviewers", []) or []) + list(pr.get("approvers", []) or []):
        if not r or r in seen or is_bot(r):
            continue
        seen.add(r)
        reviewers.append(r)
    for r in reviewers:
        tags.append(f"reviewer:{r}")

    if files:
        tags.append(f"module:{primary_bucket(files)}")

    # entities: PR author + all reviewers, deduped, alphabetical for
    # stability across runs.
    entity_set: set[str] = set()
    if user:
        entity_set.add(user)
    entity_set.update(reviewers)
    entities = sorted(entity_set)

    return {
        "id": _stable_event_id(repo, "pr", pr["number"]),
        "kind": "pr",
        "created_at": created_at,
        "content": content,
        "tags": tags,
        "entities": entities,
    }


def _build_commit_event(commit: dict, repo: str) -> dict | None:
    author = commit.get("author", "") or ""
    if author and is_bot(author):
        return None
    authored_at = commit.get("authored_at")
    if not authored_at:
        logger.warning(
            "commit %s has no authored_at; dropping commit event", commit.get("sha")
        )
        return None
    created_at = _to_z(authored_at)

    full_msg = commit.get("message", "") or ""
    short_msg = full_msg.split("\n", 1)[0]
    files = list(commit.get("files", []) or [])
    if len(files) > _FILES_CAP:
        rendered = ", ".join(files[:_FILES_CAP])
        more = len(files) - _FILES_CAP
        files_line = f"files: {rendered} ... and {more} more"
    elif files:
        files_line = f"files: {', '.join(files)}"
    else:
        files_line = "files: (none)"
    content = f"commit: {short_msg}\n\n{full_msg}\n{files_line}".rstrip()

    tags = [
        f"era:{era_for(_parse_iso(created_at))}",
        f"repo:{repo}",
        "type:commit",
    ]
    if author:
        tags.append(f"author:{author}")
    if files:
        tags.append(f"module:{primary_bucket(files)}")

    entities = [author] if author else []

    return {
        "id": _stable_event_id(repo, "commit", commit["sha"]),
        "kind": "commit",
        "created_at": created_at,
        "content": content,
        "tags": tags,
        "entities": entities,
    }


def build_bundle_record(thread: dict, repo: str = "vercel/next.js") -> dict | None:
    """Construct one bundle JSONL record for a fully-extracted issue thread.

    ``thread`` shape (join of B5/B6 caches):

        {
          "issue":   {...},        # from issues.json
          "pr":      {...} | None, # from prs.json (None for orphans)
          "commits": [{...},...],  # from commits.json
          "files":   [...],        # union of files touched across PR commits
        }

    Returns the bundle dict ready for ``json.dumps``. Returns None when:

    - the thread is an orphan (no resolving PR), OR
    - all events would be filtered (e.g., issue + only-bot commits with
      a bot PR author).

    Never returns a record with zero events — the Go adapter rejects that.
    """
    issue = thread["issue"]
    pr = thread.get("pr")
    commits = list(thread.get("commits", []) or [])
    files = list(thread.get("files", []) or [])

    # Orphan: no resolving PR. Drop unconditionally.
    if pr is None:
        return None

    events: list[dict] = []

    issue_ev = _build_issue_event(issue, repo)
    if issue_ev is not None:
        events.append(issue_ev)

    # Build a sha -> commit lookup so the PR event can pull its merge
    # commit message out without a second pass through `commits`.
    commits_by_sha = {c["sha"]: c for c in commits if c.get("sha")}
    pr_ev = _build_pr_event(pr, files, repo, commits_by_sha=commits_by_sha)
    if pr_ev is not None:
        events.append(pr_ev)

    for c in commits:
        cev = _build_commit_event(c, repo)
        if cev is not None:
            events.append(cev)

    if not events:
        return None

    # Chronological ordering pins the on-disk shape so reruns diff cleanly
    # and chapterhouse sees events in causal order.
    events.sort(key=lambda e: e["created_at"])

    issue_number = issue["number"]
    started_at = _to_z(issue["created_at"])

    # ended_at: prefer PR merged_at (the resolution point). Fall back to
    # issue.closed_at, then last event timestamp.
    ended_at: str | None = None
    if pr.get("merged_at"):
        ended_at = _to_z(pr["merged_at"])
    elif issue.get("closed_at"):
        ended_at = _to_z(issue["closed_at"])
    elif events:
        ended_at = events[-1]["created_at"]

    title = issue.get("title", "") or ""
    summary = f"issue: {title}"

    session = {
        "id": _stable_session_id(repo, issue_number),
        "started_at": started_at,
        "ended_at": ended_at,
        "summary": summary,
        "agent_kind": "github",
        "cwd": repo,
        "git_branch": None,
    }

    return {
        "thread_id": f"{repo}#{issue_number}",
        "session": session,
        "events": events,
    }


def build_bundle(
    issues: list[dict],
    links: dict[str, dict],
    prs: dict[str, dict],
    commits: dict[str, dict],
    repo: str = "vercel/next.js",
) -> Iterator[dict]:
    """Stream bundle records from cached extracts, skipping orphans.

    Logs counts of dropped (orphan, all-filtered) threads at INFO so the
    operator gets a quick "how much did we keep" signal without needing
    the full record stream in memory.
    """
    orphan_count = 0
    filtered_count = 0
    emitted = 0

    for issue in issues:
        issue_num = issue["number"]
        link = links.get(str(issue_num))
        if link is None or link.get("pr") is None:
            orphan_count += 1
            continue

        pr_num = link["pr"]
        pr = prs.get(str(pr_num))
        if pr is None:
            # link points at a PR we don't have a record for — treat as
            # orphan. Conservative: don't fabricate a record without
            # source data.
            logger.warning(
                "issue #%s links to PR #%s but PR record is missing",
                issue_num,
                pr_num,
            )
            orphan_count += 1
            continue

        commit_records = [
            commits[sha] for sha in link.get("commits", []) if sha in commits
        ]
        files = list(link.get("files", []) or [])

        thread = {
            "issue": issue,
            "pr": pr,
            "commits": commit_records,
            "files": files,
        }
        rec = build_bundle_record(thread, repo=repo)
        if rec is None:
            filtered_count += 1
            continue

        emitted += 1
        yield rec

    logger.info(
        "build_bundle: emitted=%d orphans=%d filtered=%d",
        emitted,
        orphan_count,
        filtered_count,
    )


def write_bundle(records: Iterable[dict], out_path: Path) -> int:
    """Serialize records to JSONL at ``out_path``. Atomic write via
    tempfile + os.replace; returns the count written.

    The temp file lives in the same directory as ``out_path`` so the
    final rename is a same-filesystem atomic op (POSIX). Compact
    separators (no whitespace) keep file size minimal for large corpora.
    """
    out_path = Path(out_path)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    count = 0
    tmp_path: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            "w",
            dir=out_path.parent,
            delete=False,
            prefix=f".{out_path.stem}-",
            suffix=".jsonl.tmp",
        ) as tmp:
            tmp_path = Path(tmp.name)
            for rec in records:
                tmp.write(json.dumps(rec, separators=(",", ":")))
                tmp.write("\n")
                count += 1
            tmp.flush()
            os.fsync(tmp.fileno())
        tmp_path.replace(out_path)
        tmp_path = None
    finally:
        if tmp_path is not None and tmp_path.exists():
            try:
                tmp_path.unlink()
            except OSError:
                pass
    return count


__all__ = [
    "NS_SESSION",
    "NS_EVENT",
    "build_bundle_record",
    "build_bundle",
    "write_bundle",
]
