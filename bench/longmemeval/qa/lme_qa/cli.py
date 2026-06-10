"""CLI entry points: lme-qa-run (reader) and lme-qa-judge (judge).

Two execution backends select via ``--backend`` (default ``batches``):

  - ``batches``: submit one Anthropic Batches API batch per stage as Opus 4.8;
    reads ANTHROPIC_API_KEY and persists each stage's batch_id to a state file
    so an interrupted run resumes by polling the in-flight batch.
  - ``claude-code``: drive headless ``claude -p`` once per question through the
    operator's Claude Code subscription (no API key). Resume is per-question:
    completed answer/judgment rows are appended to ``--out`` as they land and
    re-running skips question_ids already succeeded (failures are retried).

The reader/judge prompt content is shared across both backends (built once from
prompts.py) so the two paths are directly comparable — the model is identical
(``claude-opus-4-8``); only the serving harness differs, and the report footer
records which path produced the number.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from . import aggregate, context, prompts
from .batch import MODEL, BatchDriver, judge_request, reader_request
from .cc import CCRequest, CCRunner, UsageLimitExhausted

# The two execution backends; ``batches`` stays the default so existing
# invocations and the key-bearing path are unchanged.
BACKENDS = ("batches", "claude-code")


def _require_api_key() -> None:
    if not os.environ.get("ANTHROPIC_API_KEY"):
        sys.exit(
            "error: ANTHROPIC_API_KEY is not set. Export it before running "
            "(the reader and judge submit Batches API requests as Opus 4.8). "
            "no API key? use --backend claude-code"
        )


def _claude_bin() -> str:
    """The claude binary name/path; env LME_QA_CLAUDE_BIN overrides (also the
    test seam). Defaults to ``claude`` on PATH.
    """
    return os.environ.get("LME_QA_CLAUDE_BIN", "claude")


def _require_claude_bin() -> str:
    """Verify the claude binary is runnable for the cc backend, or error
    clearly. Returns the resolved binary to invoke.
    """
    binary = _claude_bin()
    if shutil.which(binary) is None:
        sys.exit(
            f"error: claude-code backend selected but the claude binary "
            f"'{binary}' was not found on PATH. Install Claude Code (or set "
            f"LME_QA_CLAUDE_BIN to its path)."
        )
    return binary


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


def _append_jsonl_row(path: Path, row: dict[str, Any]) -> None:
    """Append one row to a JSONL file and flush to disk immediately.

    The cc backend persists each completed row as it lands so a crash (or a
    usage-window exhaustion) mid-run loses nothing — the next run resumes from
    what is on disk. We open in append mode and never rewrite the file, so a
    pre-existing partial file is extended, not clobbered.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as fh:
        fh.write(json.dumps(row) + "\n")
        fh.flush()
        os.fsync(fh.fileno())


