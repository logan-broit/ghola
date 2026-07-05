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
  - ``results-table.tex`` — ``render_latex_table`` for the paper's Table 1;
  - ``fig1-frontier.pdf`` — rate-distortion frontier (F1, Task 5);
  - ``fig2-escalation.pdf`` — transform vs selection at matched budgets (F2, Task 5).

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
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from . import rd_aggregate
from .rd_aggregate import FAMILY

# ---------------------------------------------------------------------------
# Figure family classification
# ---------------------------------------------------------------------------
# FAMILY is defined in rd_aggregate (canonical insertion-order dict used for
# both figure coloring and the paper table's family-grouped sort).
# Re-exported here so existing figure code and tests can still import it as
# ``paper_artifacts.FAMILY``.


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


def _selection_plateau(rows: list[dict[str, Any]]) -> tuple[float, float] | None:
    """Return (center, halfwidth) for the noise band anchored on the extractive
    plateau, or None if there are no eligible rows.

    Eligible rows: selection-family compressors, excluding ``full`` (which is
    the ceiling, not the plateau).  Returns None when the filtered list is
    empty so callers can skip the band entirely.
    """
    plateau_rows = [
        r
        for r in rows
        if FAMILY.get(r.get("compressor", ""), "other") == "selection"
        and r.get("compressor") != "full"
    ]
    if not plateau_rows:
        return None
    best_acc = max(r["accuracy"] for r in plateau_rows)
    # visual approximation - arithmetic mean of per-row CIs, not a pooled SE
    mean_ci = sum(r.get("acc_ci", 0.0) for r in plateau_rows) / len(plateau_rows)
    return (best_acc, mean_ci)


def _humanize_series(row: dict[str, Any]) -> str:
    """Human-readable legend label for a plot series.

    Maps known compressors + settings to readable names; falls back to the
    raw ``_series_key`` for unknown compressors.
    """
    comp = row.get("compressor", "")
    if comp == "extractive_relevance":
        return "extractive (neural)"
    if comp == "extractive_relevance_expanded":
        r = row.get("expansion_reserve")
        if r is not None:
            return f"extractive + expansion (r={r:.1f})"
        return "extractive + expansion"
    if comp == "lexical_relevance":
        return "BM25"
    if comp == "truncate_tokens":
        return "truncate"
    if comp == "topk_sessions":
        return "top-k sessions"
    if comp == "statistical_prune":
        return "IDF prune"
    if comp == "graph_community":
        return "Leiden coverage"
    if comp == "perplexity_prune":
        return "surprisal prune"
    if comp == "llm_distill":
        qmode = row.get("query_mode") or "agnostic"
        dm = row.get("distill_model")
        if dm:
            return f"distill ({dm}, {qmode})"
        return f"distill ({qmode})"
    return rd_aggregate._series_key(row)


def _save_fig(fig: Any, plt: Any, path: Path) -> None:
    """Save *fig* to *path* as PDF, then close it unconditionally.

    ``plt.close(fig)`` is called in the ``finally`` block so figures are
    released even when ``savefig`` raises (prevents matplotlib figure leaks).
    """
    try:
        fig.tight_layout()
        fig.savefig(path, format="pdf")
    finally:
        plt.close(fig)


