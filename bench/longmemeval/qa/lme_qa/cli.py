"""CLI entry points: lme-qa-run (reader) and lme-qa-judge (judge).

Two execution backends select via ``--backend`` (default ``batches``):

  - ``batches``: submit one Anthropic Batches API batch per stage as Opus 4.8;
    reads ANTHROPIC_API_KEY and persists each stage's batch_id to a state file
    so an interrupted run resumes by polling the in-flight batch.
  - ``claude-code``: drive headless ``claude -p`` once per question through the
    operator's Claude Code subscription (no API key). Resume is per-question:
    each completed answer/judgment row is appended to ``--out`` the moment it
    lands (durable before the stage finishes) and re-running skips question_ids
    already succeeded (failures are retried). The output is append-only — never
    rewritten — so stale errored rows from a prior run are left in place and
    superseded last-wins by question_id on load (every reader dedups).

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

from . import aggregate, compress, context, prompts
from . import scorer as scorer_mod
from .batch import MODEL, BatchDriver, judge_request, reader_request
from .cc import CCRequest, CCRunner, UsageLimitExhausted
from .tokenize import CharRatioTokenizer, TiktokenTokenizer, Tokenizer, default_tokenizer

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


def _dedup_last_wins(rows: list[dict[str, Any]], key: str = "question_id") -> list[dict[str, Any]]:
    """Collapse rows to one-per-``key``, last occurrence wins, order preserved.

    The cc backend's output is append-only (never truncate-rewritten), so a qid
    that failed then succeeded on a later run appears twice — the earlier failed
    row superseded by the later succeeded one. Every reader of such a file must
    take the LAST row per qid; ``status`` then reflects the final outcome. Rows
    without ``key`` are passed through unchanged (no qid to dedup on).
    """
    latest: dict[str, dict[str, Any]] = {}
    no_key: list[dict[str, Any]] = []
    for row in rows:
        k = row.get(key)
        if k is None:
            no_key.append(row)
        else:
            latest[k] = row  # last wins
    return no_key + list(latest.values())


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
    """question_ids whose LATEST row in ``path`` has status=="succeeded".

    Used by the cc backend to skip done questions on resume. Last-wins by qid:
    the file is append-only, so a qid that failed then succeeded on a re-run has
    its succeeded row last and is counted done; a qid whose latest row is
    non-succeeded (errored/empty) is NOT counted, so it is retried. A
    missing/empty file yields an empty set.
    """
    if not path.exists():
        return set()
    latest: dict[str, dict[str, Any]] = {}
    for row in _load_jsonl(path):
        qid = row.get("question_id")
        if qid:
            latest[qid] = row  # last row wins
    return {qid for qid, row in latest.items() if row.get("status") == "succeeded"}


def _default_state(out_path: Path) -> Path:
    # State lives alongside --out by default, named after it.
    return out_path.with_suffix(out_path.suffix + ".state.json")


def _select_rate_tokenizer(choice: str) -> Tokenizer:
    """Resolve the --rate-tokenizer flag to a tokenizer instance.

    ``auto`` (default) defers to default_tokenizer() (tiktoken if importable,
    else char-ratio). ``tiktoken`` forces the cl100k impl (errors clearly if the
    [rate] extra is absent rather than silently falling back — an explicit
    request should not be downgraded without telling the operator). ``char``
    forces the dependency-free fallback (used by tests so the unit is stable
    regardless of whether tiktoken happens to be installed). The SAME returned
    instance budgets the compressor AND measures the rate, so the two share a
    unit.
    """
    if choice == "tiktoken":
        try:
            return TiktokenTokenizer()
        except ImportError:
            sys.exit(
                "error: --rate-tokenizer tiktoken requires the [rate] extra. "
                "Install it (pip install 'lme-qa[rate]') or use "
                "--rate-tokenizer char / auto."
            )
    if choice == "char":
        return CharRatioTokenizer()
    return default_tokenizer()


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
    ap.add_argument(
        "--compressor",
        default="full",
        choices=sorted(compress.REGISTRY),
        help="context compressor applied to the selected sessions before the "
        "reader prompt is built (default 'full' = no compression, "
        "byte-identical to the pre-rate-distortion path)",
    )
    ap.add_argument(
        "--budget",
        type=int,
        default=None,
        help="approximate token budget the compressor targets (default none: "
        "no budget — 'full' ignores it; the other compressors keep everything). "
        "The plotted rate is the emitted-context token count (context_tokens), "
        "measured by the rate tokenizer — not usage.input_tokens.",
    )
    ap.add_argument(
        "--expansion-reserve",
        type=float,
        default=None,
        help="fraction of the budget reserved for neighbor expansion in "
        "extractive_relevance_expanded (0.0–1.0; default: 0.3 = the "
        "compressor's built-in default). Ignored by other compressors. Threading "
        "this through the sweep (rather than baking a single value) lets the "
        "expanded-vs-extractive comparison distinguish a hypothesis failure from "
        "a reserve-mistuning confound.",
    )
    ap.add_argument(
        "--scorer",
        default="truthsayer",
        choices=sorted(scorer_mod._SCORERS),
        help="relevance scorer for the extractive_relevance compressor "
        "(truthsayer | guild; default truthsayer). Ignored by other compressors.",
    )
    ap.add_argument(
        "--rate-tokenizer",
        choices=("auto", "tiktoken", "char"),
        default="auto",
        help="tokenizer used to BOTH budget the compressor and measure the rate "
        "axis (context_tokens per row), so the budget unit and the rate unit "
        "match. 'auto' (default): tiktoken cl100k if the [rate] extra is "
        "installed, else char-ratio. 'tiktoken' forces cl100k (errors if absent); "
        "'char' forces the dependency-free fallback.",
    )
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

    # The context compressor sits between session selection and the reader
    # prompt. ``full`` (default) renders every selected session unchanged
    # (byte-identical to the pre-rate-distortion path); the others transform the
    # sessions down to ``--budget`` approximate tokens. ONE tokenizer instance
    # both budgets the compressor AND measures the rate axis (context_tokens per
    # row), so "budget 1000" and the recorded rate share a unit. The rate is the
    # emitted-context token count measured here — NOT the reader's
    # usage.input_tokens, which on the claude-code backend is fixed harness
    # overhead (~3279 tokens regardless of payload) and so reported the same rate
    # for every setting (the bug this fix corrects). The extractive_relevance
    # compressor needs a relevance scorer; build it once (env points it at the
    # live stack) and inject so it is reused across questions rather than
    # reconstructed per call.
    tokenizer = _select_rate_tokenizer(args.rate_tokenizer)
    compress_kwargs: dict[str, Any] = {}
    if args.compressor == "extractive_relevance":
        from .scorer import make_scorer

        compress_kwargs["scorer"] = make_scorer(args.scorer)
    elif args.compressor == "extractive_relevance_expanded":
        from .scorer import make_scorer

        compress_kwargs["scorer"] = make_scorer(args.scorer)
        if args.expansion_reserve is not None:
            compress_kwargs["expansion_reserve"] = args.expansion_reserve

    # Build the (qid, system, user_text) per question ONCE. The system + user
    # split is identical for both backends (prompts.READER_SYSTEM + the rendered
    # user prompt) so the two paths answer the same content.
    built_prompts: list[tuple[str, str, str]] = []
    meta: dict[str, dict[str, Any]] = {}
    # Tally context diagnostics across all questions so a retrieve backend
    # emitting stale/cross-question session ids surfaces as one stderr line
    # rather than vanishing per-question.
    total_unknown = 0
    for line in run_lines:
        qid = line["question_id"]
        entry = by_qid.get(qid)
        if entry is None:
            print(f"warning: {qid} not in dataset; skipping", file=sys.stderr)
            continue
        sessions, diag = context.select_sessions(entry, line, k=args.k)
        total_unknown += len(diag.unknown_session_ids)
        text = compress.compress(
            args.compressor,
            sessions,
            query=entry["question"],
            target_tokens=args.budget,
            tokenizer=tokenizer,
            **compress_kwargs,
        )
        # Measure the rate HERE: count the emitted context's tokens with the same
        # tokenizer that budgeted it. This is the rate axis — it tracks the
        # payload, so it varies across compressors/budgets (unlike the constant
        # harness usage.input_tokens). Counted on the rendered context text only
        # (not the reader-prompt wrapper) so it is the memory payload we emit.
        context_tokens = tokenizer.count(text)
        user_text = prompts.build_reader_prompt(
            entry["question"], entry["question_date"], text
        )
        built_prompts.append((qid, prompts.READER_SYSTEM, user_text))
        meta[qid] = {
            "question_type": entry["question_type"],
            "context_tokens": context_tokens,
            "rate_tokenizer": tokenizer.name,
        }

    if not built_prompts:
        sys.exit("error: no questions to read (dataset/run mismatch?)")

    print(
        f"reader: built {len(built_prompts)} requests "
        f"(compressor={args.compressor}, budget={args.budget}; "
        f"{total_unknown} unknown session ids dropped)",
        file=sys.stderr,
    )

    def _reader_row(r: Any) -> dict[str, Any]:
        # K is a reader-stage parameter; persist it so the judge stage can stamp
        # it into the report's provenance footer. compressor + budget are the
        # rate-distortion setting for this run — persisted alongside K so the
        # aggregator can group answers by (compressor, budget) without re-deriving
        # the setting from the leaf directory name. context_tokens is the RATE
        # axis (emitted-context token count, measured at build time with
        # rate_tokenizer); rd_aggregate reads it instead of usage.input_tokens.
        # usage.input_tokens is kept as-is — harmless, and a useful cross-check on
        # a future batches backend where it is the real input count.
        m = meta.get(r.custom_id, {})
        return {
            "question_id": r.custom_id,
            "question_type": m.get("question_type", ""),
            "k": args.k,
            "compressor": args.compressor,
            "budget": args.budget,
            "context_tokens": m.get("context_tokens"),
            "rate_tokenizer": m.get("rate_tokenizer"),
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
    (prior failures ARE retried). The output is APPEND-ONLY: each fresh result
    row is built + appended + flushed + fsync'd the moment its future lands (via
    CCRunner's ``on_result`` callback, which runs on this thread), so a crash or
    usage-window exhaustion mid-run loses nothing — every completed row is
    already durable. We never truncate-rewrite the file (a crash mid-rewrite
    could lose previously-succeeded rows); stale errored/duplicate rows left
    behind by a prior run are harmless because every reader of this file applies
    last-wins dedup by question_id (``_completed_ids`` here, and the judge's
    answer load). Returns (all_rows, exhausted) where ``all_rows`` is the
    preserved + freshly-run rows and ``exhausted`` is True if a usage limit cut
    the run short (partial progress is on disk either way).

    ``row_of`` maps a CCResult to the stage's persisted row dict (so reader and
    judge keep their own shapes); the preserved rows are reused verbatim.
    """
    done = _completed_ids(out_path)
    preserved: list[dict[str, Any]] = []
    if out_path.exists():
        # The succeeded rows we're skipping (last-wins per qid so a
        # failed-then-succeeded pair resolves to the succeeded one); these are
        # returned for aggregation but NOT rewritten to disk — they are already
        # there. Non-done qids' stale rows stay on disk, superseded last-wins by
        # the fresh append.
        latest = {r["question_id"]: r for r in _dedup_last_wins(_load_jsonl(out_path))}
        preserved = [latest[qid] for qid in done if qid in latest]

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
    fresh_rows: list[dict[str, Any]] = []

    def _persist(result: Any) -> None:
        # Invoked on the main thread per landed result (see CCRunner.run): build
        # the row and append+flush+fsync it immediately so it survives a crash.
        row = row_of(result)
        fresh_rows.append(row)
        _append_jsonl_row(out_path, row)

    try:
        runner.run(todo, label=label, on_result=_persist)
    except UsageLimitExhausted:
        # Rows that landed before the limit were already persisted by _persist
        # (called per-result inside run); the exception only stops NEW
        # submissions. fresh_rows already holds exactly what hit disk.
        exhausted = True
        print(
            f"{label}: subscription usage window exhausted "
            f"({len(fresh_rows)} completed this run); progress saved to "
            f"{out_path}. Re-run the same command after the window resets to "
            f"continue (already-succeeded questions are skipped).",
            file=sys.stderr,
        )

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
    # The cc backend appends to answers.jsonl (never rewrites), so a qid that
    # failed then succeeded on a re-run appears twice — take the last row per
    # qid (last-wins) so the judge scores the final hypothesis once, not a stale
    # failed row and not both. The batches path writes one row per qid, so the
    # dedup is a no-op there.
    answers = _dedup_last_wins(_load_jsonl(args.answers))
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
