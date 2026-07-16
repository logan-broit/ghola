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

import functools
import json
from pathlib import Path

import pytest

from lme_qa import sweep
from lme_qa.scorer import make_scorer
from tests.fake_claude_bin import call_count, write_fake_claude

from .fake_truthsayer_server import FakeScorerServer


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


def test_sweep_leaf_path_with_expansion_reserve():
    # An explicit expansion_reserve gets a ``__r0.3`` suffix so multiple
    # reserve values at the same budget get distinct leaf directories.
    base = Path("/tmp/x")
    assert sweep.leaf_dir(
        base, "extractive_relevance_expanded", 1000, 0, expansion_reserve=0.3
    ) == base / "extractive_relevance_expanded__b1000__r0.3" / "s0"
    # None (default) gets no suffix — the leaf path matches the old format.
    assert sweep.leaf_dir(
        base, "extractive_relevance_expanded", 1000, 0
    ) == base / "extractive_relevance_expanded__b1000" / "s0"


@pytest.fixture
def reserve_settings_file(tmp_path) -> Path:
    # Three expansion_reserve values at the same budget for the expanded
    # compressor — the minimum to interpret an expanded-vs-extractive comparison
    # without the reserve-mistuning confound.
    p = tmp_path / "reserve_settings.json"
    p.write_text(
        json.dumps(
            [
                {"compressor": "extractive_relevance_expanded", "budget": 50, "expansion_reserve": 0.0},
                {"compressor": "extractive_relevance_expanded", "budget": 50, "expansion_reserve": 0.3},
                {"compressor": "extractive_relevance_expanded", "budget": 50, "expansion_reserve": 0.5},
            ]
        )
    )
    return p


def test_sweep_expansion_reserve_creates_distinct_leaves(
    monkeypatch, dataset_file, run_file, reserve_settings_file, tmp_path
):
    """Different expansion_reserve values at the same budget must produce
    distinct leaf directories so their results don't collide."""
    _use_fake(monkeypatch, tmp_path, {"answer": "yes"})
    outdir = tmp_path / "sweep"
    # extractive_relevance_expanded needs a relevance scorer; sweep_main's
    # --scorer defaults to "truthsayer", a live HTTP reranker normally at
    # :8085. Point the factory at a local fake server instead of the real
    # network endpoint -- this test only cares about leaf-path uniqueness,
    # not reranker quality, and CI has no truthsayer running.
    with FakeScorerServer() as fake_truthsayer:
        monkeypatch.setattr(
            "lme_qa.scorer.make_scorer",
            functools.partial(make_scorer, base_url=fake_truthsayer.url),
        )
        rc = sweep.sweep_main(
            [
                "--dataset", str(dataset_file),
                "--run", str(run_file),
                "--outdir", str(outdir),
                "--settings", str(reserve_settings_file),
                "--samples", "1",
                "--backend", "claude-code",
                "--parallel", "1",
            ]
        )
    assert rc == 0
    # Three distinct leaves at the same budget, distinguished by __r suffix.
    assert (outdir / "extractive_relevance_expanded__b50__r0.0" / "s0" / "judgments.jsonl").exists()
    assert (outdir / "extractive_relevance_expanded__b50__r0.3" / "s0" / "judgments.jsonl").exists()
    assert (outdir / "extractive_relevance_expanded__b50__r0.5" / "s0" / "judgments.jsonl").exists()


def test_load_settings_rejects_invalid_expansion_reserve(tmp_path):
    """expansion_reserve must be in [0.0, 1.0] and a number."""
    p = tmp_path / "bad.json"
    p.write_text(json.dumps(
        [{"compressor": "extractive_relevance_expanded", "budget": 100, "expansion_reserve": 1.5}]
    ))
    with pytest.raises(ValueError, match="expansion_reserve"):
        sweep.load_settings(p)

    p.write_text(json.dumps(
        [{"compressor": "extractive_relevance_expanded", "budget": 100, "expansion_reserve": "high"}]
    ))
    with pytest.raises(ValueError, match="expansion_reserve"):
        sweep.load_settings(p)


def test_load_settings_accepts_missing_expansion_reserve(tmp_path):
    """expansion_reserve defaults to None (the compressor's built-in default)
    when omitted from the settings JSON."""
    p = tmp_path / "ok.json"
    p.write_text(json.dumps(
        [{"compressor": "extractive_relevance_expanded", "budget": 100}]
    ))
    settings = sweep.load_settings(p)
    assert len(settings) == 1
    assert settings[0].expansion_reserve is None


# --- Stage B hyperparameters: leaf_dir uniqueness ----------------------------


