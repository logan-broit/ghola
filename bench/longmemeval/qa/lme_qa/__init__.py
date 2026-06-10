"""lme-qa: reader+judge QA-accuracy stage for LongMemEval-S.

Stage 1 (reader): build context from a question's top-K retrieved sessions,
submit one Batches API batch to Claude Opus 4.8, collect answers.

Stage 2 (judge): score each answer against gold with the upstream LongMemEval
judge prompts (ported verbatim), submit a second batch, aggregate accuracy.

Pure-logic modules (context, prompts, aggregate) carry the tested core; the
SDK boundary lives in batch/cli.
"""

__all__ = ["context", "prompts", "aggregate"]
