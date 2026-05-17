"""B7 bundle writer — record builder + writer contract tests.

Real fixture file, real disk. No mocks. The fixture mirrors the B6 cache
shape: issue + pr + commits + files (union). Tests pin the wire format
the Go adapter (internal/importlogs/adapters/github/adapter.go) parses.
"""
from __future__ import annotations

import json
import uuid
from pathlib import Path

import pytest

from seeding_eval.bundle import (
    NS_EVENT,
    NS_SESSION,
    build_bundle,
    build_bundle_record,
    write_bundle,
)

FIX = Path(__file__).parent / "fixtures"


def _load_fixture() -> dict:
    return json.loads((FIX / "extracted_thread_full.json").read_text())


def test_record_has_required_top_level_keys():
    rec = build_bundle_record(_load_fixture(), repo="vercel/next.js")
    assert set(rec.keys()) == {"thread_id", "session", "events"}


def test_thread_id_format():
    rec = build_bundle_record(_load_fixture())
    assert rec["thread_id"] == "vercel/next.js#100"


def test_session_id_is_stable_uuid5():
    rec = build_bundle_record(_load_fixture())
    expected = str(uuid.uuid5(NS_SESSION, "vercel/next.js#100"))
    assert rec["session"]["id"] == expected


def test_session_started_at_matches_issue_created_at():
    rec = build_bundle_record(_load_fixture())
    assert rec["session"]["started_at"] == "2024-03-15T14:33:01Z"


def test_session_agent_kind_and_cwd():
    rec = build_bundle_record(_load_fixture())
    assert rec["session"]["agent_kind"] == "github"
    assert rec["session"]["cwd"] == "vercel/next.js"
    assert rec["session"]["git_branch"] is None


def test_events_in_chronological_order():
    rec = build_bundle_record(_load_fixture())
    timestamps = [e["created_at"] for e in rec["events"]]
    assert timestamps == sorted(timestamps), (
        f"events must be chronological by created_at, got {timestamps}"
    )


def test_event_ids_are_stable_uuid5():
    rec = build_bundle_record(_load_fixture())
    issue_event = next(e for e in rec["events"] if e["kind"] == "issue")
    expected = str(uuid.uuid5(NS_EVENT, "vercel/next.js/issue/100"))
    assert issue_event["id"] == expected

    pr_event = next(e for e in rec["events"] if e["kind"] == "pr")
    expected_pr = str(uuid.uuid5(NS_EVENT, "vercel/next.js/pr/200"))
    assert pr_event["id"] == expected_pr

    commits = [e for e in rec["events"] if e["kind"] == "commit"]
    shas = ["abc123abc123", "def456def456"]
    actual_commit_ids = {e["id"] for e in commits}
    expected_commit_ids = {
        str(uuid.uuid5(NS_EVENT, f"vercel/next.js/commit/{sha}")) for sha in shas
    }
    assert actual_commit_ids == expected_commit_ids


def test_tags_present_on_all_events():
    rec = build_bundle_record(_load_fixture())
    for ev in rec["events"]:
        assert any(t.startswith("era:") for t in ev["tags"]), (
            f"missing era on {ev['kind']}"
        )
        assert "repo:vercel/next.js" in ev["tags"]
        assert f"type:{ev['kind']}" in ev["tags"]
        assert any(t.startswith("author:") for t in ev["tags"]), (
            f"missing author on {ev['kind']}"
        )


def test_pr_has_reviewers_as_tags():
    rec = build_bundle_record(_load_fixture())
    pr_event = next(e for e in rec["events"] if e["kind"] == "pr")
    assert "reviewer:carol" in pr_event["tags"]
    assert "reviewer:dave" in pr_event["tags"]


def test_pr_module_bucket_from_files():
    rec = build_bundle_record(_load_fixture())
    pr_event = next(e for e in rec["events"] if e["kind"] == "pr")
    # All-files: 2x packages/next/src/client/* (bucket=packages/next/src)
    # and 1x test/integration/hydration/test.ts (bucket=test/integration/hydration)
    # Majority → packages/next/src.
    module_tags = [t for t in pr_event["tags"] if t.startswith("module:")]
    assert module_tags == ["module:packages/next/src"]


def test_entities_dedupe_and_sort():
    rec = build_bundle_record(_load_fixture())
    pr_event = next(e for e in rec["events"] if e["kind"] == "pr")
    # PR author = bob, approvers = [carol, dave]. Dedup, sort alphabetical.
    assert pr_event["entities"] == ["bob", "carol", "dave"]


def test_orphan_thread_returns_none():
    fixture = _load_fixture()
    fixture["pr"] = None
    fixture["commits"] = []
    assert build_bundle_record(fixture) is None


def test_bot_commits_filtered():
    fixture = _load_fixture()
    fixture["commits"][0]["author"] = "dependabot[bot]"
    rec = build_bundle_record(fixture)
    # Commit by bot dropped; PR + issue + 1 non-bot commit remain (3 events)
    assert len(rec["events"]) == 3
    commit_authors = [
        next((t.split(":", 1)[1] for t in e["tags"] if t.startswith("author:")), None)
        for e in rec["events"]
        if e["kind"] == "commit"
    ]
    assert "dependabot[bot]" not in commit_authors


