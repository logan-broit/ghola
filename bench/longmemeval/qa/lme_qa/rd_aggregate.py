"""lme-qa-rd: aggregate a sweep's leaves into the rate-distortion frontier.

Joins, per setting, the emitted-context token count (rate) to the judge's
verdict (distortion):

  - rate  = answers ``context_tokens`` (the emitted-context token count the
            reader measured at build time with its rate tokenizer — the memory
            PAYLOAD we emit). This is explicitly NOT ``usage.input_tokens``: on
            the claude-code backend that field is Claude Code's fixed harness
            overhead (~3279 tokens regardless of payload), so it reported the
            same rate for every setting (the bug this aggregator's rate axis
            once had). A pre-fix run with no ``context_tokens`` falls back to
            ``usage.input_tokens`` with a stderr warning naming the setting.
  - label = judgments ``label`` (True == judged correct).

The join is per ``question_id`` within a leaf. Within a setting, samples are
averaged per question first (so a noisy question does not get extra weight from
its sample count), then questions are averaged to the setting's mean rate +
accuracy. ``distortion == 1 - accuracy`` by construction.

Emits three artifacts under ``--outdir``:

  - ``rd-curve.jsonl`` — one row per setting (the machine-readable frontier);
  - ``rd-curve.md`` — a table sorted by mean rate, with the truncate-vs-
    extractive accuracy gap at the nearest shared budget called out (the whole
    point of the baseline frontier: does relevance-aware selection beat a blind
    byte cut at the same budget?);
  - ``rd-curve.png`` — best-effort (accuracy vs mean_rate, one line per
    compressor). matplotlib is an OPTIONAL ``[plot]`` extra; if it is not
    installed the PNG is skipped with a stderr note, never an error.

Both files reuse the reader's append-only dedup (last-wins per qid) so a
failed-then-succeeded qid within a leaf counts once, at its succeeded values.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from . import cli

# 95% normal-approximation multiplier for the reported CI half-widths. The CIs
# are simple std-err-based (Wald for accuracy, SEM for rate) — a rough spread
# indicator on a small eval set, not an exact interval.
_Z95 = 1.96


@dataclass(frozen=True)
class _LeafSetting:
    """A setting recovered from a leaf directory name."""

    compressor: str
    budget: int | None


def _parse_setting_dir(name: str) -> _LeafSetting | None:
    """Recover ``(compressor, budget)`` from a ``<compressor>__b<budget>`` dir.

    The compressor name itself contains underscores (``truncate_tokens``,
    ``extractive_relevance``), so split on the LAST ``__b`` separator. ``bNone``
    -> ``None`` budget. Returns ``None`` for a name that does not match the
    pattern (a stray directory under outdir is ignored, not crashed on).
    """
    sep = "__b"
    cut = name.rfind(sep)
    if cut == -1:
        return None
    compressor = name[:cut]
    btag = name[cut + len(sep):]
    if not compressor or not btag:
        return None
    if btag == "None":
        budget: int | None = None
    else:
        try:
            budget = int(btag)
        except ValueError:
            return None
    return _LeafSetting(compressor=compressor, budget=budget)


def _rate_by_qid(answers: Path, setting_label: str) -> tuple[dict[str, float], str | None]:
    """question_id -> rate, last-wins per qid; plus the rate tokenizer name.

    Rate = the row's ``context_tokens`` (the emitted-context token count the
    reader measured). A row missing ``context_tokens`` (a pre-fix run) falls
    back to ``usage.input_tokens`` and emits a one-time stderr warning naming
    ``setting_label`` so a stale-schema leaf is obvious in the run log rather
    than silently producing a harness-overhead rate. Only succeeded rows carry a
    real rate; we dedup last-wins (the append-only resume shape) and keep the
    last row — a leaf that reached the aggregator is expected complete, so the
    last row is the succeeded one. Returns the ``rate_tokenizer`` name seen on
    the rows (None if absent) so the unit travels to the curve.
    """
    out: dict[str, float] = {}
    rate_tokenizer: str | None = None
    warned = False
    for row in cli._dedup_last_wins(cli._load_jsonl(answers)):
        qid = row.get("question_id")
        if qid is None:
            continue
        if row.get("rate_tokenizer"):
            rate_tokenizer = row["rate_tokenizer"]
        ct = row.get("context_tokens")
        if ct is None:
            # Pre-fix schema: no measured rate. Fall back to usage.input_tokens
            # (harness overhead on the cc backend — wrong, but better than 0 and
            # the warning makes the staleness loud). Warn once per leaf.
            if not warned:
                print(
                    f"warning: {setting_label}: answer rows lack 'context_tokens'"
                    f" (pre-fix run?) — rate axis falling back to "
                    f"usage.input_tokens, which on the claude-code backend is "
                    f"harness overhead, not the emitted-context size. Re-run the "
                    f"reader to record the real rate.",
                    file=sys.stderr,
                )
                warned = True
            usage = row.get("usage") or {}
            out[qid] = float(usage.get("input_tokens", 0) or 0)
        else:
            out[qid] = float(ct or 0)
    return out, rate_tokenizer


def _label_by_qid(judgments: Path) -> dict[str, bool]:
    """question_id -> label (True == correct), last-wins per qid."""
    out: dict[str, bool] = {}
    for row in cli._dedup_last_wins(cli._load_jsonl(judgments)):
        qid = row.get("question_id")
        if qid is None:
            continue
        out[qid] = bool(row.get("label", False))
    return out


def _mean(xs: list[float]) -> float:
    return sum(xs) / len(xs) if xs else 0.0


def _sem_ci(xs: list[float]) -> float:
    """95% half-width from the standard error of the mean of ``xs``.

    Sample std-dev / sqrt(n) * z. n<2 -> 0 (no spread estimate from one point).
    """
    n = len(xs)
    if n < 2:
        return 0.0
    mu = _mean(xs)
    var = sum((x - mu) ** 2 for x in xs) / (n - 1)
    return _Z95 * math.sqrt(var) / math.sqrt(n)


def aggregate(outdir: Path) -> list[dict[str, Any]]:
    """Aggregate a sweep tree into one rate-distortion row per setting.

    Walks ``<outdir>/<compressor>__b<budget>/s*/`` leaves, joins each leaf's
    answers (rate) to its judgments (label) per qid, averages samples per
    question then questions per setting, and returns one dict per setting with
    ``{compressor, budget, n, mean_rate, distortion, accuracy, rate_ci,
    acc_ci}``. Rows are sorted by ``mean_rate`` ascending (the frontier reads
    left-to-right cheapest-first). Settings with no joinable question are
    dropped (they contribute no point to the curve).
    """
    if not outdir.exists():
        return []

    # setting -> qid -> {"rates": [...per sample...], "labels": [...]}
    per_setting: dict[_LeafSetting, dict[str, dict[str, list[float]]]] = {}
    # setting -> rate tokenizer name (the unit; carried to the curve row).
    rate_tok_by_setting: dict[_LeafSetting, str | None] = {}
    # Preserve first-seen setting order for a stable tie-break under equal rate.
    order: list[_LeafSetting] = []

    for setting_dir in sorted(p for p in outdir.iterdir() if p.is_dir()):
        setting = _parse_setting_dir(setting_dir.name)
        if setting is None:
            continue
        for sample_dir in sorted(p for p in setting_dir.iterdir() if p.is_dir()):
            answers = sample_dir / "answers.jsonl"
            judgments = sample_dir / "judgments.jsonl"
            if not answers.exists() or not judgments.exists():
                continue
            rates, rate_tok = _rate_by_qid(answers, setting_dir.name)
            labels = _label_by_qid(judgments)
            if setting not in per_setting:
                per_setting[setting] = {}
                order.append(setting)
            # First non-null tokenizer name seen for the setting wins (samples of
            # one setting share a tokenizer; a pre-fix leaf reports None).
            if rate_tok and not rate_tok_by_setting.get(setting):
                rate_tok_by_setting[setting] = rate_tok
            qmap = per_setting[setting]
            # Join on qids present in BOTH files (a rate with no verdict, or a
            # verdict with no rate, is not a usable rate-distortion point).
            for qid in rates.keys() & labels.keys():
                slot = qmap.setdefault(qid, {"rates": [], "labels": []})
                slot["rates"].append(rates[qid])
                slot["labels"].append(1.0 if labels[qid] else 0.0)

    rows: list[dict[str, Any]] = []
    for setting in order:
        qmap = per_setting[setting]
        if not qmap:
            continue
        # Average samples per question first, then average across questions.
        per_q_rate = [_mean(slot["rates"]) for slot in qmap.values()]
        per_q_acc = [_mean(slot["labels"]) for slot in qmap.values()]
        accuracy = _mean(per_q_acc)
        mean_rate = _mean(per_q_rate)
        rows.append(
            {
                "compressor": setting.compressor,
                "budget": setting.budget,
                "n": len(qmap),
                "mean_rate": mean_rate,
                "accuracy": accuracy,
                "distortion": 1.0 - accuracy,
                "rate_ci": _sem_ci(per_q_rate),
                "acc_ci": _sem_ci(per_q_acc),
                # The rate unit travels with the row so the artifact is
                # self-describing (None for a pre-fix run that fell back).
                "rate_tokenizer": rate_tok_by_setting.get(setting),
            }
        )

    # Sort by mean rate ascending; stable on the first-seen order for ties.
    rows.sort(key=lambda r: r["mean_rate"])
    return rows


def _fmt_num(x: float) -> str:
    """Trim trailing zeros: 1100.0 -> 1100, 0.5 -> 0.5."""
    if x == int(x):
        return str(int(x))
    return f"{x:.4g}"


def _budget_label(budget: int | None) -> str:
    return "—" if budget is None else str(budget)


def _truncate_extractive_gap(rows: list[dict[str, Any]]) -> str | None:
    """A one-line callout of the truncate-vs-extractive accuracy gap.

    Compares the two relevance strategies at the NEAREST shared budget — the
    core baseline question (does per-turn relevance selection beat a blind byte
    cut at the same token budget?). Returns ``None`` if both are not present at
    any common budget.
    """
    trunc = {r["budget"]: r for r in rows if r["compressor"] == "truncate_tokens"}
    extr = {r["budget"]: r for r in rows if r["compressor"] == "extractive_relevance"}
    shared = [b for b in trunc.keys() & extr.keys() if b is not None]
    if not shared:
        return None
    # "Nearest shared budget": the smallest shared budget (tightest squeeze,
    # where the selection strategy matters most). Stable + deterministic.
    b = min(shared)
    t_acc = trunc[b]["accuracy"]
    e_acc = extr[b]["accuracy"]
    gap = e_acc - t_acc
    sign = "+" if gap >= 0 else ""
    return (
        f"At the nearest shared budget ({b} tokens): extractive_relevance "
        f"{e_acc * 100:.1f}% vs truncate_tokens {t_acc * 100:.1f}% accuracy "
        f"({sign}{gap * 100:.1f}pp for relevance-aware selection)."
    )


def _rate_unit(rows: list[dict[str, Any]]) -> str:
    """Human label for the rate axis's unit, from the rows' rate_tokenizer.

    cl100k -> "cl100k", char-ratio:4 -> "char-ratio", absent -> "unknown
    (pre-fix run; fell back to usage.input_tokens)". Used in the md header so the
    unit is on the artifact.
    """
    names = {r.get("rate_tokenizer") for r in rows if r.get("rate_tokenizer")}
    if not names:
        return "unknown (pre-fix run; fell back to usage.input_tokens)"
    if len(names) == 1:
        return next(iter(names))
    return "mixed (" + ", ".join(sorted(names)) + ")"


def render_markdown(rows: list[dict[str, Any]]) -> str:
    """Render the frontier table (sorted by mean rate) + the gap callout."""
    lines: list[str] = []
    lines.append("# Rate-distortion frontier")
    lines.append("")
    lines.append(
        f"Rate is the emitted-context token count ({_rate_unit(rows)}) — the "
        "memory PAYLOAD the reader emits, measured at build time by the rate "
        "tokenizer. It is explicitly NOT the reader's `usage.input_tokens`, "
        "which on the claude-code backend is Claude Code's fixed harness "
        "overhead (~3279 tokens regardless of payload) and so is constant across "
        "settings. Distortion is the wrong-fraction (1 - accuracy). Sorted by "
        "mean rate (cheapest first)."
    )
    lines.append("")
    lines.append(
        "| compressor | budget | N | mean_rate | rate_ci | accuracy | distortion | acc_ci |"
    )
    lines.append("|---|---|---|---|---|---|---|---|")
    for r in rows:
        lines.append(
            "| {comp} | {budget} | {n} | {rate} | {rate_ci} | {acc:.1%} | "
            "{dist:.1%} | {acc_ci} |".format(
                comp=r["compressor"],
                budget=_budget_label(r["budget"]),
                n=r["n"],
                rate=_fmt_num(r["mean_rate"]),
                rate_ci=_fmt_num(r["rate_ci"]),
                acc=r["accuracy"],
                dist=r["distortion"],
                acc_ci=_fmt_num(r["acc_ci"]),
            )
        )
    lines.append("")
    gap = _truncate_extractive_gap(rows)
    if gap is not None:
        lines.append(gap)
        lines.append("")
    return "\n".join(lines) + "\n"


def _write_plot(rows: list[dict[str, Any]], path: Path) -> bool:
    """Best-effort PNG: accuracy (y) vs mean_rate (x), one line per compressor.

    Returns True on success, False if matplotlib is unavailable (caller prints
    the install hint). matplotlib is the optional ``[plot]`` extra — never a
    hard dependency, so the markdown table (the must-have) ships regardless.
    """
    try:
        import matplotlib

        matplotlib.use("Agg")  # headless; no display needed for a file write.
        import matplotlib.pyplot as plt
    except ImportError:
        return False

    by_comp: dict[str, list[dict[str, Any]]] = {}
    for r in rows:
        by_comp.setdefault(r["compressor"], []).append(r)

    fig, ax = plt.subplots()
    for comp, pts in sorted(by_comp.items()):
        pts = sorted(pts, key=lambda r: r["mean_rate"])
        xs = [p["mean_rate"] for p in pts]
        ys = [p["accuracy"] for p in pts]
        ax.plot(xs, ys, marker="o", label=comp)
    ax.set_xlabel("mean rate (emitted-context tokens)")
    ax.set_ylabel("accuracy")
    ax.set_title("Rate-distortion frontier")
    ax.legend()
    fig.tight_layout()
    fig.savefig(path)
    plt.close(fig)
    return True


def rd_main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="lme-qa-rd",
        description="Aggregate a sweep's leaves into the rate-distortion "
        "frontier (rd-curve.jsonl + rd-curve.md + best-effort rd-curve.png).",
    )
    ap.add_argument("--outdir", required=True, type=Path, help="sweep output root (from lme-qa-sweep)")
    args = ap.parse_args(argv)

    rows = aggregate(args.outdir)
    if not rows:
        sys.exit(
            f"error: no rate-distortion settings found under {args.outdir} "
            f"(expected <compressor>__b<budget>/s*/answers.jsonl + judgments.jsonl)"
        )

    jsonl_path = args.outdir / "rd-curve.jsonl"
    with jsonl_path.open("w") as fh:
        for r in rows:
            fh.write(json.dumps(r) + "\n")

    md_path = args.outdir / "rd-curve.md"
    md_path.write_text(render_markdown(rows))

    png_path = args.outdir / "rd-curve.png"
    if _write_plot(rows, png_path):
        plotted = f", {png_path}"
    else:
        plotted = ""
        print(
            "rd: matplotlib not installed — skipping rd-curve.png. "
            "Install the plot extra to enable it: pip install 'lme-qa[plot]'.",
            file=sys.stderr,
        )

    print(
        f"rd: {len(rows)} settings -> {jsonl_path}, {md_path}{plotted}",
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(rd_main())
