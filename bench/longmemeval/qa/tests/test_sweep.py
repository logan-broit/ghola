"""Sweep driver (Task 7): the (compressor, budget, sample) grid + leaf resume.

Driven through the fake claude binary (LME_QA_CLAUDE_BIN) so NO live model is
hit — each leaf is a real reader+judge invocation over the fake. Pins:

  - the grid materializes one leaf dir per (compressor, budget, sample) at
    ``<outdir>/<compressor>__b<budget>/s<i>/{answers,judgments}.jsonl``;
  - a second invocation with all leaves already complete runs ZERO claude calls
    (leaf-level resume — a leaf whose judgments has every question succeeded is
    skipped wholesale);
  - the reader prompts carry the leaf's compressor/budget (provenance round-trips
    into the answers rows).
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lme_qa import sweep
from tests.fake_claude_bin import call_count, write_fake_claude


@pytest.fixture
def dataset_file(tmp_path, answerable_entry, abstention_entry) -> Path:
    p = tmp_path / "dataset.json"
    p.write_text(json.dumps([answerable_entry, abstention_entry]))
    return p


@pytest.fixture
def run_file(tmp_path, answerable_result_line, abstention_result_line) -> Path:
    p = tmp_path / "run.jsonl"
    p.write_text(
        json.dumps(answerable_result_line) + "\n" + json.dumps(abstention_result_line) + "\n"
    )
    return p


@pytest.fixture
def settings_file(tmp_path) -> Path:
    # Two compressors x one budget each = two settings. truncate_tokens has a
    # budget; full ignores it (budget null).
    p = tmp_path / "settings.json"
    p.write_text(
        json.dumps(
            [
                {"compressor": "full", "budget": None},
                {"compressor": "truncate_tokens", "budget": 50},
            ]
        )
    )
    return p


def _use_fake(monkeypatch, tmp_path, scenario) -> Path:
    bindir = tmp_path / "bin"
    binpath = write_fake_claude(bindir, scenario)
    monkeypatch.setenv("LME_QA_CLAUDE_BIN", str(binpath))
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    return bindir


def test_sweep_builds_grid_and_resumes(
    monkeypatch, dataset_file, run_file, settings_file, tmp_path
):
    # Fake always answers "yes" so every judged question succeeds (a leaf
    # reaches completion -> resume can skip it on the 2nd pass).
    bindir = _use_fake(monkeypatch, tmp_path, {"answer": "yes"})
    outdir = tmp_path / "sweep"

    rc = sweep.sweep_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--outdir", str(outdir),
            "--settings", str(settings_file),
            "--samples", "1",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0

    # One leaf dir per (compressor, budget, sample=0).
    full_leaf = outdir / "full__bNone" / "s0"
    trunc_leaf = outdir / "truncate_tokens__b50" / "s0"
    for leaf in (full_leaf, trunc_leaf):
        assert (leaf / "answers.jsonl").exists()
        assert (leaf / "judgments.jsonl").exists()

    # Answers in the truncate leaf carry the right setting (provenance).
    arows = [
        json.loads(l)
        for l in (trunc_leaf / "answers.jsonl").read_text().splitlines()
        if l.strip()
    ]
    assert arows and all(r["compressor"] == "truncate_tokens" for r in arows)
    assert all(r["budget"] == 50 for r in arows)

    # 2 settings x 2 questions x (reader + judge) = 8 claude calls on the first
    # pass.
    first_pass_calls = call_count(bindir)
    assert first_pass_calls == 8

    # --- leaf-level resume: a 2nd invocation with all leaves complete runs ZERO
    # new claude calls. Fresh fake binary (resets the counter) so any call shows.
    bindir2 = _use_fake(monkeypatch, tmp_path, {"answer": "yes"})
    rc = sweep.sweep_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--outdir", str(outdir),
            "--settings", str(settings_file),
            "--samples", "1",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0
    assert call_count(bindir2) == 0, "complete leaves must be skipped (zero claude calls)"


def test_sweep_samples_make_independent_leaves(
    monkeypatch, dataset_file, run_file, settings_file, tmp_path
):
    _use_fake(monkeypatch, tmp_path, {"answer": "yes"})
    outdir = tmp_path / "sweep"
    rc = sweep.sweep_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--outdir", str(outdir),
            "--settings", str(settings_file),
            "--samples", "2",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0
    # Each setting gets s0 and s1 leaves.
    for setting in ("full__bNone", "truncate_tokens__b50"):
        assert (outdir / setting / "s0" / "judgments.jsonl").exists()
        assert (outdir / setting / "s1" / "judgments.jsonl").exists()


def test_sweep_leaf_path_layout():
    # The leaf path encodes the setting + sample so the aggregator (Task 8) can
    # walk <outdir>/<compressor>__b<budget>/s<i>/.
    base = Path("/tmp/x")
    assert sweep.leaf_dir(base, "full", None, 0) == base / "full__bNone" / "s0"
    assert sweep.leaf_dir(base, "truncate_tokens", 512, 3) == (
        base / "truncate_tokens__b512" / "s3"
    )