def _completed_ids(path: Path) -> set[str]:
    """question_ids in ``path`` already recorded with status=="succeeded".

    Used by the cc backend to skip done questions on resume. Non-succeeded rows
    (errored/empty) are intentionally NOT included, so prior failures get
    retried on the next run. A missing/empty file yields an empty set.
    """
    if not path.exists():
        return set()
    done: set[str] = set()
    for row in _load_jsonl(path):
        if row.get("status") == "succeeded" and row.get("question_id"):
            done.add(row["question_id"])
    return done


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
    ap.add_argument("--poll-interval", type=int, default=60, help="seconds between batch polls (batches backend)")
    ap.add_argument(
        "--backend",
        choices=BACKENDS,
        default="batches",
        help="execution backend: batches (Anthropic Batches API, needs a key) "
        "or claude-code (headless claude -p via subscription, per-question resume)",
    )
    ap.add_argument(
        "--parallel",
        type=int,
        default=2,
        help="claude-code backend: concurrent claude -p calls (keep low; default 2)",
    )
    ap.add_argument(
        "--timeout-s",
        type=int,
        default=300,
        help="claude-code backend: per-call timeout in seconds (default 300)",
    )
    args = ap.parse_args(argv)

    # Backend selection gates which credential is required: batches needs a key;
    # claude-code needs the claude binary (no key — subscription auth).
    if args.backend == "claude-code":
        claude_bin = _require_claude_bin()
    else:
        _require_api_key()
    by_qid = _load_dataset(args.dataset)
    run_lines = _load_jsonl(args.run)
    state_path = args.state or _default_state(args.out)

    # Build the (qid, system, user_text) per question ONCE. The system + user
    # split is identical for both backends (prompts.READER_SYSTEM + the rendered
    # user prompt) so the two paths answer the same content.
    built_prompts: list[tuple[str, str, str]] = []
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
        built_prompts.append((qid, prompts.READER_SYSTEM, user_text))
        meta[qid] = {"question_type": entry["question_type"]}

    if not built_prompts:
        sys.exit("error: no questions to read (dataset/run mismatch?)")

    print(
        f"reader: built {len(built_prompts)} requests "
        f"({total_unknown} unknown session ids dropped, "
        f"{total_truncated} sessions truncated)",
        file=sys.stderr,
    )

    def _reader_row(r: Any) -> dict[str, Any]:
        # K is a reader-stage parameter; persist it so the judge stage can stamp
        # it into the report's provenance footer.
        return {
            "question_id": r.custom_id,
            "question_type": meta.get(r.custom_id, {}).get("question_type", ""),
            "k": args.k,
            "hypothesis": r.text,
            "status": r.status,
            "error": r.error,
            "usage": {"input_tokens": r.input_tokens, "output_tokens": r.output_tokens},
        }

    if args.backend == "claude-code":
        rows, exhausted = _cc_run_with_resume(
            out_path=args.out,
            built_prompts=built_prompts,
            claude_bin=claude_bin,
            row_of=_reader_row,
            label="reader",
            parallel=args.parallel,
            timeout_s=args.timeout_s,
        )
        n_failed = sum(1 for r in rows if r.get("status") != "succeeded")
        print(
            f"reader: {len(rows)} answers in {args.out} "
            f"({n_failed} non-succeeded)"
            + ("; run cut short by usage limit" if exhausted else ""),
            file=sys.stderr,
        )
        return 0

    requests = [reader_request(qid, sys_p, usr) for qid, sys_p, usr in built_prompts]
    driver = BatchDriver(_client(), state_path)
    results = driver.run(
        "reader", requests, fresh=args.fresh, interval_s=args.poll_interval, adopt=args.adopt
    )

    rows = [_reader_row(r) for r in results]
    n_failed = sum(1 for r in results if r.status != "succeeded")
    _write_jsonl(args.out, rows)
    print(
        f"reader: wrote {len(rows)} answers to {args.out} "
        f"({n_failed} non-succeeded)",
        file=sys.stderr,
    )
    return 0


