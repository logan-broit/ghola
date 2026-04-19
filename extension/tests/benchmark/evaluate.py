#!/usr/bin/env python3
"""
pg_ghola Benchmark Evaluator

LLM-as-judge evaluation of benchmark results. For each query, compares
no_memory vs cognitive responses on multiple dimensions.

This is the interesting part: not just "did retrieval find the right docs"
but "did the final answer actually improve from having memory?"

Usage:
  python3 evaluate.py snapshots/baseline/results.json
"""

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path

JUDGE_MODEL = os.environ.get("JUDGE_MODEL", "claude-sonnet-4-20250514")

JUDGE_SYSTEM = """You are evaluating whether an AI memory system improves answer quality.

You will see a question, the expected knowledge that should appear in a good answer,
and two responses: one from an LLM with NO external memory, and one with cognitive
memory retrieval providing relevant context.

Score EACH response on these dimensions (1-5 scale):

1. **Accuracy** — Are the facts correct? No hallucinations?
   1=mostly wrong, 3=partially correct, 5=fully accurate

2. **Completeness** — Does it cover all expected knowledge points?
   1=misses most, 3=covers some, 5=covers all

3. **Specificity** — Does it give concrete details (IPs, paths, commands) vs vague generalities?
   1=very vague, 3=somewhat specific, 5=precise and actionable

4. **Grounding** — Are claims traceable to stored knowledge vs potentially hallucinated?
   1=appears fabricated, 3=plausible but unverified, 5=clearly sourced from memory

5. **Reasoning Depth** — Does it build on prior analysis or re-derive from scratch?
   1=surface-level, 3=adequate reasoning, 5=builds on accumulated knowledge

Return a JSON object (no markdown fencing) with this exact structure:
{
  "no_memory": {"accuracy": N, "completeness": N, "specificity": N, "grounding": N, "reasoning": N, "notes": "brief explanation"},
  "cognitive": {"accuracy": N, "completeness": N, "specificity": N, "grounding": N, "reasoning": N, "notes": "brief explanation"},
  "winner": "no_memory|cognitive|tie",
  "memory_value": "high|medium|low|none",
  "explanation": "1-2 sentences on whether memory made a meaningful difference"
}"""


def call_judge(query_obj: dict, no_memory_response: str, cognitive_response: str) -> dict:
    """Have an LLM judge compare the two responses."""
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")
    if not api_key:
        for path in [
            Path.home() / ".anthropic" / "api_key",
            Path.home() / ".config" / "anthropic" / "api_key",
        ]:
            if path.exists():
                api_key = path.read_text().strip()
                break

    if not api_key:
        return {"error": "No ANTHROPIC_API_KEY"}

    expected = "\n".join(f"- {k}" for k in query_obj.get("expected_knowledge", []))

    user_msg = f"""## Question
{query_obj['query']}

## Expected Knowledge
{expected}

## Reasoning Required
{query_obj.get('reasoning_required', 'N/A')}

## Response A (NO MEMORY — reasoning from scratch)
{no_memory_response}

## Response B (COGNITIVE MEMORY — with retrieved context)
{cognitive_response}"""

    payload = {
        "model": JUDGE_MODEL,
        "max_tokens": 1024,
        "system": JUDGE_SYSTEM,
        "messages": [{"role": "user", "content": user_msg}],
    }

    result = subprocess.run(
        [
            "curl", "-sk", "-X", "POST", "https://api.anthropic.com/v1/messages",
            "-H", "Content-Type: application/json",
            "-H", f"x-api-key: {api_key}",
            "-H", "anthropic-version: 2023-06-01",
            "-d", json.dumps(payload),
        ],
        capture_output=True, text=True, timeout=120,
    )

    resp = json.loads(result.stdout)
    judge_text = resp.get("content", [{}])[0].get("text", "{}")

    try:
        return json.loads(judge_text)
    except json.JSONDecodeError:
        # Try to extract JSON from markdown fencing
        import re
        match = re.search(r'\{[\s\S]+\}', judge_text)
        if match:
            return json.loads(match.group())
        return {"error": f"Failed to parse judge output: {judge_text[:200]}"}


def evaluate_snapshot(results_file: Path):
    """Run LLM-as-judge evaluation on a snapshot."""
    with open(results_file) as f:
        data = json.load(f)

    # Load queries for expected knowledge
    queries_file = Path(__file__).parent / "queries.json"
    with open(queries_file) as f:
        queries_by_id = {q["id"]: q for q in json.load(f)["queries"]}

    # Group results by query_id
    by_query = {}
    for r in data["results"]:
        qid = r.get("query_id")
        if qid not in by_query:
            by_query[qid] = {}
        by_query[qid][r.get("condition")] = r

    evaluations = []
    for qid, conditions in by_query.items():
        if "no_memory" not in conditions or "cognitive" not in conditions:
            continue
        if "error" in conditions["no_memory"] or "error" in conditions["cognitive"]:
            continue

        query_obj = queries_by_id.get(qid)
        if not query_obj:
            continue

        print(f"Evaluating {qid}...", end=" ", flush=True)

        judgment = call_judge(
            query_obj,
            conditions["no_memory"]["response"],
            conditions["cognitive"]["response"],
        )

        judgment["query_id"] = qid
        judgment["category"] = query_obj["category"]
        evaluations.append(judgment)

        winner = judgment.get("winner", "?")
        value = judgment.get("memory_value", "?")
        print(f"winner={winner}, memory_value={value}")

        time.sleep(1)  # Rate limit

    # Save evaluations
    eval_dir = results_file.parent
    eval_file = eval_dir / "evaluations.json"
    with open(eval_file, "w") as f:
        json.dump(evaluations, f, indent=2)
    print(f"\nEvaluations saved to {eval_file}")

    # Print summary
    print_eval_summary(evaluations)

    # Save markdown summary
    save_eval_markdown(evaluations, eval_dir / "evaluation_report.md")

    return evaluations


