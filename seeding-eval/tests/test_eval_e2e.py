"""End-to-end integration test for the seeding-eval pipeline.

Mirrors the Go-side ``cmd/import-logs/integration_test.go`` pattern:
real fixture data, real binary call, real chapterhouse, real ghola,
no mocks. Skips cleanly when the dev stack isn't reachable so a
default ``pytest`` run on a developer box without ``make dev-up``
doesn't fail.

The integration boundary is ghola + chapterhouse, not the GitHub
API — so the input is a hand-curated cache fixture under
``tests/fixtures/e2e_extracts/`` shaped exactly like what
``extract_via_merged_prs`` writes to disk.

Marker: ``@pytest.mark.slow``. Default ``pytest`` filters this out;
``pytest -m slow`` runs it.
"""
from __future__ import annotations

import json
import os
import subprocess
import uuid
from dataclasses import asdict
from pathlib import Path

import httpx
import pytest

from seeding_eval.bundle import NS_EVENT, build_bundle, write_bundle
from seeding_eval.cases import build_cases
from seeding_eval.eval import _write_outputs, run_eval
from seeding_eval.ghola_client import GholaClient
from seeding_eval.modules import primary_bucket
from seeding_eval.report import RunReport, report_from_json


REPO = "vercel/next.js"
USER_ID = "00000000-0000-0000-0000-000000000001"
DEFAULT_GHOLA = "http://localhost:7421"
DEFAULT_CH = "http://localhost:8080"
DEFAULT_API_KEY = "dev-token"


def _stack_reachable(ghola_url: str, ch_url: str, api_key: str) -> bool:
    """Probe ghola + chapterhouse. True only if both respond.

    Connection errors / timeouts → False (skip-not-fail). 5xx responses
    also flunk the probe — they indicate the service is up but broken,
    which would make the test failure mode ambiguous. 4xx is fine: the
    sentinel workspace gets us a 4xx on validation, which proves the
    HTTP path is live.
    """
    sentinel_ws = "00000000-0000-0000-0000-0000000000ff"
    try:
        with httpx.Client(timeout=2.0) as c:
            r = c.post(
                f"{ghola_url}/v1/recall",
                json={
                    "query_text": "ping",
                    "workspace": sentinel_ws,
                    "user_id": USER_ID,
                    "limit": 1,
                },
            )
            if r.status_code >= 500:
                return False
        with httpx.Client(timeout=2.0) as c:
            r = c.post(
                f"{ch_url}/v1/episodic/query_keyword",
                headers={"Authorization": f"Bearer {api_key}"},
                json={
                    "query_text": "ping",
                    "workspace_id": sentinel_ws,
                    "user_id": USER_ID,
                    "limit": 1,
                },
            )
            if r.status_code >= 500:
                return False
        return True
    except (httpx.ConnectError, httpx.ReadTimeout, httpx.ConnectTimeout):
        return False


def _load_fixture_cache() -> dict:
    """Load the hand-curated cache fixture matching the on-disk shape
    that ``extract_via_merged_prs`` emits."""
    fix = Path(__file__).parent / "fixtures" / "e2e_extracts"
    return {
        "issues":  json.loads((fix / "issues.json").read_text()),
        "links":   json.loads((fix / "links.json").read_text()),
        "prs":     json.loads((fix / "prs.json").read_text()),
        "commits": json.loads((fix / "commits.json").read_text()),
    }


def _build_event_buckets(extracts: dict, repo: str) -> dict[str, str]:
    """Build the event_id → module_bucket map the eval orchestrator
    needs for H1 entropy. Mirrors the helper used in the manual rework
    run (`/tmp/seeding-eval-rework`). Issue + PR share the PR's file
    list; each commit gets its own bucket from the commit's files.
    """
    out: dict[str, str] = {}
    for issue in extracts["issues"]:
        issue_num = str(issue["number"])
        link = extracts["links"].get(issue_num) or {}
        if link.get("pr") is None:
            continue
        if link.get("files"):
            issue_eid = str(uuid.uuid5(NS_EVENT, f"{repo}/issue/{issue_num}"))
            out[issue_eid] = primary_bucket(link["files"])
            pr_eid = str(uuid.uuid5(NS_EVENT, f"{repo}/pr/{link['pr']}"))
            out[pr_eid] = primary_bucket(link["files"])
        for sha in link.get("commits", []):
            commit = extracts["commits"].get(sha) or {}
            if commit.get("files"):
                commit_eid = str(uuid.uuid5(NS_EVENT, f"{repo}/commit/{sha}"))
                out[commit_eid] = primary_bucket(commit["files"])
    return out