def test_content_includes_issue_body():
    rec = build_bundle_record(_load_fixture())
    issue_event = next(e for e in rec["events"] if e["kind"] == "issue")
    assert "Hydration mismatch on dynamic import" in issue_event["content"]
    assert "Repro:" in issue_event["content"]


def test_content_includes_pr_body():
    rec = build_bundle_record(_load_fixture())
    pr_event = next(e for e in rec["events"] if e["kind"] == "pr")
    assert "Fix hydration mismatch" in pr_event["content"]
    assert "Closes #100" in pr_event["content"]


def test_commit_content_includes_files_list():
    rec = build_bundle_record(_load_fixture())
    commit_events = [e for e in rec["events"] if e["kind"] == "commit"]
    assert any("files:" in e["content"] for e in commit_events)


def test_timestamps_round_trip_iso8601_utc():
    rec = build_bundle_record(_load_fixture())
    assert rec["session"]["started_at"].endswith("Z")
    for ev in rec["events"]:
        ts = ev["created_at"]
        assert ts.endswith("Z") or "+00:00" in ts, f"non-UTC timestamp: {ts}"


def test_bot_pr_reviewer_filtered_from_tags():
    fixture = _load_fixture()
    fixture["pr"]["approvers"] = ["carol", "dependabot[bot]"]
    rec = build_bundle_record(fixture)
    pr_event = next(e for e in rec["events"] if e["kind"] == "pr")
    assert "reviewer:carol" in pr_event["tags"]
    assert "reviewer:dependabot[bot]" not in pr_event["tags"]
    # And entities should also drop the bot
    assert "dependabot[bot]" not in pr_event["entities"]


def test_bot_pr_author_drops_pr_event():
    fixture = _load_fixture()
    fixture["pr"]["user"] = "renovate[bot]"
    rec = build_bundle_record(fixture)
    # PR event dropped; commits remain (still bob)
    kinds = [e["kind"] for e in rec["events"]]
    assert "pr" not in kinds


def test_session_ended_at_matches_pr_merged_at_when_present():
    rec = build_bundle_record(_load_fixture())
    assert rec["session"]["ended_at"] == "2024-03-22T08:00:00Z"


def test_summary_starts_with_issue_title():
    rec = build_bundle_record(_load_fixture())
    assert rec["session"]["summary"].startswith("issue: ")
    assert "Hydration mismatch" in rec["session"]["summary"]


def test_build_bundle_drops_orphans_and_yields_records(tmp_path: Path):
    issues = [
        {
            "id": 1,
            "number": 100,
            "title": "Has PR",
            "body": "x",
            "created_at": "2024-03-15T14:33:01Z",
            "closed_at": "2024-03-22T08:11:44Z",
            "user": "alice",
            "assignees": [],
        },
        {
            "id": 2,
            "number": 101,
            "title": "Orphan",
            "body": "no pr",
            "created_at": "2024-03-15T14:33:01Z",
            "closed_at": None,
            "user": "alice",
            "assignees": [],
        },
    ]
    links = {
        "100": {"pr": 200, "commits": ["abc123abc123"], "files": ["x/y/z.ts"]},
        "101": {"pr": None, "commits": [], "files": []},
    }
    prs = {
        "200": {
            "id": 2200,
            "number": 200,
            "title": "Fix",
            "body": "fix",
            "merged_at": "2024-03-22T08:00:00Z",
            "user": "bob",
            "reviewers": [],
            "approvers": [],
            "merge_commit_sha": "abc123abc123",
        }
    }
    commits = {
        "abc123abc123": {
            "sha": "abc123abc123",
            "message": "Fix",
            "author": "bob",
            "committer": "bob",
            "authored_at": "2024-03-22T08:00:00Z",
            "files": ["x/y/z.ts"],
        }
    }
    out = list(build_bundle(issues, links, prs, commits))
    assert len(out) == 1
    assert out[0]["thread_id"] == "vercel/next.js#100"


def test_naive_timestamp_coerced_to_utc_z():
    """Cached timestamps without a tz suffix (hand-curated fixtures, or
    older cache shapes) must come out the writer as RFC3339 with Z, so
    Go's encoding/json time.Time decoder accepts them."""
    fixture = _load_fixture()
    fixture["issue"]["created_at"] = "2025-01-01T00:00:00"
    rec = build_bundle_record(fixture)
    assert rec["session"]["started_at"] == "2025-01-01T00:00:00Z"
    issue_event = next(e for e in rec["events"] if e["kind"] == "issue")
    assert issue_event["created_at"].endswith("Z")


