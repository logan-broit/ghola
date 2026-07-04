"""lme-qa-paper: manifest -> merged paper artifacts (Task 4).

Builds leaf trees by hand (reusing test_rd_aggregate's ``_write_leaf``) and
drives ``paper_artifacts`` end-to-end: a JSON manifest of sweep outdirs (each
with optional extra labels + a compressor scope) is loaded, adapted to
``aggregate_many``'s ``(outdir, labels, label_compressors)`` triples, merged
into one row set, and written as the paper's merged-curve.jsonl + .md +
results-table.tex. Figures are Task 5 — not exercised here.

``_write_leaf`` is imported directly from the sibling test module: the repo's
pytest config sets ``pythonpath = ["."]`` and ``tests/`` is a package
(``tests/__init__.py`` exists), so ``from tests.test_rd_aggregate import
_write_leaf`` resolves the same way the existing cross-test helper imports do
(e.g. ``from tests.fake_batches_server import FakeBatchesServer``).
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lme_qa import paper_artifacts
from tests.test_rd_aggregate import _write_leaf


def _write_manifest(path: Path, entries: list[dict]) -> None:
    path.write_text(json.dumps(entries))


def test_load_manifest_roundtrip(tmp_path):
    """A full entry keeps its fields; a minimal entry gets the defaults; a
    tilde in an outdir path is expanded to the home directory."""
    manifest = tmp_path / "manifest.json"
    _write_manifest(
        manifest,
        [
            {
                "outdir": "/abs/path/rd-sweep-out2",
                "labels": {"distill_model": "haiku"},
                "label_compressors": ["llm_distill"],
            },
            {"outdir": "/abs/path/rd-sweep-out4-replicates"},
            {"outdir": "~/longmemeval-ghola/results/rd-sweep"},
        ],
    )
    sources = paper_artifacts.load_manifest(manifest)
    assert len(sources) == 3

    # Full entry: fields carried verbatim.
    full = sources[0]
    assert full.outdir == Path("/abs/path/rd-sweep-out2")
    assert full.labels == {"distill_model": "haiku"}
    assert full.label_compressors == ["llm_distill"]

    # Minimal entry: labels default to {}, label_compressors to None.
    minimal = sources[1]
    assert minimal.outdir == Path("/abs/path/rd-sweep-out4-replicates")
    assert minimal.labels == {}
    assert minimal.label_compressors is None

    # Tilde expansion: the ~ becomes the real home dir.
    expanded = sources[2]
    assert "~" not in str(expanded.outdir)
    assert expanded.outdir == Path("~/longmemeval-ghola/results/rd-sweep").expanduser()


def test_load_manifest_malformed_exits(tmp_path):
    """A manifest that is not a JSON list, and one whose entry lacks the
    required 'outdir' key, both raise SystemExit with a clear message."""
    not_a_list = tmp_path / "not_a_list.json"
    not_a_list.write_text(json.dumps({"outdir": "/x"}))
    with pytest.raises(SystemExit) as ei:
        paper_artifacts.load_manifest(not_a_list)
    assert "list" in str(ei.value)

    missing_outdir = tmp_path / "missing_outdir.json"
    missing_outdir.write_text(json.dumps([{"labels": {"a": "b"}}]))
    with pytest.raises(SystemExit) as ei:
        paper_artifacts.load_manifest(missing_outdir)
    assert "outdir" in str(ei.value)


def test_label_compressors_scopes_labels(tmp_path):
    """One outdir with an llm_distill leaf AND a truncate_tokens leaf, with the
    label scoped to llm_distill: the llm_distill row carries the label, the
    truncate row does not."""
    out = tmp_path / "out"
    _write_leaf(out, "llm_distill", 1000, 0, [("q1", 800, True), ("q2", 820, True)])
    _write_leaf(out, "truncate_tokens", 1000, 0, [("q1", 900, True), ("q2", 920, False)])
    manifest = tmp_path / "manifest.json"
    _write_manifest(
        manifest,
        [
            {
                "outdir": str(out),
                "labels": {"distill_model": "haiku"},
                "label_compressors": ["llm_distill"],
            }
        ],
    )
    sources = paper_artifacts.load_manifest(manifest)
    rows = paper_artifacts.merged_rows(sources)
    by_comp = {r["compressor"]: r for r in rows}
    assert by_comp["llm_distill"]["distill_model"] == "haiku"
    assert "distill_model" not in by_comp["truncate_tokens"]


def test_merged_rows_skips_missing_outdir_with_warning(tmp_path, capsys):
    """One existing + one nonexistent outdir: rows come back for the existing
    one, and a warning naming the missing path is printed to stderr."""
    out = tmp_path / "real"
    _write_leaf(out, "full", None, 0, [("q1", 1000, True), ("q2", 1200, False)])
    missing = tmp_path / "does-not-exist"

    sources = [
        paper_artifacts.Source(outdir=out, labels={}, label_compressors=None),
        paper_artifacts.Source(outdir=missing, labels={}, label_compressors=None),
    ]
    rows = paper_artifacts.merged_rows(sources)
    # Only the real outdir contributed a row.
    assert len(rows) == 1
    assert rows[0]["compressor"] == "full"

    err = capsys.readouterr().err
    assert "skipping" in err
    assert str(missing) in err


def test_paper_main_emits_artifacts(tmp_path):
    """End-to-end: rc 0, all three artifacts exist, the jsonl parses back to the
    same row count, and the tex is a real tabular."""
    out = tmp_path / "out"
    _write_leaf(out, "full", None, 0, [("q1", 2000, True), ("q2", 1800, True)])
    _write_leaf(out, "truncate_tokens", 500, 0, [("q1", 500, True), ("q2", 520, False)])
    _write_leaf(out, "extractive_relevance", 500, 0, [("q1", 510, True), ("q2", 540, True)])

    manifest = tmp_path / "manifest.json"
    _write_manifest(manifest, [{"outdir": str(out)}])
    dest = tmp_path / "paper-artifacts"

    rc = paper_artifacts.paper_main(["--manifest", str(manifest), "--dest", str(dest)])
    assert rc == 0

    jsonl = dest / "merged-curve.jsonl"
    md = dest / "merged-curve.md"
    tex = dest / "results-table.tex"
    assert jsonl.exists() and md.exists() and tex.exists()

    rows = [json.loads(l) for l in jsonl.read_text().splitlines() if l.strip()]
    assert len(rows) == 3  # one row per setting

    assert "\\begin{tabular}" in tex.read_text()
    # No figure artifacts (Task 5) were emitted.
    assert not (dest / "merged-curve.png").exists()


def test_paper_main_all_empty_exits(tmp_path):
    """If EVERY source is empty/missing, paper_main exits nonzero rather than
    writing empty artifacts."""
    manifest = tmp_path / "manifest.json"
    _write_manifest(manifest, [{"outdir": str(tmp_path / "nope")}])
    dest = tmp_path / "dest"
    with pytest.raises(SystemExit):
        paper_artifacts.paper_main(["--manifest", str(manifest), "--dest", str(dest)])
