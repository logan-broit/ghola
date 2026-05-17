"""H3.c narrow experiment: does server-side era filtering lift retrieval?

Bypasses ghola's RRF fan-out and goes chapterhouse-direct on a single
tier (the hybrid vector+FTS endpoint /v1/episodic/query) with the new
`tags_any` filter. For each held-out case we run THREE queries:

    q_none         : query_text only, no tags filter
    q_correct_era  : query_text + tags_any=[case.era]
    q_wrong_era    : query_text + tags_any=[deterministic_wrong_era]

P@5 = ground_truth_event_ids ∩ top_5_ids non-empty (binary per case,
average across cases). Lifts:

    L_correct = P@5(q_correct_era) - P@5(q_none)
    L_decay   = P@5(q_correct_era) - P@5(q_wrong_era)

Outputs a markdown findings file plus per-case top-3 dumps so we can
see whether the filter changes top-K composition even when P@5 is
flat.

One-off. If lift is real, we productionize by plumbing tags_any
through ghola. If not, we throw this away.
"""
from __future__ import annotations

import argparse
import json
import logging
import random
import sys
from collections import Counter
from pathlib import Path
from typing import Iterable

# Ensure the package next to scripts/ is importable when running the
# script directly (no install required).
_HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(_HERE.parent))

from seeding_eval.chapterhouse_client import (  # noqa: E402
    ChapterhouseClient,
    GuildEmbeddingClient,
)
from seeding_eval.eras import ERA_BOUNDARIES  # noqa: E402

logger = logging.getLogger("h3c")

# All concrete eras. pre-v12 doesn't get filtered against (corpus too
# sparse, and the corpus tagger emits "era:pre-v12" too) but we never
# pick it for the wrong-era variant.
ALL_ERAS = sorted(name for name, _ in ERA_BOUNDARIES)


def wrong_era_for(case_id: str, correct_era: str) -> str:
    """Deterministically pick a single wrong era for a case.

    Mirrors contexts.render_query's seeding so the wrong-era variant
    here lines up with the prefix-style baseline we ran earlier.
    """
    if correct_era == "pre-v12":
        candidates = ALL_ERAS
    else:
        candidates = [e for e in ALL_ERAS if e != correct_era]
    rng = random.Random(case_id)
    return rng.choice(candidates)


def _hit_at_k(hits: list[dict], gt_ids: set[str], k: int) -> bool:
    top = hits[:k]
    for h in top:
        if h.get("id") in gt_ids:
            return True
    return False


def _format_hit_summary(h: dict) -> str:
    """One-line summary of a hit for inline trace dumps."""
    text = (h.get("text") or "").replace("\n", " ").strip()
    if len(text) > 80:
        text = text[:77] + "..."
    tags = h.get("tags") or []
    score = h.get("score") or {}
    merged = score.get("merged") if isinstance(score, dict) else None
    merged_s = f"{merged:.3f}" if isinstance(merged, (int, float)) else "—"
    return f"id={h.get('id', '?')[:8]} merged={merged_s} tags={tags} text={text!r}"


def _load_cases(path: Path, held_out_only: bool) -> list[dict]:
    cases: list[dict] = []
    with path.open() as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            c = json.loads(line)
            if held_out_only and not c.get("held_out"):
                continue
            cases.append(c)
    return cases


def run_variant(
    *,
    ch: ChapterhouseClient,
    embed: list[float],
    case: dict,
    workspace_id: str,
    user_id: str,
    tags_any: list[str] | None,
    limit: int,
) -> list[dict]:
    return ch.query_episodic(
        query_text=case["query_text"],
        query_embedding=embed,
        workspace_id=workspace_id,
        user_id=user_id,
        limit=limit,
        tags_any=tags_any,
    )


def _summarize(per_case: list[dict], k: int) -> dict[str, float]:
    n = len(per_case)
    if n == 0:
        return {"p_at_k": 0.0, "n": 0}
    p = sum(1 for r in per_case if r["hit_at_k"]) / n
    return {"p_at_k": p, "n": n}


