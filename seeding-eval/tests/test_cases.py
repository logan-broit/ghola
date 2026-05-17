from __future__ import annotations

import json
import uuid
from pathlib import Path

import pytest

from seeding_eval.bundle import NS_EVENT, NS_SESSION
from seeding_eval.cases import EvalCase, build_cases, is_held_out

FIX = Path(__file__).parent / "fixtures"


def _load_extracts():
    return json.loads((FIX / "extracts_c2.json").read_text())


# is_held_out — determinism + ratio


def test_split_determinism_same_id_same_result():
    issue_ids = [str(i) for i in range(100)]
    held1 = {i for i in issue_ids if is_held_out(i)}
    held2 = {i for i in issue_ids if is_held_out(i)}
    assert held1 == held2


def test_split_approximately_20_percent_at_n_10000():
    issue_ids = [str(i) for i in range(10_000)]
    held = sum(1 for i in issue_ids if is_held_out(i))
    # 20% expected; with sha256 distribution + N=10k the empirical SE is
    # ~sqrt(0.2*0.8/10000) * 10000 = 40, so ±200 is comfortably 5σ
    assert 1800 <= held <= 2200, f"got {held}, expected ~2000"


def test_split_stable_across_python_versions():
    # Pin a few known-issue-id results. Compute the expected values from
    # the spec (sha256(id).hexdigest() as int, mod 100, < 20) — these
    # MUST NOT change unless the algorithm changes.
    import hashlib

    for issue_id in ["12345", "1", "99999", "abc"]:
        bucket = int(hashlib.sha256(issue_id.encode()).hexdigest(), 16) % 100
        expected = bucket < 20
        assert is_held_out(issue_id) == expected, (
            f"{issue_id} bucket={bucket} expected_held_out={expected}"
        )


def test_split_input_is_string():
    # is_held_out takes a string. Don't accept ints silently.
    with pytest.raises((TypeError, AttributeError)):
        is_held_out(12345)  # type: ignore[arg-type]


# EvalCase — dataclass shape


def test_evalcase_constructible():
    case = EvalCase(
        case_id="case-vercel/next.js#100",
        issue_id="100",
        thread_session_id="00000000-0000-0000-0000-000000000001",
        query_text="some text",
        era="v15",
        ground_truth_event_ids=("e1", "e2"),
        module_path_buckets=("packages/next/src",),
        held_out=False,
    )
    assert case.issue_id == "100"
    assert case.held_out is False
    assert case.ground_truth_event_ids == ("e1", "e2")


def test_evalcase_frozen():
    case = EvalCase(
        case_id="x",
        issue_id="1",
        thread_session_id="x",
        query_text="x",
        era="v15",
        ground_truth_event_ids=(),
        module_path_buckets=(),
        held_out=False,
    )
    with pytest.raises(Exception):  # FrozenInstanceError, but match generously
        case.issue_id = "2"  # type: ignore[misc]


def test_evalcase_hashable():
    case = EvalCase(
        case_id="x",
        issue_id="1",
        thread_session_id="x",
        query_text="x",
        era="v15",
        ground_truth_event_ids=("e1",),
        module_path_buckets=("m1",),
        held_out=False,
    )
    # Should be hashable (frozen dataclass with all-tuple fields)
    {case}  # raises if not hashable


def test_evalcase_held_out_can_be_set_via_is_held_out():
    """Convenience pattern — held_out flag is computed at case-build time."""
    case = EvalCase(
        case_id="case-vercel/next.js#100",
        issue_id="100",
        thread_session_id="x",
        query_text="x",
        era="v15",
        ground_truth_event_ids=(),
        module_path_buckets=(),
        held_out=is_held_out("100"),
    )
    # Just confirm it composed — don't pin the bool, that's covered above
    assert isinstance(case.held_out, bool)


# build_cases — basic shape


def test_build_cases_drops_orphans():
    extracts = _load_extracts()
    cases = build_cases(extracts, repo="vercel/next.js")
    # 2 threads in fixture, 1 orphan (#300) → 1 case (#100)
    assert len(cases) == 1
    assert cases[0].issue_id == "100"


def test_build_cases_query_text():
    cases = build_cases(_load_extracts(), repo="vercel/next.js")
    # query_text is title + body
    assert "App Router prefetch" in cases[0].query_text
    assert "Repro:" in cases[0].query_text


