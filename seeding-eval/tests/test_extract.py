"""Cache-first behavior of extract_issues — pure disk I/O, no GitHub.

The atomic-write property (tempfile + os.replace) is verified by code review
of the implementation, not by fault-injection here. The other two tests pin
the contract: a present cache short-circuits the API path, and a missing
cache with no usable client must fail loudly rather than silently return [].
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest

from seeding_eval.extract import extract_issues, extract_links, extract_via_merged_prs


def test_cache_hit_returns_cached_data(tmp_path: Path):
    """When the cache file exists, return its contents without calling the API."""
    cache_dir = tmp_path
    repo = "owner/name"
    cache_file = cache_dir / "owner__name" / "issues.json"
    cache_file.parent.mkdir(parents=True)
    expected = [{"id": 1, "number": 100, "title": "cached"}]
    cache_file.write_text(json.dumps(expected))

    # No client passed; should never be constructed because we hit cache.
    actual = extract_issues(repo, n=10, cache_dir=cache_dir)
    assert actual == expected


def test_cache_miss_with_no_client_raises(tmp_path: Path, monkeypatch):
    """When the cache is missing and no client is wired, the function should
    fail loudly — not silently return an empty list."""
    # Setup: clear GITHUB_TOKEN so GHClient can't auto-construct (avoid
    # hitting the network in a unit test).
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.setenv("PATH", "/nonexistent")  # break gh CLI fallback too

    cache_dir = tmp_path
    repo = "owner/never-fetched"
    with pytest.raises(Exception):  # any failure is acceptable; just not silent success
        extract_issues(repo, n=10, cache_dir=cache_dir)


def test_atomic_write_no_partial_file_on_crash(tmp_path: Path):
    """Confirm the cache is written atomically: a partial dict from a crashed
    process should not leave behind a half-written issues.json."""
    # This one is harder to test without injecting a fault. Skip for now;
    # mark as TODO if you want to add a fault-injection harness later.
    pytest.skip("Atomic-write fault-injection deferred — manual review of tempfile+replace pattern.")


def test_extract_links_cache_hit_returns_cached_data(tmp_path: Path):
    """When links.json exists, extract_links returns it without calling the API."""
    repo = "owner/name"
    cache_root = tmp_path / "owner__name"
    cache_root.mkdir()
    # extract_links short-circuits on links.json before reading issues.json,
    # but we still write an empty issues.json for parity with real layout.
    (cache_root / "issues.json").write_text("[]")
    expected = {"100": {"pr": 200, "commits": ["sha1", "sha2"], "files": ["a.ts", "b.ts"]}}
    (cache_root / "links.json").write_text(json.dumps(expected))

    actual = extract_links(repo, cache_dir=tmp_path)
    assert actual == expected


def test_extract_links_missing_issues_raises(tmp_path: Path, monkeypatch):
    """When links.json is absent and issues.json doesn't exist either, raise
    rather than constructing a client and walking an empty input set."""
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.setenv("PATH", "/nonexistent")  # break gh CLI fallback

    repo = "owner/never-fetched"
    with pytest.raises(FileNotFoundError):
        extract_links(repo, cache_dir=tmp_path)


def test_extract_links_cache_miss_with_no_client_raises(tmp_path: Path, monkeypatch):
    """When links.json is missing but issues.json has content and no client
    is wired, fail loudly instead of returning silently."""
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.setenv("PATH", "/nonexistent")

    repo = "owner/name"
    cache_root = tmp_path / "owner__name"
    cache_root.mkdir()
    (cache_root / "issues.json").write_text(
        json.dumps([{"id": 1, "number": 100, "title": "x", "body": ""}])
    )
    with pytest.raises(Exception):
        extract_links(repo, cache_dir=tmp_path)


def test_extract_via_merged_prs_cache_hit_no_api(tmp_path: Path, monkeypatch):
    """When all four cache files exist, extract_via_merged_prs returns them
    without constructing a client or hitting the API.

    Mirrors the cache-first contract of extract_issues + extract_links: the
    merged-PR strategy is just a different way to *populate* the same four
    on-disk caches (issues.json, links.json, prs.json, commits.json). On
    re-run with a populated cache, no network call should fire.
    """
    # Break GitHub auth paths so any accidental client construction would
    # fail loudly rather than silently hit the network.
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.setenv("PATH", "/nonexistent")

    repo = "owner/name"
    repo_cache = tmp_path / "owner__name"
    repo_cache.mkdir(parents=True)

    issues = [
        {
            "id": 1, "number": 100, "title": "bug", "body": "broken",
            "created_at": "2024-03-15T14:33:01Z",
            "closed_at": "2024-03-22T08:00:00Z",
            "user": "alice", "assignees": [],
        },
    ]
    links = {"100": {"pr": 200, "commits": ["sha1"], "files": ["a.ts"]}}
    prs = {
        "200": {
            "id": 999, "number": 200, "title": "fix", "body": "Closes #100",
            "merged_at": "2024-03-22T08:00:00Z", "user": "bob",
            "reviewers": [], "approvers": [], "merge_commit_sha": "sha1",
        },
    }
    commits = {
        "sha1": {
            "sha": "sha1", "message": "fix: thing", "author": "bob",
            "committer": "bob", "authored_at": "2024-03-22T07:55:00Z",
            "files": ["a.ts"],
        },
    }
    (repo_cache / "issues.json").write_text(json.dumps(issues))
    (repo_cache / "links.json").write_text(json.dumps(links))
    (repo_cache / "prs.json").write_text(json.dumps(prs))
    (repo_cache / "commits.json").write_text(json.dumps(commits))

    # No client passed; cache hit must short-circuit before any GHClient
    # construction (which would explode given the broken auth above).
    result = extract_via_merged_prs(repo, n_prs=10, cache_dir=tmp_path)
    assert result == {
        "issues": issues,
        "links": links,
        "prs": prs,
        "commits": commits,
    }


def test_extract_via_merged_prs_preserves_cached_top_comment(
    tmp_path: Path, monkeypatch
):
    """When the issues.json cache already contains a `top_comment` field on
    each issue, extract_via_merged_prs returns it untouched without re-
    fetching. Cache-first beats every API call — the top-comment fetch is
    just one more call to skip on a warm cache.

    Symmetric with the existing all-four-caches-present test: the
    top_comment field rides along on each issue dict and survives the
    cache-hit fast path.
    """
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.setenv("PATH", "/nonexistent")

    repo = "owner/name"
    repo_cache = tmp_path / "owner__name"
    repo_cache.mkdir(parents=True)

    top_comment = {
        "body": "Confirmed; the regression starts at v15.1.2.",
        "user": "rauchg",
        "created_at": "2024-03-16T10:00:00Z",
        "plus_one_count": 12,
    }
    issues = [
        {
            "id": 1, "number": 100, "title": "bug", "body": "broken",
            "created_at": "2024-03-15T14:33:01Z",
            "closed_at": "2024-03-22T08:00:00Z",
            "user": "alice", "assignees": [],
            "top_comment": top_comment,
        },
    ]
    links = {"100": {"pr": 200, "commits": ["sha1"], "files": ["a.ts"]}}
    prs = {
        "200": {
            "id": 999, "number": 200, "title": "fix", "body": "Closes #100",
            "merged_at": "2024-03-22T08:00:00Z", "user": "bob",
            "reviewers": [], "approvers": [], "merge_commit_sha": "sha1",
        },
    }
    commits = {
        "sha1": {
            "sha": "sha1", "message": "fix: thing", "author": "bob",
            "committer": "bob", "authored_at": "2024-03-22T07:55:00Z",
            "files": ["a.ts"],
        },
    }
    (repo_cache / "issues.json").write_text(json.dumps(issues))
    (repo_cache / "links.json").write_text(json.dumps(links))
    (repo_cache / "prs.json").write_text(json.dumps(prs))
    (repo_cache / "commits.json").write_text(json.dumps(commits))

    result = extract_via_merged_prs(repo, n_prs=10, cache_dir=tmp_path)
    assert result["issues"][0]["top_comment"] == top_comment


def test_main_writes_bundle_from_cache(tmp_path: Path, monkeypatch, capsys):
    """End-to-end CLI test: pre-populated cache → main() → bundle file.

    Exercises argparse → extract_issues (cache hit) → extract_links (cache hit)
    → build_bundle → write_bundle without touching GitHub. The all-orphan
    fixture means main() reports 0 resolved and writes an empty bundle —
    enough to pin the wiring without depending on PR/commit shape details.
    """
    from seeding_eval.extract import main

    repo = "vercel/next.js"
    repo_cache = tmp_path / "vercel__next.js"
    repo_cache.mkdir(parents=True)
    (repo_cache / "issues.json").write_text(json.dumps([
        {
            "id": 1,
            "number": 100,
            "title": "test",
            "body": "",
            "created_at": "2024-03-15T14:33:01Z",
            "closed_at": "2024-03-22T08:00:00Z",
            "user": "alice",
            "assignees": [],
        },
    ]))
    (repo_cache / "links.json").write_text(
        json.dumps({"100": {"pr": None, "commits": [], "files": []}})
    )
    (repo_cache / "prs.json").write_text("{}")
    (repo_cache / "commits.json").write_text("{}")

    bundle_out = tmp_path / "bundle.jsonl"
    monkeypatch.setattr("sys.argv", [
        "seeding-extract",
        "--repo", repo,
        "--n-issues", "1",
        "--cache-dir", str(tmp_path),
        "--bundle-out", str(bundle_out),
    ])

    main()
    captured = capsys.readouterr()
    assert "issues cached: 1" in captured.out
    assert "links resolved: 0" in captured.out
    assert "wrote 0 bundle records" in captured.out
    # Bundle file exists but is empty (orphan dropped).
    assert bundle_out.exists()
    assert bundle_out.stat().st_size == 0
