"""Tier 1 encoding eval harness.

Runs curated (session, query, target_turn) cases through an encoder strategy,
scores top-K hit rates and MRR, prints human-readable summaries or JSON.

Usage:
    python eval.py --strategy late-chunk-last-token
    python eval.py --strategy late-chunk-last-token --compare isolated
    python eval.py --strategy late-chunk-last-token --category back-reference
    python eval.py --strategy late-chunk-last-token --json

See README.md for details.
"""
from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Any, Dict, List, Optional

import torch

import strategies as S


# --- Data types ----------------------------------------------------------

VALID_CATEGORIES = {
    "self-contained",
    "back-reference",
    "forward-reference",
    "long-session",
    "short-session",
    "multi-topic",
    "identity-baseline",
}


@dataclass
class EvalCase:
    id: str
    category: str
    session_text: str
    turns: List[Dict[str, Any]]  # [{role, content, char_start, char_end}, ...]
    query: str
    target_position: int
    secondary_positions: List[int] = field(default_factory=list)
    notes: str = ""


@dataclass
class EvalResult:
    case_id: str
    category: str
    target_position: int
    ranked_positions: List[int]
    target_rank: int  # 1-indexed, 0 if not found
    top1_hit: bool
    top3_hit: bool
    top5_hit: bool
    reciprocal_rank: float
    cosines_by_position: Dict[int, float]


@dataclass
class EvalSummary:
    strategy_name: str
    n_cases: int
    top1_rate: float
    top3_rate: float
    top5_rate: float
    mrr: float
    per_category: Dict[str, Dict[str, float]]
    per_case: List[EvalResult]


# --- Case loading --------------------------------------------------------

def load_cases(path: Path) -> List[EvalCase]:
    if not path.exists():
        raise FileNotFoundError(f"cases file not found: {path}")

    cases: List[EvalCase] = []
    seen_ids: set[str] = set()
    with path.open("r", encoding="utf-8") as f:
        for lineno, raw in enumerate(f, start=1):
            raw = raw.strip()
            if not raw or raw.startswith("#"):
                continue
            try:
                obj = json.loads(raw)
            except json.JSONDecodeError as e:
                raise ValueError(f"{path}:{lineno} JSON parse error: {e}") from e

            # Required fields
            for field_name in ("id", "category", "session_text", "turns",
                               "query", "target_position"):
                if field_name not in obj:
                    raise ValueError(
                        f"{path}:{lineno} case missing required field "
                        f"'{field_name}'"
                    )

            cid = obj["id"]
            if cid in seen_ids:
                raise ValueError(f"{path}:{lineno} duplicate case id '{cid}'")
            seen_ids.add(cid)

            if obj["category"] not in VALID_CATEGORIES:
                raise ValueError(
                    f"{path}:{lineno} case '{cid}' has invalid category "
                    f"'{obj['category']}'; valid: {sorted(VALID_CATEGORIES)}"
                )

            turns = obj["turns"]
            if not isinstance(turns, list) or not turns:
                raise ValueError(
                    f"{path}:{lineno} case '{cid}' has empty or invalid turns"
                )

            for i, t in enumerate(turns):
                for f2 in ("role", "content", "char_start", "char_end"):
                    if f2 not in t:
                        raise ValueError(
                            f"{path}:{lineno} case '{cid}' turn {i} missing "
                            f"'{f2}'"
                        )
                if t["char_end"] <= t["char_start"]:
                    raise ValueError(
                        f"{path}:{lineno} case '{cid}' turn {i} has empty "
                        f"char span"
                    )

            tp = obj["target_position"]
            if not isinstance(tp, int) or tp < 0 or tp >= len(turns):
                raise ValueError(
                    f"{path}:{lineno} case '{cid}' target_position={tp} out "
                    f"of range for {len(turns)} turns"
                )

            # Validate turn reconstruction
            for i, t in enumerate(turns):
                actual = obj["session_text"][t["char_start"] : t["char_end"]]
                if actual != t["content"]:
                    raise ValueError(
                        f"{path}:{lineno} case '{cid}' turn {i} content "
                        f"does not match session_text slice "
                        f"({len(actual)} chars actual vs {len(t['content'])} "
                        f"declared)"
                    )

            cases.append(
                EvalCase(
                    id=cid,
                    category=obj["category"],
                    session_text=obj["session_text"],
                    turns=turns,
                    query=obj["query"],
                    target_position=tp,
                    secondary_positions=obj.get("secondary_positions", []),
                    notes=obj.get("notes", ""),
                )
            )
    return cases