def test_leaf_dir_all_new_fields_none_is_legacy_path():
    """Back-compat: a Setting with every new field None produces the SAME path
    as before the Stage B hyperparameters existed (byte-identical leaf names so
    existing done leaves are still skipped on resume)."""
    base = Path("/tmp/x")
    assert sweep.leaf_dir(base, "full", None, 0) == base / "full__bNone" / "s0"
    assert sweep.leaf_dir(base, "truncate_tokens", 512, 3) == (
        base / "truncate_tokens__b512" / "s3"
    )
    # And via the keyword-arg form with all new fields explicitly None.
    assert sweep.leaf_dir(
        base, "statistical_prune", 1000, 0,
        query_mode=None, output_form=None, prune_level=None,
        oracle_model=None, edge_metric=None,
    ) == base / "statistical_prune__b1000" / "s0"


def test_leaf_dir_query_mode_tag():
    base = Path("/tmp/x")
    assert sweep.leaf_dir(
        base, "statistical_prune", 1000, 0, query_mode="agnostic"
    ) == base / "statistical_prune__b1000__qagnostic" / "s0"


def test_leaf_dir_output_form_makes_distinct_paths():
    """THE collision trap: two settings differing ONLY in output_form (at the
    same compressor+budget+query_mode) MUST get different leaf directories or
    their results overwrite each other."""
    base = Path("/tmp/x")
    prose = sweep.leaf_dir(
        base, "llm_distill", 1000, 0, query_mode="agnostic", output_form="prose"
    )
    structured = sweep.leaf_dir(
        base, "llm_distill", 1000, 0, query_mode="agnostic", output_form="structured"
    )
    assert prose != structured
    assert prose == base / "llm_distill__b1000__qagnostic__ofprose" / "s0"
    assert structured == base / "llm_distill__b1000__qagnostic__ofstructured" / "s0"


def test_leaf_dir_oracle_model_sanitized():
    """The oracle model name can contain path-unsafe chars (a HF repo id like
    ``org/model``); the tag must sanitize them so the leaf is a valid single
    directory component."""
    base = Path("/tmp/x")
    leaf = sweep.leaf_dir(
        base, "perplexity_prune", 1000, 0,
        query_mode="agnostic", oracle_model="Qwen/Qwen2.5-1.5B-Instruct",
    )
    name = leaf.parent.name
    assert "/" not in name
    assert "__omQwen_Qwen2.5-1.5B-Instruct" in name


def test_leaf_dir_full_ordering():
    """All new fields set: the tags appear in the fixed order
    budget, reserve, q, of, pl, om, e."""
    base = Path("/tmp/x")
    leaf = sweep.leaf_dir(
        base, "graph_community", 1000, 0,
        expansion_reserve=0.3,
        query_mode="aware",
        output_form="prose",
        prune_level="turn",
        oracle_model="m",
        edge_metric="jaccard",
    )
    assert leaf.parent.name == (
        "graph_community__b1000__r0.3__qaware__ofprose__plturn__omm__ejaccard"
    )


def test_leaf_dir_every_field_changes_the_path():
    """Two Settings differing in ANY single new field must map to different
    leaf dirs (no collisions across the full field set)."""
    base = Path("/tmp/x")
    common = dict(
        expansion_reserve=0.3,
        query_mode="agnostic",
        output_form="prose",
        prune_level="turn",
        oracle_model="m1",
        edge_metric="jaccard",
    )
    baseline = sweep.leaf_dir(base, "c", 1000, 0, **common)
    variants = [
        {**common, "query_mode": "aware"},
        {**common, "output_form": "structured"},
        {**common, "prune_level": "sentence"},
        {**common, "oracle_model": "m2"},
        {**common, "edge_metric": "tfidf"},
        {**common, "expansion_reserve": 0.5},
    ]
    paths = {baseline}
    for v in variants:
        p = sweep.leaf_dir(base, "c", 1000, 0, **v)
        assert p != baseline, f"variant {v} collided with baseline"
        paths.add(p)
    # Every variant produced a distinct path (no two collide with each other).
    assert len(paths) == len(variants) + 1


# --- Stage B hyperparameters: setting.json sidecar ---------------------------


