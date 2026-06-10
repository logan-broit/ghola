"""Aggregate judge labels into per-question-type and overall accuracy.

Follows upstream LongMemEval evaluate_qa.py exactly for the canonical numbers:

  - overall accuracy = mean over every judged entry (``np.mean`` of the labels);
  - per-question-type accuracy buckets by the question's ``question_type``
    (``qtype2acc[qid2qtype[qid]]``).

Upstream does NOT split ``_abs`` (abstention) questions into a separate bucket —
each ``_abs`` question is scored with the abstention judge prompt but its label
lands in its base ``question_type`` bucket. We preserve that for the canonical
table, and additionally report a supplementary abstention breakdown (answerable
vs abstention) so the ``_abs`` slice is visible without altering the
upstream-canonical per-type accuracy.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Iterable


@dataclass(frozen=True)
class Judgment:
    """One judged question: its type, abstention flag, and correctness."""

    question_id: str
    question_type: str
    is_abstention: bool
    label: bool  # True == judged correct


@dataclass(frozen=True)
class Bucket:
    """Accuracy over a set of judgments."""

    n: int
    correct: int

    @property
    def accuracy(self) -> float:
        # Empty bucket -> 0.0. Reported alongside n=0 so it reads as "no data"
        # rather than a real zero-accuracy result.
        return self.correct / self.n if self.n else 0.0


@dataclass
class Report:
    """Full aggregation result."""

    overall: Bucket
    by_type: dict[str, Bucket]
    # Supplementary abstention view (does not feed the canonical by_type).
    answerable: Bucket
    abstention: Bucket
    total_input_tokens: int = 0
    total_output_tokens: int = 0
    # Pipeline-failure counts, surfaced so the denominator isn't silently
    # inflated by non-succeeded items. A reader item that errored becomes an
    # empty hypothesis and is judged wrong (defensible — it produced no answer);
    # a judge item that errored is labeled False. Both are counted in `overall`
    # as incorrect, so these counts make the loss VISIBLE rather than hidden.
    reader_failures: int = 0
    judge_failures: int = 0
    _type_order: list[str] = field(default_factory=list)


def _bucket(labels: Iterable[bool]) -> Bucket:
    labels = list(labels)
    return Bucket(n=len(labels), correct=sum(1 for x in labels if x))


def aggregate(
    judgments: Iterable[Judgment],
    total_input_tokens: int = 0,
    total_output_tokens: int = 0,
    reader_failures: int = 0,
    judge_failures: int = 0,
) -> Report:
    """Aggregate judgments into overall + per-type + abstention buckets."""
    judgments = list(judgments)

    by_type_labels: dict[str, list[bool]] = {}
    type_order: list[str] = []
    answerable_labels: list[bool] = []
    abstention_labels: list[bool] = []

    for j in judgments:
        if j.question_type not in by_type_labels:
            by_type_labels[j.question_type] = []
            type_order.append(j.question_type)
        by_type_labels[j.question_type].append(j.label)
        if j.is_abstention:
            abstention_labels.append(j.label)
        else:
            answerable_labels.append(j.label)

    return Report(
        overall=_bucket(j.label for j in judgments),
        by_type={t: _bucket(by_type_labels[t]) for t in type_order},
        answerable=_bucket(answerable_labels),
        abstention=_bucket(abstention_labels),
        total_input_tokens=total_input_tokens,
        total_output_tokens=total_output_tokens,
        reader_failures=reader_failures,
        judge_failures=judge_failures,
        _type_order=type_order,
    )


def _pct(b: Bucket) -> str:
    return f"{b.accuracy * 100:.1f}%"


def render_markdown(
    report: Report,
    title: str = "QA accuracy",
    model: str | None = None,
    k: int | None = None,
    date: str | None = None,
) -> str:
    """Render the report as a markdown section ready for docs/benchmarks.md.

    ``model`` / ``k`` / ``date`` are provenance, passed in from the CLI (not
    hard-coded here) so the published number always records which model, top-K,
    sample size, and UTC date produced it.
    """
    lines: list[str] = []
    lines.append(f"## {title}")
    lines.append("")
    lines.append(
        f"**Overall accuracy: {_pct(report.overall)}** "
        f"({report.overall.correct}/{report.overall.n})"
    )
    lines.append("")
    # Failure footnote: makes the silent denominator loss visible. Reader/judge
    # failures are already counted as incorrect in `overall`; spell that out so
    # a depressed accuracy isn't mistaken for a pure capability result.
    if report.reader_failures or report.judge_failures:
        parts: list[str] = []
        if report.reader_failures:
            parts.append(f"{report.reader_failures} reader failures (counted incorrect)")
        if report.judge_failures:
            parts.append(f"{report.judge_failures} judge failures")
        lines.append(f"_{', '.join(parts)}._")
        lines.append("")
    lines.append("| Question type | N | Accuracy |")
    lines.append("|---|---|---|")
    # Sort question types for a stable, readable table.
    for qtype in sorted(report._type_order):
        b = report.by_type[qtype]
        lines.append(f"| {qtype} | {b.n} | {_pct(b)} |")
    lines.append("")
    lines.append("Supplementary (abstention split, not part of the per-type table above):")
    lines.append("")
    lines.append("| Slice | N | Accuracy |")
    lines.append("|---|---|---|")
    lines.append(
        f"| answerable | {report.answerable.n} | {_pct(report.answerable)} |"
    )
    lines.append(
        f"| abstention (`_abs`) | {report.abstention.n} | {_pct(report.abstention)} |"
    )
    if report.total_input_tokens or report.total_output_tokens:
        lines.append("")
        lines.append(
            f"_Judge token usage: {report.total_input_tokens} input, "
            f"{report.total_output_tokens} output._"
        )
    # Provenance footer: model / top-K / sample size / UTC date, when supplied.
    prov: list[str] = []
    if model is not None:
        prov.append(f"model={model}")
    if k is not None:
        prov.append(f"K={k}")
    prov.append(f"n={report.overall.n}")
    if date is not None:
        prov.append(f"{date} (UTC)")
    if model is not None or k is not None or date is not None:
        lines.append("")
        lines.append(f"_{', '.join(prov)}._")
    return "\n".join(lines) + "\n"