# --- Evaluation ----------------------------------------------------------

def run_encoding_eval(
    cases: List[EvalCase],
    strategy: S.EncoderStrategy,
) -> EvalSummary:
    model = strategy.model()

    results: List[EvalResult] = []
    for case in cases:
        # Encode turns via the strategy
        turn_embs = strategy.encode_fn(case.session_text, case.turns, model)
        if turn_embs.shape[0] != len(case.turns):
            raise RuntimeError(
                f"strategy '{strategy.name}' returned {turn_embs.shape[0]} "
                f"embeddings for {len(case.turns)} turns on case '{case.id}'"
            )
        if not torch.isfinite(turn_embs).all():
            raise RuntimeError(
                f"strategy '{strategy.name}' produced non-finite embeddings "
                f"on case '{case.id}'"
            )

        # Encode query via the model's native sentence encoder
        q = model.encode(
            case.query,
            normalize_embeddings=True,
            convert_to_tensor=True,
        ).float()

        turn_embs = turn_embs.float()
        # Ensure same device
        if q.device != turn_embs.device:
            q = q.to(turn_embs.device)

        # Cosine per turn
        cosines = (turn_embs @ q).tolist()
        ranked = sorted(
            range(len(case.turns)),
            key=lambda i: cosines[i],
            reverse=True,
        )

        # Find target rank (1-indexed; 0 means not found, which should be
        # impossible since we rank every turn)
        try:
            target_rank = ranked.index(case.target_position) + 1
        except ValueError:
            target_rank = 0

        rr = 1.0 / target_rank if target_rank > 0 else 0.0

        results.append(
            EvalResult(
                case_id=case.id,
                category=case.category,
                target_position=case.target_position,
                ranked_positions=ranked,
                target_rank=target_rank,
                top1_hit=target_rank == 1,
                top3_hit=1 <= target_rank <= 3,
                top5_hit=1 <= target_rank <= 5,
                reciprocal_rank=rr,
                cosines_by_position={i: float(c) for i, c in enumerate(cosines)},
            )
        )

    n = len(results) or 1
    top1 = sum(r.top1_hit for r in results) / n
    top3 = sum(r.top3_hit for r in results) / n
    top5 = sum(r.top5_hit for r in results) / n
    mrr = sum(r.reciprocal_rank for r in results) / n

    per_cat: Dict[str, Dict[str, float]] = {}
    for cat in sorted({r.category for r in results}):
        subset = [r for r in results if r.category == cat]
        sn = len(subset) or 1
        per_cat[cat] = {
            "n": len(subset),
            "top1": sum(r.top1_hit for r in subset) / sn,
            "top3": sum(r.top3_hit for r in subset) / sn,
            "top5": sum(r.top5_hit for r in subset) / sn,
            "mrr": sum(r.reciprocal_rank for r in subset) / sn,
        }

    return EvalSummary(
        strategy_name=strategy.name,
        n_cases=len(results),
        top1_rate=top1,
        top3_rate=top3,
        top5_rate=top5,
        mrr=mrr,
        per_category=per_cat,
        per_case=results,
    )


# --- Output --------------------------------------------------------------

def format_summary(summary: EvalSummary) -> str:
    lines = []
    lines.append(f"Strategy: {summary.strategy_name}")
    lines.append(f"Cases: {summary.n_cases}")
    lines.append(
        f"Top-1: {summary.top1_rate*100:5.1f}%  "
        f"Top-3: {summary.top3_rate*100:5.1f}%  "
        f"Top-5: {summary.top5_rate*100:5.1f}%  "
        f"MRR: {summary.mrr:.3f}"
    )
    lines.append("")
    lines.append("Per category:")
    for cat, stats in summary.per_category.items():
        lines.append(
            f"  {cat:<20} (n={stats['n']:>2}): "
            f"top1={stats['top1']*100:5.1f}%  "
            f"top3={stats['top3']*100:5.1f}%  "
            f"top5={stats['top5']*100:5.1f}%  "
            f"mrr={stats['mrr']:.3f}"
        )
    return "\n".join(lines)