def test_write_setting_sidecar_emits_all_fields(tmp_path):
    """The per-setting sidecar serializes the full Setting (every field) so the
    aggregator can recover the setting without parsing the dir name."""
    setting = sweep.Setting(
        compressor="llm_distill",
        budget=1000,
        query_mode="agnostic",
        output_form="structured",
    )
    setting_dir = tmp_path / "a_setting_dir"
    setting_dir.mkdir()
    sweep._write_setting_sidecar(setting_dir, setting)
    sidecar = setting_dir / "setting.json"
    assert sidecar.exists()
    obj = json.loads(sidecar.read_text())
    assert obj["compressor"] == "llm_distill"
    assert obj["budget"] == 1000
    assert obj["query_mode"] == "agnostic"
    assert obj["output_form"] == "structured"
    # Unset new fields are present as null.
    assert obj["edge_metric"] is None
    assert obj["oracle_model"] is None


# --- Stage B hyperparameters: load_settings validation -----------------------


def test_load_settings_parses_new_fields(tmp_path):
    p = tmp_path / "ok.json"
    p.write_text(json.dumps([
        {
            "compressor": "llm_distill",
            "budget": 1000,
            "query_mode": "aware",
            "output_form": "structured",
        },
        {
            "compressor": "perplexity_prune",
            "budget": 500,
            "query_mode": "agnostic",
            "oracle_model": "Qwen/Qwen2.5-1.5B-Instruct",
        },
        {
            "compressor": "graph_community",
            "budget": 800,
            "edge_metric": "jaccard",
            "prune_level": "turn",
        },
    ]))
    settings = sweep.load_settings(p)
    assert settings[0].query_mode == "aware"
    assert settings[0].output_form == "structured"
    assert settings[1].query_mode == "agnostic"
    assert settings[1].oracle_model == "Qwen/Qwen2.5-1.5B-Instruct"
    assert settings[2].edge_metric == "jaccard"
    assert settings[2].prune_level == "turn"
    # Unspecified new fields stay None.
    assert settings[0].oracle_model is None
    assert settings[1].output_form is None


def test_load_settings_rejects_bad_query_mode(tmp_path):
    p = tmp_path / "bad.json"
    p.write_text(json.dumps(
        [{"compressor": "statistical_prune", "budget": 100, "query_mode": "sideways"}]
    ))
    with pytest.raises(ValueError, match="query_mode"):
        sweep.load_settings(p)


def test_load_settings_rejects_bad_output_form(tmp_path):
    p = tmp_path / "bad.json"
    p.write_text(json.dumps(
        [{"compressor": "llm_distill", "budget": 100, "output_form": "haiku"}]
    ))
    with pytest.raises(ValueError, match="output_form"):
        sweep.load_settings(p)


def test_load_settings_rejects_bad_edge_metric(tmp_path):
    p = tmp_path / "bad.json"
    p.write_text(json.dumps(
        [{"compressor": "graph_community", "budget": 100, "edge_metric": "cosine"}]
    ))
    with pytest.raises(ValueError, match="edge_metric"):
        sweep.load_settings(p)


def test_load_settings_rejects_bad_prune_level(tmp_path):
    p = tmp_path / "bad.json"
    p.write_text(json.dumps(
        [{"compressor": "statistical_prune", "budget": 100, "prune_level": "paragraph"}]
    ))
    with pytest.raises(ValueError, match="prune_level"):
        sweep.load_settings(p)


def test_load_settings_rejects_empty_oracle_model(tmp_path):
    p = tmp_path / "bad.json"
    p.write_text(json.dumps(
        [{"compressor": "perplexity_prune", "budget": 100, "oracle_model": ""}]
    ))
    with pytest.raises(ValueError, match="oracle_model"):
        sweep.load_settings(p)


def test_load_settings_validation_names_the_index(tmp_path):
    """The ValueError must name the offending entry's index (mirrors the
    expansion_reserve validation style)."""
    p = tmp_path / "bad.json"
    p.write_text(json.dumps([
        {"compressor": "full"},
        {"compressor": "statistical_prune", "budget": 100, "query_mode": "nope"},
    ]))
    with pytest.raises(ValueError, match="index 1"):
        sweep.load_settings(p)


def test_sweep_writes_setting_sidecar_per_setting(
    monkeypatch, dataset_file, run_file, settings_file, tmp_path
):
    """After a real sweep, each setting dir holds a setting.json sidecar with
    the full Setting (driven through the fake claude binary — no live model)."""
    _use_fake(monkeypatch, tmp_path, {"answer": "yes"})
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
    for setting_name in ("full__bNone", "truncate_tokens__b50"):
        sidecar = outdir / setting_name / "setting.json"
        assert sidecar.exists(), f"missing sidecar for {setting_name}"
        obj = json.loads(sidecar.read_text())
        assert "compressor" in obj and "budget" in obj
        # New fields present (null for these legacy compressors).
        assert "query_mode" in obj and "output_form" in obj