@pytest.mark.slow
def test_eval_e2e_full_pipeline(tmp_path: Path) -> None:
    """End-to-end: fixture cache → bundle → ingest → eval → report.

    Skips if the dev stack at localhost:7421 / 8080 is unreachable.
    Requires ``make dev-up``.
    """
    ghola_url = os.environ.get("GHOLA_BASE_URL", DEFAULT_GHOLA)
    ch_url = os.environ.get("CHAPTERHOUSE_URL", DEFAULT_CH)
    api_key = os.environ.get("CHAPTERHOUSE_API_KEY", DEFAULT_API_KEY)

    if not _stack_reachable(ghola_url, ch_url, api_key):
        pytest.skip(f"dev stack unreachable at {ghola_url} / {ch_url}")

    # 1. Bundle from fixture cache
    extracts = _load_fixture_cache()
    bundle_dir = tmp_path / "bundle-dir"
    bundle_dir.mkdir()
    bundle_path = bundle_dir / "bundle.jsonl"
    records = list(
        build_bundle(
            extracts["issues"],
            extracts["links"],
            extracts["prs"],
            extracts["commits"],
            repo=REPO,
        )
    )
    n_written = write_bundle(iter(records), bundle_path)
    assert n_written == 3, f"expected 3 records, got {n_written}"

    # 2. cases.jsonl — written for parity with the manual workflow even
    # though run_eval consumes the in-memory list directly. Future
    # debugging benefits from the artifact being on disk.
    cases = build_cases(extracts, repo=REPO)
    cases_path = tmp_path / "cases.jsonl"
    with cases_path.open("w") as f:
        for c in cases:
            d = asdict(c)
            # tuple → list for JSON
            d["ground_truth_event_ids"] = list(d["ground_truth_event_ids"])
            d["module_path_buckets"] = list(d["module_path_buckets"])
            f.write(json.dumps(d) + "\n")
    assert len(cases) == 3, f"expected 3 cases, got {len(cases)}"

    # 3. event_buckets.json — the H1 entropy lookup table.
    event_buckets = _build_event_buckets(extracts, REPO)
    assert event_buckets, "event_buckets must not be empty"

    # 4. Fresh workspace UUID per run so reruns against the same dev
    # stack don't collide. Chapterhouse upserts by event id, but each
    # run's recall set should be confined to that run's workspace.
    workspace = str(uuid.uuid4())

    # 5. Ingest via the real import-logs binary (subprocess), matching
    # the Go integration-test pattern. ``--resume=false`` means we
    # don't read the resume-state file, so previous runs can't make
    # this idempotency-flaky. Resume-state path still goes to tmp_path
    # to avoid touching the user's default state file.
    repo_root = Path(__file__).resolve().parents[2]
    resume_state = tmp_path / "imported.txt"
    cmd = [
        "go", "run", "./cmd/import-logs",
        f"--source=github:{bundle_dir}",
        f"--workspace={workspace}",
        f"--user={USER_ID}",
        f"--chapterhouse-url={ch_url}",
        f"--resume-state={resume_state}",
        "--resume=false",
    ]
    env = {**os.environ, "CHAPTERHOUSE_API_KEY": api_key}
    result = subprocess.run(
        cmd, cwd=repo_root, env=env, capture_output=True, text=True, timeout=300
    )
    assert result.returncode == 0, (
        f"import-logs failed (rc={result.returncode}):\n"
        f"stdout={result.stdout}\nstderr={result.stderr}"
    )
    # Summary format: "imported=N skipped=N failed=N total=N"
    assert "imported=3" in result.stdout, (
        f"expected imported=3, got: {result.stdout}"
    )
    assert "failed=0" in result.stdout, (
        f"expected failed=0, got: {result.stdout}"
    )

    # 6. Run eval programmatically against the just-ingested workspace.
    with GholaClient(base_url=ghola_url) as ghola:
        report, traces = run_eval(
            cases,
            workspace_id=workspace,
            user_id=USER_ID,
            ghola=ghola,
            event_buckets=event_buckets,
            k=20,
        )

    # 7. Write outputs (replicate the CLI's output behavior).
    out_dir = tmp_path / "results"
    out_dir.mkdir()
    _write_outputs(out_dir, report, traces)

    # 8. Pipeline-level invariants — NOT specific metric values. The
    # rework findings show P@5 depends on corpus size + recall pipeline
    # tuning; the integration test asserts the pipeline ran cleanly,
    # not that it produced good numbers.
    assert isinstance(report, RunReport)
    assert report.n_cases == 3
    assert report.failures == ()
    assert report.h2.n_cases >= 0
    assert report.h1.avg_entropy >= 0.0

    # 9. Output files produced + JSON round-trip.
    report_json_path = out_dir / "report.json"
    report_md_path = out_dir / "report.md"
    traces_path = out_dir / "per-case-traces.jsonl"
    assert report_json_path.exists()
    assert report_md_path.exists()
    assert traces_path.exists()

    rebuilt = report_from_json(report_json_path.read_text())
    assert rebuilt == report, "report.json should round-trip via report_from_json"

    # 10. Per-case traces: one trace per (held-out case, variant) pair.
    n_held_out = sum(1 for c in cases if c.held_out)
    expected_traces = n_held_out * 3  # 3 variants per held-out case
    actual_traces = sum(
        1 for line in traces_path.read_text().splitlines() if line.strip()
    )
    assert actual_traces == expected_traces, (
        f"expected {expected_traces} traces "
        f"({n_held_out} held-out × 3 variants), got {actual_traces}"
    )