def format_comparison(candidate: EvalSummary, baseline: EvalSummary) -> str:
    """Format a side-by-side comparison. The first summary is the candidate
    (--strategy), the second is the baseline (--compare). Deltas are
    candidate - baseline: positive means the candidate won."""
    lines = []
    lines.append(
        f"{'Category':<20}  {candidate.strategy_name:<24}  "
        f"{baseline.strategy_name:<24}  {'delta (cand-base)':>17}"
    )
    lines.append("-" * 92)
    all_cats = sorted(set(candidate.per_category) | set(baseline.per_category))
    for cat in all_cats:
        cc = candidate.per_category.get(cat, {"top1": 0, "mrr": 0, "n": 0})
        cb = baseline.per_category.get(cat, {"top1": 0, "mrr": 0, "n": 0})
        d_top1 = cc["top1"] - cb["top1"]
        d_mrr = cc["mrr"] - cb["mrr"]
        lines.append(
            f"{cat:<20}  "
            f"top1={cc['top1']*100:5.1f}%  mrr={cc['mrr']:.3f}   "
            f"top1={cb['top1']*100:5.1f}%  mrr={cb['mrr']:.3f}   "
            f"top1 {d_top1*100:+5.1f}pp  mrr {d_mrr:+.3f}"
        )
    lines.append("-" * 92)
    d_top1 = candidate.top1_rate - baseline.top1_rate
    d_mrr = candidate.mrr - baseline.mrr
    lines.append(
        f"{'OVERALL':<20}  "
        f"top1={candidate.top1_rate*100:5.1f}%  mrr={candidate.mrr:.3f}   "
        f"top1={baseline.top1_rate*100:5.1f}%  mrr={baseline.mrr:.3f}   "
        f"top1 {d_top1*100:+5.1f}pp  mrr {d_mrr:+.3f}"
    )
    return "\n".join(lines)


def summary_to_jsonable(summary: EvalSummary) -> Dict[str, Any]:
    d = asdict(summary)
    # asdict handles dataclasses recursively; just ensure floats are floats
    return d


# --- CLI -----------------------------------------------------------------

def main(argv: Optional[List[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().split("\n")[0])
    p.add_argument(
        "--cases",
        type=Path,
        default=Path(__file__).parent / "eval_cases.jsonl",
        help="path to JSONL cases file",
    )
    p.add_argument(
        "--strategy",
        required=True,
        help=f"strategy name; registered: {', '.join(S.list_strategies())}",
    )
    p.add_argument(
        "--compare",
        default=None,
        help="optional second strategy for A/B comparison",
    )
    p.add_argument(
        "--category",
        default=None,
        help="filter cases to a single category",
    )
    p.add_argument(
        "--json",
        action="store_true",
        help="emit JSON instead of human-readable output",
    )
    args = p.parse_args(argv)

    try:
        cases = load_cases(args.cases)
    except (FileNotFoundError, ValueError) as e:
        print(f"error loading cases: {e}", file=sys.stderr)
        return 1

    if args.category:
        if args.category not in VALID_CATEGORIES:
            print(
                f"error: invalid --category '{args.category}'; "
                f"valid: {sorted(VALID_CATEGORIES)}",
                file=sys.stderr,
            )
            return 1
        cases = [c for c in cases if c.category == args.category]
        if not cases:
            print(f"no cases in category '{args.category}'", file=sys.stderr)
            return 1

    try:
        strat_a = S.get_strategy(args.strategy)
    except KeyError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1

    try:
        summary_a = run_encoding_eval(cases, strat_a)
    except Exception as e:
        print(f"error evaluating '{args.strategy}': {e}", file=sys.stderr)
        return 1

    if args.compare:
        try:
            strat_b = S.get_strategy(args.compare)
        except KeyError as e:
            print(f"error: {e}", file=sys.stderr)
            return 1
        try:
            summary_b = run_encoding_eval(cases, strat_b)
        except Exception as e:
            print(f"error evaluating '{args.compare}': {e}", file=sys.stderr)
            return 1

        if args.json:
            print(json.dumps(
                {
                    "baseline": summary_to_jsonable(summary_a),
                    "candidate": summary_to_jsonable(summary_b),
                },
                indent=2,
            ))
        else:
            # a = current default / baseline, b = candidate
            # The user passed --strategy X --compare Y;
            # X is presented as the left column (treat as "baseline-of-record")
            # Y is the right column, deltas = Y - X
            print(format_comparison(summary_a, summary_b))
        return 0

    if args.json:
        print(json.dumps(summary_to_jsonable(summary_a), indent=2))
    else:
        print(format_summary(summary_a))
    return 0


if __name__ == "__main__":
    sys.exit(main())