def print_eval_summary(evaluations: list):
    """Print a summary of evaluation results."""
    valid = [e for e in evaluations if "error" not in e]
    if not valid:
        print("No valid evaluations")
        return

    print(f"\n{'='*60}")
    print(f"EVALUATION SUMMARY ({len(valid)} queries)")
    print(f"{'='*60}\n")

    # Win/loss/tie
    wins = {"no_memory": 0, "cognitive": 0, "tie": 0}
    for e in valid:
        w = e.get("winner", "tie")
        wins[w] = wins.get(w, 0) + 1
    print(f"Winners: cognitive={wins.get('cognitive',0)}, "
          f"no_memory={wins.get('no_memory',0)}, tie={wins.get('tie',0)}")

    # Memory value distribution
    values = {}
    for e in valid:
        v = e.get("memory_value", "unknown")
        values[v] = values.get(v, 0) + 1
    print(f"Memory value: {values}")

    # Average scores per condition
    for condition in ["no_memory", "cognitive"]:
        scores = [e[condition] for e in valid if condition in e and isinstance(e[condition], dict)]
        if not scores:
            continue
        dims = ["accuracy", "completeness", "specificity", "grounding", "reasoning"]
        avgs = {d: sum(s.get(d, 0) for s in scores) / len(scores) for d in dims}
        composite = sum(avgs.values()) / len(dims)
        print(f"\n{condition}:")
        for d, v in avgs.items():
            print(f"  {d}: {v:.1f}/5")
        print(f"  composite: {composite:.1f}/5")

    # Per-category breakdown
    categories = set(e.get("category") for e in valid)
    print(f"\nBy category:")
    for cat in sorted(categories):
        cat_evals = [e for e in valid if e.get("category") == cat]
        cat_wins = sum(1 for e in cat_evals if e.get("winner") == "cognitive")
        print(f"  {cat}: {cat_wins}/{len(cat_evals)} cognitive wins")


def save_eval_markdown(evaluations: list, output_file: Path):
    """Save evaluation as markdown report."""
    valid = [e for e in evaluations if "error" not in e]
    if not valid:
        return

    lines = [
        "# pg_ghola Memory Evaluation Report",
        "",
        "## Summary",
        "",
    ]

    wins = {"no_memory": 0, "cognitive": 0, "tie": 0}
    for e in valid:
        wins[e.get("winner", "tie")] = wins.get(e.get("winner", "tie"), 0) + 1

    lines.extend([
        f"| Metric | Value |",
        f"|--------|-------|",
        f"| Queries evaluated | {len(valid)} |",
        f"| Cognitive wins | {wins.get('cognitive', 0)} |",
        f"| No-memory wins | {wins.get('no_memory', 0)} |",
        f"| Ties | {wins.get('tie', 0)} |",
        "",
        "## Score Comparison",
        "",
        "| Dimension | No Memory | Cognitive | Delta |",
        "|-----------|-----------|-----------|-------|",
    ])

    dims = ["accuracy", "completeness", "specificity", "grounding", "reasoning"]
    for d in dims:
        nm_scores = [e["no_memory"][d] for e in valid if "no_memory" in e and isinstance(e["no_memory"], dict)]
        cg_scores = [e["cognitive"][d] for e in valid if "cognitive" in e and isinstance(e["cognitive"], dict)]
        nm_avg = sum(nm_scores) / max(len(nm_scores), 1)
        cg_avg = sum(cg_scores) / max(len(cg_scores), 1)
        delta = cg_avg - nm_avg
        lines.append(f"| {d.title()} | {nm_avg:.1f} | {cg_avg:.1f} | {delta:+.1f} |")

    lines.extend(["", "## Per-Query Details", ""])

    for e in valid:
        lines.extend([
            f"### {e['query_id']} ({e.get('category', '?')})",
            f"**Winner:** {e.get('winner', '?')} | **Memory value:** {e.get('memory_value', '?')}",
            f"",
            f"> {e.get('explanation', 'N/A')}",
            "",
        ])

        for condition in ["no_memory", "cognitive"]:
            if condition in e and isinstance(e[condition], dict):
                scores = e[condition]
                lines.append(
                    f"**{condition}**: acc={scores.get('accuracy',0)} "
                    f"comp={scores.get('completeness',0)} "
                    f"spec={scores.get('specificity',0)} "
                    f"ground={scores.get('grounding',0)} "
                    f"reason={scores.get('reasoning',0)}"
                )
                if scores.get("notes"):
                    lines.append(f"  _{scores['notes']}_")
        lines.append("")

    with open(output_file, "w") as f:
        f.write("\n".join(lines))
    print(f"Report saved to {output_file}")


def main():
    parser = argparse.ArgumentParser(description="Evaluate pg_ghola benchmark results")
    parser.add_argument("results", help="Path to results.json")
    args = parser.parse_args()

    evaluate_snapshot(Path(args.results))


if __name__ == "__main__":
    main()