def _write_frontier_fig(rows: list[dict[str, Any]], path: Path) -> None:
    """Write fig1-frontier.pdf: accuracy (y) vs mean_rate (x, linear scale).

    One series per ``_series_key``; families distinguished by color (selection=C0,
    transform=C1, other=C2); individual series within a family distinguished by
    marker + linestyle. The ``full`` compressor is drawn as a dashed horizontal
    reference line at its accuracy, labeled in the legend with the actual values.
    A noise band spans +/- the pooled ``acc_ci`` around the best selection
    series accuracy.

    Raises on any failure (caller catches and warns).
    """
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    # Group rows by series key.
    by_series: dict[str, list[dict[str, Any]]] = {}
    for r in rows:
        by_series.setdefault(rd_aggregate._series_key(r), []).append(r)

    fig, ax = plt.subplots(figsize=(7, 4.5))

    # Compute noise band: best extractive-plateau accuracy +/- mean acc_ci.
    # full is excluded (ceiling, not plateau) — see _selection_plateau.
    band = _selection_plateau(rows)
    if band is not None:
        best_acc, mean_ci = band
        ax.axhspan(
            best_acc - mean_ci,
            best_acc + mean_ci,
            alpha=0.08,
            color="gray",
            zorder=0,
        )

    # Family colors: one color per family, consistent across series.
    _FAMILY_COLOR = {"selection": "C0", "transform": "C1", "other": "C2"}
    # Per-family marker/linestyle cycling so series within a family are distinct.
    _SELECTION_STYLES: list[dict[str, Any]] = [
        {"marker": "o", "linestyle": "-"},
        {"marker": "^", "linestyle": "-"},
        {"marker": "s", "linestyle": "-"},
        {"marker": "D", "linestyle": "-"},
        {"marker": "v", "linestyle": "--"},
    ]
    _TRANSFORM_STYLES: list[dict[str, Any]] = [
        {"marker": "o", "linestyle": "--", "markerfacecolor": "none"},
        {"marker": "^", "linestyle": "--", "markerfacecolor": "none"},
        {"marker": "D", "linestyle": ":", "markerfacecolor": "none"},
        {"marker": "v", "linestyle": "-.", "markerfacecolor": "none"},
        {"marker": "p", "linestyle": "--", "markerfacecolor": "none"},
    ]
    _OTHER_STYLES: list[dict[str, Any]] = [
        {"marker": "D", "linestyle": ":", "markerfacecolor": "none"},
    ]
    _family_style_idx: dict[str, int] = {"selection": 0, "transform": 0, "other": 0}

    full_acc: float | None = None
    full_rate: float | None = None
    max_non_full_rate: float = 0.0

    for series_key in sorted(by_series):
        pts = sorted(by_series[series_key], key=lambda r: r["mean_rate"])
        comp = pts[0].get("compressor", "")
        family = FAMILY.get(comp, "other")

        xs = [p["mean_rate"] for p in pts]
        ys = [p["accuracy"] for p in pts]

        if comp == "full":
            full_acc = ys[0]
            full_rate = xs[0]
            continue

        # Track max non-full rate for x-axis limit.
        max_non_full_rate = max(max_non_full_rate, max(xs))

        color = _FAMILY_COLOR[family]
        idx = _family_style_idx[family]
        if family == "selection":
            styles = _SELECTION_STYLES
        elif family == "transform":
            styles = _TRANSFORM_STYLES
        else:
            styles = _OTHER_STYLES

        kwargs: dict[str, Any] = dict(styles[idx % len(styles)])

        # Query-aware llm_distill: filled square marker to stand out.
        is_aware = comp == "llm_distill" and pts[0].get("query_mode") == "aware"
        if is_aware:
            kwargs = {"marker": "s", "linestyle": "--"}
        else:
            _family_style_idx[family] = idx + 1

        label = _humanize_series(pts[0])
        ax.plot(xs, ys, color=color, label=label, **kwargs)

    # Set linear x-axis limits: 0 to max non-full rate * 1.08.
    x_max = max_non_full_rate * 1.08 if max_non_full_rate > 0 else 1.0
    ax.set_xlim(0, x_max)

    # Draw the full-context dashed reference line.
    if full_acc is not None and full_rate is not None:
        full_label = (
            f"full context ({full_acc * 100:.1f}% at "
            f"{int(round(full_rate)):,} tok)"
        )
        ax.axhline(
            y=full_acc,
            linestyle="--",
            color="black",
            linewidth=1.2,
            zorder=4,
            label=full_label,
        )

    ax.set_xlabel("context tokens (cl100k)")
    ax.set_ylabel("QA accuracy")
    ax.legend(fontsize=7, loc="lower right")
    _save_fig(fig, plt, path)