def write_report(
    path: Path,
    *,
    cases: list[dict],
    per_variant: dict[str, list[dict]],
    k: int,
    workspace_id: str,
    user_id: str,
) -> None:
    lines: list[str] = []
    lines.append("# H3.c — server-side era filter via tags_any (chapterhouse-direct)\n")
    lines.append(
        f"\nCases: {len(cases)} | k={k} | workspace={workspace_id} | user={user_id}\n"
    )
    lines.append("\nSingle-tier (vector+FTS hybrid via /v1/episodic/query). NOT ghola RRF.\n")

    lines.append("\n## Aggregate P@5\n\n")
    lines.append("| variant | P@5 | n |\n|---|---|---|\n")
    summaries = {v: _summarize(per_variant[v], k) for v in per_variant}
    for variant in ("q_none", "q_correct_era", "q_wrong_era"):
        s = summaries[variant]
        lines.append(f"| {variant} | {s['p_at_k']:.3f} | {s['n']} |\n")

    if "q_none" in summaries and "q_correct_era" in summaries and "q_wrong_era" in summaries:
        l_correct = summaries["q_correct_era"]["p_at_k"] - summaries["q_none"]["p_at_k"]
        l_decay = summaries["q_correct_era"]["p_at_k"] - summaries["q_wrong_era"]["p_at_k"]
        lines.append("\n### Lifts\n\n")
        lines.append(f"- L_correct (correct − none) = `{l_correct:+.3f}`\n")
        lines.append(f"- L_decay (correct − wrong) = `{l_decay:+.3f}`\n")

    # Tier-shape sanity: how many hits per variant on average?
    lines.append("\n## Top-K shape\n\n")
    lines.append("| variant | mean hits returned | mean top-1 same-era frac |\n|---|---|---|\n")
    for variant in ("q_none", "q_correct_era", "q_wrong_era"):
        per = per_variant[variant]
        mean_hits = sum(r["n_hits"] for r in per) / max(1, len(per))
        # fraction of cases where top-1 hit's tags include the correct era
        same_era_frac = (
            sum(1 for r in per if r.get("top1_correct_era")) / max(1, len(per))
        )
        lines.append(f"| {variant} | {mean_hits:.2f} | {same_era_frac:.3f} |\n")

    lines.append("\n## Per-case traces\n\n")
    by_id = {c["case_id"]: c for c in cases}
    case_ids = list(by_id)
    for case_id in case_ids:
        case = by_id[case_id]
        lines.append(f"\n### `{case_id}` — era={case['era']}\n")
        lines.append(f"- ground_truth: {case['ground_truth_event_ids']}\n")
        for variant in ("q_none", "q_correct_era", "q_wrong_era"):
            row = next(r for r in per_variant[variant] if r["case_id"] == case_id)
            lines.append(
                f"- **{variant}** "
                f"(tags_any={row['tags_any']}, hit@{k}={row['hit_at_k']}, "
                f"n_hits={row['n_hits']}):\n"
            )
            for h in row["top3"]:
                lines.append(f"    - {h}\n")
    path.write_text("".join(lines))


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--cases", type=Path, required=True)
    ap.add_argument("--workspace", required=True)
    ap.add_argument("--user", required=True)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--k", type=int, default=5)
    ap.add_argument("--limit", type=int, default=20,
                    help="top-N pulled from chapterhouse before P@k cutoff")
    ap.add_argument("--chapterhouse-url", default="http://localhost:8080")
    ap.add_argument("--guild-url", default="http://localhost:8082")
    ap.add_argument("--guild-model", default="qwen3-embedding")
    ap.add_argument("--all-cases", action="store_true",
                    help="run every case, not just held-out (default: held-out only)")
    args = ap.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

    cases = _load_cases(args.cases, held_out_only=not args.all_cases)
    if not cases:
        logger.error("no cases loaded from %s (held_out filter on?)", args.cases)
        return 1
    logger.info("loaded %d cases", len(cases))

    per_variant: dict[str, list[dict]] = {
        "q_none": [],
        "q_correct_era": [],
        "q_wrong_era": [],
    }

    args.out.parent.mkdir(parents=True, exist_ok=True)

    with GuildEmbeddingClient(base_url=args.guild_url, model=args.guild_model) as guild, \
            ChapterhouseClient(base_url=args.chapterhouse_url) as ch:
        for i, case in enumerate(cases, start=1):
            gt = set(case["ground_truth_event_ids"])
            era = case["era"]
            wrong_era = wrong_era_for(case["case_id"], era)

            # One embedding per case — same query_text across variants,
            # only the tags_any filter differs.
            embed = guild.embed(case["query_text"])

            for variant, tags_any in (
                ("q_none", None),
                ("q_correct_era", [f"era:{era}"]),
                ("q_wrong_era", [f"era:{wrong_era}"]),
            ):
                hits = run_variant(
                    ch=ch, embed=embed, case=case,
                    workspace_id=args.workspace,
                    user_id=args.user,
                    tags_any=tags_any,
                    limit=args.limit,
                )
                hit = _hit_at_k(hits, gt, args.k)
                top1_correct_era = False
                if hits:
                    top1_tags = hits[0].get("tags") or []
                    top1_correct_era = f"era:{era}" in top1_tags
                per_variant[variant].append({
                    "case_id": case["case_id"],
                    "tags_any": tags_any,
                    "hit_at_k": hit,
                    "n_hits": len(hits),
                    "top1_correct_era": top1_correct_era,
                    "top3": [_format_hit_summary(h) for h in hits[:3]],
                })

            logger.info(
                "[%d/%d] %s era=%s wrong=%s p@k none=%s corr=%s wrong=%s",
                i, len(cases), case["case_id"], era, wrong_era,
                per_variant["q_none"][-1]["hit_at_k"],
                per_variant["q_correct_era"][-1]["hit_at_k"],
                per_variant["q_wrong_era"][-1]["hit_at_k"],
            )

    write_report(
        args.out,
        cases=cases,
        per_variant=per_variant,
        k=args.k,
        workspace_id=args.workspace,
        user_id=args.user,
    )
    logger.info("wrote findings → %s", args.out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
