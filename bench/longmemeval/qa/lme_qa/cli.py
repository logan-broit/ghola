"""CLI entry points: lme-qa-run (reader) and lme-qa-judge (judge).

Both read ANTHROPIC_API_KEY from the environment and error early if it is
unset. The two stages persist their batch ids to a state file so an
interrupted run resumes by polling the in-flight batch.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from . import aggregate, context, prompts
from .batch import MODEL, BatchDriver, judge_request, reader_request


def _require_api_key() -> None:
    if not os.environ.get("ANTHROPIC_API_KEY"):
        sys.exit(
            "error: ANTHROPIC_API_KEY is not set. Export it before running "
            "(the reader and judge submit Batches API requests as Opus 4.8)."
        )


def _client() -> Any:
    # Imported lazily so the pure-logic modules and `--help` don't require the
    # SDK to be importable in every environment.
    import anthropic

    return anthropic.Anthropic()


def _load_dataset(path: Path) -> dict[str, dict[str, Any]]:
    """Load the LongMemEval dataset JSON into a question_id -> entry map.

    Raises a clear error naming the offending entry index if any record is
    missing ``question_id`` — a malformed dataset should fail loudly with a
    pointer, not KeyError deep in a dict comprehension.
    """
    data = json.loads(path.read_text())
    out: dict[str, dict[str, Any]] = {}
    for i, e in enumerate(data):
        if "question_id" not in e:
            raise ValueError(
                f"{path}: dataset entry at index {i} is missing 'question_id'"
            )
        out[e["question_id"]] = e
    return out


def _load_jsonl(path: Path) -> list[dict[str, Any]]:
    """Load a JSONL file, reporting the file + line number of a bad JSON line."""
    out: list[dict[str, Any]] = []
    with path.open() as fh:
        for lineno, line in enumerate(fh, start=1):
            line = line.strip()
            if not line:
                continue
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError as exc:
                raise ValueError(
                    f"{path}: line {lineno}: invalid JSON: {exc}"
                ) from exc
    return out


def _write_jsonl(path: Path, rows: list[dict[str, Any]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w") as fh:
        for row in rows:
            fh.write(json.dumps(row) + "\n")


def _default_state(out_path: Path) -> Path:
    # State lives alongside --out by default, named after it.
    return out_path.with_suffix(out_path.suffix + ".state.json")


# --- reader -----------------------------------------------------------------


def run_main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="lme-qa-run",
        description="Reader stage: build context from retrieve results and "
        "produce answers via an Opus 4.8 Batches run.",
    )
    ap.add_argument("--dataset", required=True, type=Path, help="LongMemEval dataset JSON")
    ap.add_argument("--run", required=True, type=Path, help="retrieve results JSONL")
    ap.add_argument("--out", required=True, type=Path, help="answers JSONL output")
    ap.add_argument("--k", type=int, default=10, help="top-K sessions for context (default 10)")
    ap.add_argument("--state", type=Path, default=None, help="batch state file (default alongside --out)")
    ap.add_argument("--fresh", action="store_true", help="ignore state; submit a new batch")
    ap.add_argument(
        "--adopt",
        type=str,
        default=None,
        help="manually adopt an existing batch_id instead of submitting "
        "(use to recover an orphaned paid batch the auto-adoption missed)",
    )
    ap.add_argument("--poll-interval", type=int, default=60, help="seconds between batch polls")
    args = ap.parse_args(argv)

    _require_api_key()
    by_qid = _load_dataset(args.dataset)
    run_lines = _load_jsonl(args.run)
    state_path = args.state or _default_state(args.out)

    requests: list[dict[str, Any]] = []
    meta: dict[str, dict[str, Any]] = {}
    # Tally context-build diagnostics across all questions so a retrieve backend
    # emitting stale/cross-question session ids (or pathologically long sessions)
    # surfaces as one stderr line rather than vanishing per-question.
    total_unknown = 0
    total_truncated = 0
    for line in run_lines:
        qid = line["question_id"]
        entry = by_qid.get(qid)
        if entry is None:
            print(f"warning: {qid} not in dataset; skipping", file=sys.stderr)
            continue
        built = context.build_context(entry, line, k=args.k)
        total_unknown += len(built.unknown_session_ids)
        total_truncated += len(built.truncated_session_ids)
        user_text = prompts.build_reader_prompt(
            entry["question"], entry["question_date"], built.text
        )
        requests.append(reader_request(qid, prompts.READER_SYSTEM, user_text))
        meta[qid] = {"question_type": entry["question_type"]}

    if not requests:
        sys.exit("error: no questions to read (dataset/run mismatch?)")

    print(
        f"reader: built {len(requests)} requests "
        f"({total_unknown} unknown session ids dropped, "
        f"{total_truncated} sessions truncated)",
        file=sys.stderr,
    )

    driver = BatchDriver(_client(), state_path)
    results = driver.run(
        "reader", requests, fresh=args.fresh, interval_s=args.poll_interval, adopt=args.adopt
    )

    rows: list[dict[str, Any]] = []
    n_failed = 0
    for r in results:
        if r.status != "succeeded":
            n_failed += 1
        rows.append(
            {
                "question_id": r.custom_id,
                "question_type": meta.get(r.custom_id, {}).get("question_type", ""),
                # K is a reader-stage parameter; persist it so the judge stage
                # can stamp it into the report's provenance footer.
                "k": args.k,
                "hypothesis": r.text,
                "status": r.status,
                "error": r.error,
                "usage": {"input_tokens": r.input_tokens, "output_tokens": r.output_tokens},
            }
        )
    _write_jsonl(args.out, rows)
    print(
        f"reader: wrote {len(rows)} answers to {args.out} "
        f"({n_failed} non-succeeded)",
        file=sys.stderr,
    )
    return 0


# --- judge ------------------------------------------------------------------


def judge_main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="lme-qa-judge",
        description="Judge stage: score answers against gold with the upstream "
        "LongMemEval judge prompts via an Opus 4.8 Batches run.",
    )
    ap.add_argument("--dataset", required=True, type=Path, help="LongMemEval dataset JSON")
    ap.add_argument("--answers", required=True, type=Path, help="answers JSONL from lme-qa-run")
    ap.add_argument("--out", required=True, type=Path, help="judgments JSONL output")
    ap.add_argument("--report", required=True, type=Path, help="markdown report output")
    ap.add_argument("--state", type=Path, default=None, help="batch state file (default alongside --out)")
    ap.add_argument("--fresh", action="store_true", help="ignore state; submit a new batch")
    ap.add_argument(
        "--adopt",
        type=str,
        default=None,
        help="manually adopt an existing batch_id instead of submitting "
        "(use to recover an orphaned paid batch the auto-adoption missed)",
    )
    ap.add_argument("--poll-interval", type=int, default=60, help="seconds between batch polls")
    args = ap.parse_args(argv)

    _require_api_key()
    by_qid = _load_dataset(args.dataset)
    answers = _load_jsonl(args.answers)
    state_path = args.state or _default_state(args.out)

    requests: list[dict[str, Any]] = []
    meta: dict[str, dict[str, Any]] = {}
    # A reader item that did not succeed produced an empty hypothesis; the judge
    # will score it wrong (defensible — no answer). Count it so the report can
    # footnote the loss rather than hide it inside a depressed accuracy.
    reader_failures = 0
    for ans in answers:
        qid = ans["question_id"]
        entry = by_qid.get(qid)
        if entry is None:
            print(f"warning: {qid} not in dataset; skipping", file=sys.stderr)
            continue
        if ans.get("status", "succeeded") != "succeeded":
            reader_failures += 1
        task = entry["question_type"]
        abst = prompts.is_abstention(qid)
        prompt = prompts.get_anscheck_prompt(
            task,
            entry["question"],
            entry["answer"],
            ans.get("hypothesis", ""),
            abstention=abst,
        )
        requests.append(judge_request(qid, prompt))
        meta[qid] = {"question_type": task, "is_abstention": abst}

    if not requests:
        sys.exit("error: no answers to judge")

    driver = BatchDriver(_client(), state_path)
    results = driver.run(
        "judge", requests, fresh=args.fresh, interval_s=args.poll_interval, adopt=args.adopt
    )

    rows: list[dict[str, Any]] = []
    judgments: list[aggregate.Judgment] = []
    total_in = total_out = 0
    judge_failures = 0
    for r in results:
        if r.status != "succeeded":
            judge_failures += 1
        label = prompts.parse_judge_label(r.text) if r.status == "succeeded" else False
        m = meta.get(r.custom_id, {})
        qtype = m.get("question_type", "")
        abst = bool(m.get("is_abstention", False))
        total_in += r.input_tokens
        total_out += r.output_tokens
        rows.append(
            {
                "question_id": r.custom_id,
                "question_type": qtype,
                "is_abstention": abst,
                "judge_text": r.text,
                "label": label,
                "status": r.status,
                "error": r.error,
                "usage": {"input_tokens": r.input_tokens, "output_tokens": r.output_tokens},
            }
        )
        judgments.append(
            aggregate.Judgment(
                question_id=r.custom_id,
                question_type=qtype,
                is_abstention=abst,
                label=label,
            )
        )

    _write_jsonl(args.out, rows)
    report = aggregate.aggregate(
        judgments,
        total_in,
        total_out,
        reader_failures=reader_failures,
        judge_failures=judge_failures,
    )
    # K travels with the answers file (reader-stage parameter); only stamp it
    # when every answer agrees — mixed-K answer files get no K claim.
    k_values = {ans.get("k") for ans in answers if ans.get("k") is not None}
    answers_k = k_values.pop() if len(k_values) == 1 else None
    md = aggregate.render_markdown(
        report,
        title="QA accuracy (LongMemEval-S)",
        model=MODEL,
        k=answers_k,
        date=datetime.now(timezone.utc).strftime("%Y-%m-%d"),
    )
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(md)
    print(
        f"judge: overall {report.overall.correct}/{report.overall.n} "
        f"-> {args.out}, report -> {args.report}",
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(run_main())