def _write_escalation_fig(rows: list[dict[str, Any]], path: Path) -> None:
    """Write fig2-escalation.pdf: transform accuracy vs selection reference at
    matched budgets.

    For each budget present in >= 2 rows: grouped bars of every transform-family
    row, plus a horizontal reference line at extractive_relevance accuracy for
    that budget (if present).

    Raises on any failure (caller catches and warns).
    """
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    import numpy as np

    # Find budgets where >= 2 rows are present.
    # Singleton budgets are skipped: a bar group with a single bar is unreadable.
    budget_counts: Counter[Any] = Counter(r.get("budget") for r in rows)
    eligible_budgets = sorted(
        b for b, cnt in budget_counts.items() if b is not None and cnt >= 2
    )

    if not eligible_budgets:
        # Nothing to plot — write an empty figure rather than error.
        fig, ax = plt.subplots(figsize=(6, 4))
        ax.text(0.5, 0.5, "No eligible budgets", ha="center", va="center", transform=ax.transAxes)
        ax.set_ylabel("QA accuracy")
        _save_fig(fig, plt, path)
        return

    # Collect transform rows and extractive_relevance reference per budget.
    transform_rows_by_budget: dict[Any, list[dict[str, Any]]] = {b: [] for b in eligible_budgets}
    extr_ref: dict[Any, float] = {}

    for r in rows:
        b = r.get("budget")
        if b not in eligible_budgets:
            continue
        comp = r.get("compressor", "")
        if FAMILY.get(comp, "other") == "transform":
            transform_rows_by_budget[b].append(r)
        if comp == "extractive_relevance":
            extr_ref[b] = r["accuracy"]

    # Build bar groups.
    # Each group is a budget. Within each group, one bar per transform series.
    all_series: list[str] = []
    for b in eligible_budgets:
        for r in transform_rows_by_budget[b]:
            sk = rd_aggregate._series_key(r)
            if sk not in all_series:
                all_series.append(sk)
    all_series.sort()

    n_budgets = len(eligible_budgets)
    n_series = max(len(all_series), 1)
    group_width = 0.8
    bar_width = group_width / n_series

    fig, ax = plt.subplots(figsize=(max(5, n_budgets * n_series * 0.8 + 2), 4.5))

    for si, series_key in enumerate(all_series):
        xs = []
        ys = []
        for gi, b in enumerate(eligible_budgets):
            matching = [r for r in transform_rows_by_budget[b] if rd_aggregate._series_key(r) == series_key]
            if not matching:
                continue
            x_center = gi + (si - (n_series - 1) / 2) * bar_width
            xs.append(x_center)
            ys.append(matching[0]["accuracy"])
        ax.bar(xs, ys, width=bar_width * 0.9, label=series_key, zorder=2)

    # Reference lines per budget group.
    for gi, b in enumerate(eligible_budgets):
        if b in extr_ref:
            ref_x_left = gi - group_width / 2
            ref_x_right = gi + group_width / 2
            ax.hlines(
                extr_ref[b],
                ref_x_left,
                ref_x_right,
                colors="black",
                linewidths=1.5,
                linestyles="--",
                zorder=3,
                label=f"extractive_relevance b={b}" if gi == 0 else "_nolegend_",
            )

    ax.set_xticks(range(n_budgets))
    ax.set_xticklabels([str(b) for b in eligible_budgets])
    ax.set_xlabel("budget (context tokens)")
    ax.set_ylabel("QA accuracy")
    ax.legend(fontsize=7)
    _save_fig(fig, plt, path)


def _write_figures(rows: list[dict[str, Any]], dest: Path) -> None:
    """Write both paper figures to ``dest``.

    Called from ``paper_main`` inside a try/except: any error here is a warning,
    not a hard failure.
    """
    _write_frontier_fig(rows, dest / "fig1-frontier.pdf")
    _write_escalation_fig(rows, dest / "fig2-escalation.pdf")


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

    try:
        _write_figures(rows, args.dest)
    except Exception as exc:  # noqa: BLE001
        print(
            f"paper: warning: figure writing failed ({exc}); text artifacts are intact.",
            file=sys.stderr,
        )

    print(f"paper: {len(rows)} rows -> {args.dest}", file=sys.stderr)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(paper_main())
