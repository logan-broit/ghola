"""Import random samples from LongMemEval-S into the encoding-eval format.

Each LongMemEval question has:
  - answer_session_ids: list of session ids containing the answer
  - haystack_sessions: the actual sessions, each a list of {role, content, has_answer}

For Tier 1 encoding eval we want within-session ranking: given the session that
contains the answer, does the encoder rank the has_answer turn highest for the
query? We pick the first answer session per question (skipping multi-session
cases where no single session is sufficient).

Categories: all imported cases get category "longmemeval" with the LongMemEval
subtype recorded in notes. Keeps the per-category output readable; we can
split later if subtype-specific signal matters.

Run me:
    python import_longmemeval.py
    (writes eval_cases_longmemeval.jsonl)

Flags:
    --n           number of cases to sample (default 100)
    --seed        random seed for reproducibility (default 20260417)
    --source      path to LongMemEval-S cleaned JSON
                  (default ./data/longmemeval_s_cleaned.json — override via
                  LONGMEMEVAL_SOURCE env var or --source flag)
    --out         output JSONL path
                  (default eval_cases_longmemeval.jsonl next to this script)
"""
from __future__ import annotations

import argparse
import json
import os
import random
from pathlib import Path
from typing import Any, Dict, List, Optional


DEFAULT_SOURCE = Path(
    os.environ.get("LONGMEMEVAL_SOURCE", "./data/longmemeval_s_cleaned.json")
)
DEFAULT_OUT = Path(__file__).parent / "eval_cases_longmemeval.jsonl"
DEFAULT_N = 100
DEFAULT_SEED = 20260417


def build_case_from_question(q: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    """Return an EvalCase dict or None if the question isn't usable.

    Skips:
      - questions with no has_answer turn in their answer session
      - abstention-type questions where the answer is "I don't know" etc.
        (no has_answer turn either; would be skipped by above)
    """
    ans_ids = q.get("answer_session_ids") or []
    if not ans_ids:
        return None

    # Pick first answer session that exists in haystack and has a has_answer turn
    ans_session = None
    for ans_id in ans_ids:
        try:
            idx = q["haystack_session_ids"].index(ans_id)
        except ValueError:
            continue
        candidate = q["haystack_sessions"][idx]
        has_answer_positions = [
            i for i, t in enumerate(candidate) if t.get("has_answer")
        ]
        if has_answer_positions:
            ans_session = (candidate, has_answer_positions)
            break

    if ans_session is None:
        return None

    session_turns, has_answer_positions = ans_session
    target_position = has_answer_positions[0]
    secondary_positions = has_answer_positions[1:]

    # Build turns with explicit char offsets. LongMemEval turn content does
    # not include role prefixes, so we prepend "USER: " / "ASSISTANT: " to make
    # the reconstructed session_text readable and have turn-natural boundaries.
    # NOTE: this deviates from raw LongMemEval content but produces a better
    # test substrate for Tier 1; Phase 2 production encoding can decide its
    # own turn-delimitation convention.
    turns: List[Dict[str, Any]] = []
    parts: List[str] = []
    cursor = 0
    for i, t in enumerate(session_turns):
        role = t.get("role", "user")
        content_body = t.get("content", "")
        # Separator before the turn
        sep = "" if i == 0 else "\n"
        prefix = f"{sep}{role.upper()}: "
        content = prefix + content_body
        start = cursor
        end = cursor + len(content)
        turns.append({
            "role": role,
            "content": content,
            "char_start": start,
            "char_end": end,
        })
        parts.append(content)
        cursor = end

    session_text = "".join(parts)

    # Sanity: reconstruct matches
    recon = "".join(
        session_text[t["char_start"]:t["char_end"]] for t in turns
    )
    if recon != session_text:
        return None

    case_id = f"lme-{q['question_type']}-{q['question_id']}"
    answer_str = str(q.get('answer', ''))[:120]
    notes = (
        f"LongMemEval type={q['question_type']}; "
        f"answer={answer_str!r}; "
        f"has_answer turns={has_answer_positions}"
    )

    return {
        "id": case_id,
        "category": "longmemeval",
        "notes": notes,
        "session_text": session_text,
        "turns": turns,
        "query": q["question"],
        "target_position": target_position,
        "secondary_positions": secondary_positions,
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.strip().split("\n")[0])
    ap.add_argument("--n", type=int, default=DEFAULT_N)
    ap.add_argument("--seed", type=int, default=DEFAULT_SEED)
    ap.add_argument("--source", type=Path, default=DEFAULT_SOURCE)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    args = ap.parse_args()

    if not args.source.exists():
        print(f"error: source not found: {args.source}")
        return 1

    with args.source.open("r", encoding="utf-8") as f:
        data = json.load(f)

    if not isinstance(data, list):
        print(f"error: source is not a list of questions: {type(data).__name__}")
        return 1

    rng = random.Random(args.seed)
    indices = list(range(len(data)))
    rng.shuffle(indices)

    cases: List[Dict[str, Any]] = []
    skipped = 0
    for i in indices:
        if len(cases) >= args.n:
            break
        case = build_case_from_question(data[i])
        if case is None:
            skipped += 1
            continue
        cases.append(case)

    with args.out.open("w", encoding="utf-8") as f:
        for c in cases:
            f.write(json.dumps(c, ensure_ascii=False) + "\n")

    # Category breakdown by LongMemEval subtype (from the notes)
    by_subtype: Dict[str, int] = {}
    for c in cases:
        # Subtype is the middle part of the id: lme-{subtype}-{qid}
        parts = c["id"].split("-", 2)
        subtype = parts[1] if len(parts) >= 2 else "unknown"
        by_subtype[subtype] = by_subtype.get(subtype, 0) + 1

    print(f"Wrote {len(cases)} cases to {args.out} (skipped {skipped} unusable)")
    for st in sorted(by_subtype):
        print(f"  lme-{st:<30} {by_subtype[st]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
