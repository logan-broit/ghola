"""lme-qa-paper: merge a manifest of sweep outdirs into the paper's artifacts.

A measurement campaign traces the rate-distortion frontier across SEVERAL sweep
runs — replicate outdirs that add samples to the same settings, and separate
outdirs whose only distinguishing feature is a distiller model (the leaf name
does not encode it). ``lme-qa-rd`` aggregates ONE outdir; this command reads a
manifest naming all of them (each with optional extra labels + a compressor
scope), pools them via ``rd_aggregate.aggregate_many``, and writes the paper's
merged artifacts to a single ``--dest`` directory:

  - ``merged-curve.jsonl`` — the machine-readable merged frontier (one row per
    line, ``json.dumps``);
  - ``merged-curve.md`` — ``render_markdown`` of the merged rows;
  - ``results-table.tex`` — ``render_latex_table`` for the paper's Table 1.

The figure (Figure 1) is Task 5 — not written here.

Manifest format — a JSON list of objects::

    [
      {"outdir": "/abs/path/rd-sweep-out2",
       "labels": {"distill_model": "haiku"},
       "label_compressors": ["llm_distill"]},
      {"outdir": "/abs/path/rd-sweep-out4-replicates"}
    ]

``labels`` defaults to ``{}`` (no extra label); ``label_compressors`` defaults
to ``None`` (labels apply to every compressor). Tilde in outdir paths is
expanded. Malformed manifests fail with a clear ``SystemExit`` (mirroring
``sweep.load_settings``'s per-index error style) before any aggregation.

Resilience: a source whose outdir does not exist, or exists but contributes no
leaves, is skipped with a one-line stderr warning and the run continues — a
partially-landed measurement weekend still produces artifacts from whatever did
land. Only if EVERY source is empty does the command error out.
"""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from . import rd_aggregate


@dataclass
class Source:
    """One manifest entry: a sweep outdir plus the extra labels to stamp onto
    its rows and the compressors those labels apply to.

    ``label_compressors is None`` means the labels apply to every compressor;
    an empty ``labels`` dict is a no-op source (just its leaves, unlabelled).
    Maps directly to ``rd_aggregate.aggregate_many``'s
    ``(outdir, labels, label_compressors)`` triple.
    """

    outdir: Path
    labels: dict[str, str] = field(default_factory=dict)
    label_compressors: list[str] | None = None


def load_manifest(path: Path) -> list[Source]:
    """Parse + validate the manifest, returning one ``Source`` per entry.

    Validation mirrors ``sweep.load_settings``: a top-level JSON list of
    objects, each with a required string ``outdir``, an optional ``labels``
    object of string->string, and an optional ``label_compressors`` list of
    strings. Any violation raises ``SystemExit`` naming the offending entry
    index and the problem, so a malformed manifest fails before any aggregation
    (never a half-written artifact set). ``~`` in outdir paths is expanded.
    """
    try:
        data = json.loads(path.read_text())
    except OSError as exc:
        raise SystemExit(f"error: cannot read manifest {path}: {exc}")
    except json.JSONDecodeError as exc:
        raise SystemExit(f"error: manifest {path} is not valid JSON: {exc}")

    if not isinstance(data, list):
        raise SystemExit(f"error: manifest {path} must be a JSON list of objects")

    sources: list[Source] = []
    for i, item in enumerate(data):
        if not isinstance(item, dict):
            raise SystemExit(
                f"error: manifest {path} entry at index {i} must be an object"
            )
        outdir = item.get("outdir")
        if not isinstance(outdir, str) or not outdir:
            raise SystemExit(
                f"error: manifest {path} entry at index {i} must have a "
                f"non-empty string 'outdir'"
            )

        labels = item.get("labels", {})
        if not isinstance(labels, dict) or not all(
            isinstance(k, str) and isinstance(v, str) for k, v in labels.items()
        ):
            raise SystemExit(
                f"error: manifest {path} entry at index {i} 'labels' must be an "
                f"object mapping strings to strings, got {labels!r}"
            )

        label_compressors = item.get("label_compressors")
        if label_compressors is not None:
            if not isinstance(label_compressors, list) or not all(
                isinstance(c, str) for c in label_compressors
            ):
                raise SystemExit(
                    f"error: manifest {path} entry at index {i} "
                    f"'label_compressors' must be a list of strings or null, "
                    f"got {label_compressors!r}"
                )

        sources.append(
            Source(
                outdir=Path(outdir).expanduser(),
                labels=dict(labels),
                label_compressors=label_compressors,
            )
        )
    return sources


def merged_rows(sources: list[Source]) -> list[dict[str, Any]]:
    """Aggregate all sources into the merged frontier via ``aggregate_many``.

    Thin adapter: each ``Source`` becomes an ``(outdir, labels,
    label_compressors)`` triple. Before aggregating, sources that cannot
    contribute are dropped with a one-line stderr warning so a partial campaign
    still yields artifacts:

      - a missing outdir: ``paper: skipping <outdir>: not found``;
      - an outdir with no aggregatable leaves: ``paper: skipping <outdir>: no
        leaves``.

    Emptiness is decided by aggregating each source in isolation (an outdir may
    hold only stray dirs, or only leaves that fail to parse) — cheap for the
    handful of sources a manifest names, and it lets the per-source warning name
    the exact directory. If NO source contributes any row, raises ``SystemExit``
    (there is nothing to write).
    """
    usable: list[tuple[Path, dict[str, str], list[str] | None]] = []
    for src in sources:
        if not src.outdir.exists():
            print(f"paper: skipping {src.outdir}: not found", file=sys.stderr)
            continue
        # Probe this source alone: does it yield any row on its own?
        probe = rd_aggregate.aggregate_many(
            [(src.outdir, src.labels, src.label_compressors)]
        )
        if not probe:
            print(f"paper: skipping {src.outdir}: no leaves", file=sys.stderr)
            continue
        usable.append((src.outdir, src.labels, src.label_compressors))

    if not usable:
        raise SystemExit(
            "error: no aggregatable leaves found in any manifest source "
            "(every outdir was missing or empty)"
        )

    # Aggregate all usable sources together so replicate outdirs pool and
    # same-label settings merge (the whole point of aggregate_many).
    return rd_aggregate.aggregate_many(usable)


def paper_main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="lme-qa-paper",
        description="Merge a manifest of sweep outdirs into the paper's merged "
        "artifacts (merged-curve.jsonl + merged-curve.md + results-table.tex).",
    )
    ap.add_argument(
        "--manifest",
        required=True,
        type=Path,
        help="JSON list of {outdir, labels?, label_compressors?} sources",
    )
    ap.add_argument(
        "--dest",
        required=True,
        type=Path,
        help="output directory for the merged artifacts (created if absent)",
    )
    args = ap.parse_args(argv)

    sources = load_manifest(args.manifest)
    rows = merged_rows(sources)  # SystemExit if all sources are empty.

    args.dest.mkdir(parents=True, exist_ok=True)

    jsonl_path = args.dest / "merged-curve.jsonl"
    with jsonl_path.open("w") as fh:
        for r in rows:
            fh.write(json.dumps(r) + "\n")

    md_path = args.dest / "merged-curve.md"
    md_path.write_text(rd_aggregate.render_markdown(rows))

    tex_path = args.dest / "results-table.tex"
    tex_path.write_text(rd_aggregate.render_latex_table(rows))

    print(f"paper: {len(rows)} rows -> {args.dest}", file=sys.stderr)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(paper_main())