def test_issue_event_includes_top_comment_when_present():
    """When the cached issue dict has a non-None ``top_comment``, the issue
    event content carries a trailing block with the comment author + body.

    Shape:
        issue: <title>

        <body>

        ---

        Top comment by @<user> (+<N>):

        <comment.body>
    """
    fixture = _load_fixture()
    fixture["issue"]["top_comment"] = {
        "body": "Confirmed; the regression starts at v15.1.2 — bisected to a47.",
        "user": "rauchg",
        "created_at": "2024-03-16T10:00:00Z",
        "plus_one_count": 12,
    }
    rec = build_bundle_record(fixture)
    issue_event = next(e for e in rec["events"] if e["kind"] == "issue")
    content = issue_event["content"]
    assert "\n\n---\n\nTop comment by @rauchg (+12):" in content
    assert "Confirmed; the regression starts at v15.1.2" in content
    # And the original issue body still leads.
    assert content.startswith("issue: Hydration mismatch on dynamic import")


def test_issue_event_omits_top_comment_when_none():
    """When ``top_comment`` is None (or missing), issue content is the
    pre-change shape: ``issue: <title>\\n\\n<body>``. No trailing
    separator block."""
    fixture = _load_fixture()
    fixture["issue"]["top_comment"] = None
    rec = build_bundle_record(fixture)
    issue_event = next(e for e in rec["events"] if e["kind"] == "issue")
    assert "---" not in issue_event["content"]
    assert "Top comment" not in issue_event["content"]


def test_issue_event_omits_top_comment_when_field_absent():
    """A pre-existing fixture without the top_comment key (older cache
    shape) must still produce a valid issue event — same shape as
    top_comment=None."""
    fixture = _load_fixture()
    fixture["issue"].pop("top_comment", None)
    rec = build_bundle_record(fixture)
    issue_event = next(e for e in rec["events"] if e["kind"] == "issue")
    assert "---" not in issue_event["content"]


def test_pr_event_includes_merge_summary_when_present():
    """When the cached commits dict contains the PR's merge_commit_sha,
    the PR event content carries a trailing ``Merge: <message>`` block.

    Shape:
        pr: <title>

        <description>

        ---

        Merge: <merge_commit.message>
    """
    fixture = _load_fixture()
    # The fixture's PR has merge_commit_sha=abc123abc123, and that's the
    # first commit in commits — its message is the merge summary.
    rec = build_bundle_record(fixture)
    pr_event = next(e for e in rec["events"] if e["kind"] == "pr")
    assert "\n\n---\n\nMerge:" in pr_event["content"]
    assert "Fix hydration mismatch (#200)" in pr_event["content"]


def test_pr_event_omits_merge_when_sha_missing():
    """When merge_commit_sha is unset or doesn't resolve, the PR event
    content is the pre-change shape: ``pr: <title>\\n\\n<body>``."""
    fixture = _load_fixture()
    fixture["pr"]["merge_commit_sha"] = None
    rec = build_bundle_record(fixture)
    pr_event = next(e for e in rec["events"] if e["kind"] == "pr")
    assert "---" not in pr_event["content"]
    assert "Merge:" not in pr_event["content"]


def test_event_lengths_increased_with_richer_content():
    """Sanity check: a fixture with both top_comment and a merge commit
    produces strictly longer issue + PR content than the bare-bones case
    (None top_comment, no merge sha). Pins the direction of the change
    against any future accidental regression to the unchunked baseline.
    """
    bare = _load_fixture()
    bare["issue"]["top_comment"] = None
    bare["pr"]["merge_commit_sha"] = None
    bare_rec = build_bundle_record(bare)
    bare_issue_len = len(next(e for e in bare_rec["events"] if e["kind"] == "issue")["content"])
    bare_pr_len = len(next(e for e in bare_rec["events"] if e["kind"] == "pr")["content"])

    rich = _load_fixture()
    rich["issue"]["top_comment"] = {
        "body": "Top comment text — enough to be visibly larger.",
        "user": "rauchg",
        "created_at": "2024-03-16T10:00:00Z",
        "plus_one_count": 12,
    }
    rich_rec = build_bundle_record(rich)
    rich_issue_len = len(next(e for e in rich_rec["events"] if e["kind"] == "issue")["content"])
    rich_pr_len = len(next(e for e in rich_rec["events"] if e["kind"] == "pr")["content"])

    assert rich_issue_len > bare_issue_len, (
        f"top_comment should grow issue content; bare={bare_issue_len} rich={rich_issue_len}"
    )
    assert rich_pr_len > bare_pr_len, (
        f"merge summary should grow pr content; bare={bare_pr_len} rich={rich_pr_len}"
    )


def test_write_bundle_atomic_jsonl(tmp_path: Path):
    rec = build_bundle_record(_load_fixture())
    out_path = tmp_path / "bundle.jsonl"
    n = write_bundle(iter([rec]), out_path)
    assert n == 1
    assert out_path.exists()
    lines = out_path.read_text().splitlines()
    assert len(lines) == 1
    parsed = json.loads(lines[0])
    assert parsed["thread_id"] == rec["thread_id"]
    # No leftover tempfiles in the dir
    leftovers = [p for p in tmp_path.iterdir() if p.name != "bundle.jsonl"]
    assert leftovers == []
