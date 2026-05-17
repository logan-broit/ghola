"""Metric primitives for the seeding eval (H1/H2/H3).

H2 — P@5: per-case hit/miss for whether any ground-truth event ID
appears in the recall's top-5 results.

H1.c — Shannon entropy: per-case `H(context | top_k)` measured over
the discrete-context distribution (module-path buckets) among the
top-K returned events. Lower = recall localized; higher = sprayed.

H3 — Lifts: pairwise differences of P@5 across query-context variants
(no-context, correct-era, wrong-era). L_correct measures whether
adding correct-era context helps; L_decay measures whether the system
distinguishes era-appropriate from era-inappropriate context.

All three are pure functions. Aggregation across cases happens in the
orchestrator (D3); these primitives operate on a single case's data.
"""
from __future__ import annotations

import math
from collections import Counter
from dataclasses import dataclass


# ---------- H2 ----------

def p_at_5(ground_truth: list[str], top_k: list[str]) -> float:
    """Return 1.0 if any ground-truth event id is in `top_k[:5]`, else 0.0.

    Aggregation across cases (mean P@5 over a held-out set) is the
    caller's job. This is the per-case primitive.

    Raises ValueError on empty ground_truth — there's no meaningful
    answer when "what should be retrieved" is unspecified.
    """
    if not ground_truth:
        raise ValueError("ground_truth must be non-empty")
    return float(any(g in top_k[:5] for g in ground_truth))


# ---------- H1.c ----------

def shannon_entropy(buckets: list[str]) -> float:
    """Shannon entropy `-Σ p · log₂ p` of the bucket distribution.

    Returns 0.0 for a constant input (one distinct bucket).
    Returns log₂(K) for K distinct buckets each at frequency 1/K (uniform).
    Raises ValueError on empty input.
    """
    if not buckets:
        raise ValueError("buckets must be non-empty")
    counts = Counter(buckets)
    total = len(buckets)
    return -sum((c / total) * math.log2(c / total) for c in counts.values())


# ---------- H3 ----------

@dataclass(frozen=True)
class H3Lifts:
    """Two derived deltas across query-context variants.

    l_correct: P@5(correct_era) - P@5(none)
        Did adding the right context help?
    l_decay:   P@5(correct_era) - P@5(wrong_era)
        Did the system distinguish right-era from wrong-era context?
    """
    l_correct: float
    l_decay: float


def compute_h3_lifts(*, p_none: float, p_correct: float, p_wrong: float) -> H3Lifts:
    """Compute H3 lifts from the three per-variant P@5 aggregates.

    All three inputs must be in [0, 1].
    """
    for name, val in (("p_none", p_none), ("p_correct", p_correct), ("p_wrong", p_wrong)):
        if not 0.0 <= val <= 1.0:
            raise ValueError(f"{name}={val} is not in [0, 1]")
    return H3Lifts(
        l_correct=p_correct - p_none,
        l_decay=p_correct - p_wrong,
    )