def test_build_cases_query_text_capped():
    extracts = _load_extracts()
    # Mutate the fixture in-memory: issue body 100KB
    extracts["issues"][0]["body"] = "x" * 100_000
    cases = build_cases(extracts, repo="vercel/next.js")
    # 500-char body cap + title + separator → bounded under ~1000 chars.
    # Deliberately tighter than bundle._BODY_CAP so the query forces recall
    # to bridge issue gist → resolving fix instead of trivially returning
    # the verbatim issue body.
    assert len(cases[0].query_text) < 1000


def test_build_cases_era_from_issue_created_at():
    cases = build_cases(_load_extracts(), repo="vercel/next.js")
    # 2025-02-15 → v15 era
    assert cases[0].era == "v15"


def test_build_cases_ground_truth_includes_pr_and_commits():
    """Ground truth = uuid5(NS_EVENT, f"{repo}/pr/{n}") + commit IDs."""
    cases = build_cases(_load_extracts(), repo="vercel/next.js")
    expected_pr_id = str(uuid.uuid5(NS_EVENT, "vercel/next.js/pr/200"))
    expected_commit_ids = {
        str(uuid.uuid5(NS_EVENT, "vercel/next.js/commit/abc123")),
        str(uuid.uuid5(NS_EVENT, "vercel/next.js/commit/def456")),
    }
    gt = set(cases[0].ground_truth_event_ids)
    assert expected_pr_id in gt
    assert expected_commit_ids.issubset(gt)
    # Total: 1 PR + 2 commits = 3 ground-truth events
    assert len(gt) == 3


def test_build_cases_thread_session_id_matches_bundle():
    """thread_session_id == uuid5(NS_SESSION, f"{repo}#{issue_num}")."""
    cases = build_cases(_load_extracts(), repo="vercel/next.js")
    expected = str(uuid.uuid5(NS_SESSION, "vercel/next.js#100"))
    assert cases[0].thread_session_id == expected


def test_build_cases_module_buckets():
    """primary_bucket of each entity's files, deduped, stable order."""
    cases = build_cases(_load_extracts(), repo="vercel/next.js")
    buckets = cases[0].module_path_buckets
    # PR files: router.ts, cache.ts (both packages/next/src/server),
    #           test.ts (test/integration/prefetch).
    #   bucket_for("packages/next/src/server/x.ts") = "packages/next/src",
    #   bucket_for("test/integration/prefetch/test.ts") = "test/integration/prefetch"
    #   primary = packages/next/src (count 2 vs 1)
    # commit abc123 files: 2x server → "packages/next/src"
    # commit def456 files: test.ts → "test/integration/prefetch"
    # Deduped buckets: {"packages/next/src", "test/integration/prefetch"}
    assert "packages/next/src" in buckets
    assert "test/integration/prefetch" in buckets


def test_build_cases_held_out_flag():
    cases = build_cases(_load_extracts(), repo="vercel/next.js")
    # held_out is whatever is_held_out("100") returns
    assert cases[0].held_out == is_held_out("100")


def test_build_cases_returns_input_order():
    """If the cache has multiple resolved threads, build_cases returns
    them in the same order they appear in extracts['issues']."""
    extracts = _load_extracts()
    # Insert a second resolved thread at the head — minimal data:
    extracts["issues"].insert(0, {
        "id": 5500, "number": 50, "title": "First", "body": "",
        "created_at": "2024-12-01T00:00:00Z", "closed_at": "2024-12-08T00:00:00Z",
        "user": "x", "assignees": [],
    })
    extracts["links"]["50"] = {
        "pr": 51, "commits": ["sha50"], "files": ["packages/next/src/x.ts"],
    }
    extracts["prs"]["51"] = {
        "id": 5510, "number": 51, "title": "Fix", "body": "Closes #50",
        "merged_at": "2024-12-08T00:00:00Z", "user": "x", "reviewers": [],
        "approvers": [], "merge_commit_sha": "sha50",
    }
    extracts["commits"]["sha50"] = {
        "sha": "sha50", "message": "fix", "author": "x", "committer": "x",
        "authored_at": "2024-12-08T00:00:00Z",
        "files": ["packages/next/src/x.ts"],
    }
    cases = build_cases(extracts, repo="vercel/next.js")
    # 2 resolved (50, 100), 1 orphan (300) dropped → input order: [50, 100]
    assert [c.issue_id for c in cases] == ["50", "100"]