def _cc_run_with_resume(
    out_path: Path,
    built_prompts: list[tuple[str, str, str]],
    claude_bin: str,
    row_of: Any,
    label: str,
    parallel: int,
    timeout_s: int,
) -> tuple[list[dict[str, Any]], bool]:
    """Run a stage through the claude-code backend with per-question resume.

    Skips question_ids already recorded with status=="succeeded" in ``out_path``
    (prior failures ARE retried). Rewrites ``out_path`` keeping only those
    preserved succeeded rows, then appends each fresh result row as it lands
    (append + flush + fsync) so a crash or usage-window exhaustion mid-run loses
    nothing. Returns (all_rows, exhausted) where ``all_rows`` is the preserved +
    freshly-run rows and ``exhausted`` is True if a usage limit cut the run
    short (partial progress is on disk either way).

    ``row_of`` maps a CCResult to the stage's persisted row dict (so reader and
    judge keep their own shapes); the preserved rows are reused verbatim.
    """
    done = _completed_ids(out_path)
    preserved: list[dict[str, Any]] = []
    if out_path.exists():
        # Keep only the succeeded rows we're skipping; drop non-succeeded so a
        # retried question doesn't leave a stale errored duplicate behind.
        for prior in _load_jsonl(out_path):
            if prior.get("status") == "succeeded" and prior.get("question_id") in done:
                preserved.append(prior)
    # Rewrite the file with just the preserved rows; fresh results append after.
    _write_jsonl(out_path, preserved)

    todo = [
        CCRequest(qid, system, user_text)
        for qid, system, user_text in built_prompts
        if qid not in done
    ]
    if done:
        print(
            f"{label}: resuming — {len(done)} already succeeded, "
            f"{len(todo)} to run",
            file=sys.stderr,
        )

    runner = CCRunner(claude_bin=claude_bin, parallel=parallel, timeout_s=timeout_s)
    exhausted = False
    fresh_results: list[Any]
    try:
        fresh_results = runner.run(todo, label=label)
    except UsageLimitExhausted as exc:
        exhausted = True
        fresh_results = exc.results
        print(
            f"{label}: subscription usage window exhausted "
            f"({len(fresh_results)} completed this run); progress saved to "
            f"{out_path}. Re-run the same command after the window resets to "
            f"continue (already-succeeded questions are skipped).",
            file=sys.stderr,
        )

    fresh_rows = [row_of(r) for r in fresh_results]
    for row in fresh_rows:
        _append_jsonl_row(out_path, row)

    return preserved + fresh_rows, exhausted


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
    ap.add_argument("--poll-interval", type=int, default=60, help="seconds between batch polls (batches backend)")
    ap.add_argument(
        "--backend",
        choices=BACKENDS,
        default="batches",
        help="execution backend: batches (Anthropic Batches API, needs a key) "
        "or claude-code (headless claude -p via subscription, per-question resume)",
    )
    ap.add_argument(
        "--parallel",
        type=int,
        default=2,
        help="claude-code backend: concurrent claude -p calls (keep low; default 2)",
    )
    ap.add_argument(
        "--timeout-s",
        type=int,
        default=300,
        help="claude-code backend: per-call timeout in seconds (default 300)",
    )
    args = ap.parse_args(argv)

    if args.backend == "claude-code":
        claude_bin = _require_claude_bin()
    else:
        _require_api_key()
    by_qid = _load_dataset(args.dataset)
    answers = _load_jsonl(args.answers)
    state_path = args.state or _default_state(args.out)

    # Build the judge prompt per answer ONCE (the same prompt content for both
    # backends — the judge is a single user turn, no system prompt). meta carries
    # the question_type + abstention flag each row needs.
    built_prompts: list[tuple[str, str, str]] = []
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
        # The judge has no system prompt; the cc backend still needs one (the
        # isolation flag requires a full system-prompt replacement), so pass a
        # neutral instruction that does not bias the verdict. The batches path
        # ignores this slot (judge_request takes only the user prompt).
        built_prompts.append((qid, prompts.JUDGE_SYSTEM, prompt))
        meta[qid] = {"question_type": task, "is_abstention": abst}

    if not built_prompts:
        sys.exit("error: no answers to judge")

    def _judge_row(r: Any) -> dict[str, Any]:
        label = prompts.parse_judge_label(r.text) if r.status == "succeeded" else False
        m = meta.get(r.custom_id, {})
        return {
            "question_id": r.custom_id,
            "question_type": m.get("question_type", ""),
            "is_abstention": bool(m.get("is_abstention", False)),
            "judge_text": r.text,
            "label": label,
            "status": r.status,
            "error": r.error,
            "usage": {"input_tokens": r.input_tokens, "output_tokens": r.output_tokens},
        }

    if args.backend == "claude-code":
        rows, _exhausted = _cc_run_with_resume(
            out_path=args.out,
            built_prompts=built_prompts,
            claude_bin=claude_bin,
            row_of=_judge_row,
            label="judge",
            parallel=args.parallel,
            timeout_s=args.timeout_s,
        )
    else:
        requests = [judge_request(qid, usr) for qid, _sys, usr in built_prompts]
        driver = BatchDriver(_client(), state_path)
        results = driver.run(
            "judge", requests, fresh=args.fresh, interval_s=args.poll_interval, adopt=args.adopt
        )
        rows = [_judge_row(r) for r in results]
        _write_jsonl(args.out, rows)

    # Aggregate from the persisted rows (works uniformly for both backends and,
    # for the cc path, folds in preserved-on-resume rows).
    judgments: list[aggregate.Judgment] = []
    total_in = total_out = 0
    judge_failures = 0
    for row in rows:
        if row.get("status") != "succeeded":
            judge_failures += 1
        usage = row.get("usage", {})
        total_in += usage.get("input_tokens", 0) or 0
        total_out += usage.get("output_tokens", 0) or 0
        judgments.append(
            aggregate.Judgment(
                question_id=row["question_id"],
                question_type=row.get("question_type", ""),
                is_abstention=bool(row.get("is_abstention", False)),
                label=bool(row.get("label", False)),
            )
        )

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
        # Provenance: which serving harness produced the number. The judge's own
        # --backend flag is authoritative for the judge stage's path.
        access=args.backend,
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
