"""Query rendering for the three H3 variants.

H3.c (this rev): variant maps to a ``tags_any`` filter list rather than
a string prefix. The query_text passed to ghola is the bare issue body
for every variant; era differentiation lives entirely in the structural
filter that ghola plumbs to chapterhouse's event-grain tiers (episodic
dense + episodic keyword).

This replaces H3.a's prefix approach. The prefix approach matched too
many topical-but-bug-irrelevant sessions in the session_vector tier
(82% of the n=50 vercel/next.js corpus is v16) and dragged P@5 from
~0.9 → ~0.4 on the correct_era variant. A structural filter expresses
the partition concern as metadata rather than as embedding-space tokens.
"""
from __future__ import annotations

import random

from .cases import EvalCase
from .eras import ERA_BOUNDARIES


# All known concrete eras (from eras.ERA_BOUNDARIES). pre-v12 is not in
# the wrong-era pool — too sparse to be a useful "wrong" choice, and
# corpus rows are unlikely to carry an "era:pre-v12" tag in practice.
_VALID_ERAS = frozenset(name for name, _ in ERA_BOUNDARIES)
_WRONG_ERA_POOL = sorted(_VALID_ERAS)  # sorted for determinism

VALID_VARIANTS = frozenset({"none", "correct_era", "wrong_era"})


def render_query(
    case: EvalCase, variant: str
) -> tuple[str, list[str] | None]:
    """Render the (query_text, tags_any) pair for ``case`` under one of
    three variants.

    variant ∈ {"none", "correct_era", "wrong_era"}:
      - none:        (case.query_text, None)
      - correct_era: (case.query_text, ["era:<case.era>"])
      - wrong_era:   (case.query_text, ["era:<deterministically-different>"])

    The query_text is the bare issue body in every variant — era
    differentiation moves entirely to the tags_any filter. Wrong-era
    choice is seeded by case.case_id so re-running against the same
    corpus produces the same wrong_era selection.

    Raises ValueError on unknown variant or unknown era (era not in
    ``seeding_eval.eras.ERA_BOUNDARIES`` and not the special
    ``pre-v12`` bucket).
    """
    if variant not in VALID_VARIANTS:
        raise ValueError(
            f"unknown variant {variant!r}; expected one of {sorted(VALID_VARIANTS)}"
        )

    if variant == "none":
        return case.query_text, None

    if variant == "correct_era":
        if case.era not in _VALID_ERAS and case.era != "pre-v12":
            raise ValueError(f"unknown era {case.era!r} on case {case.case_id}")
        return case.query_text, [f"era:{case.era}"]

    # variant == "wrong_era"
    if case.era == "pre-v12":
        # No "correct" era to exclude — pick any concrete era.
        candidates = _WRONG_ERA_POOL
    else:
        if case.era not in _VALID_ERAS:
            raise ValueError(f"unknown era {case.era!r} on case {case.case_id}")
        candidates = [e for e in _WRONG_ERA_POOL if e != case.era]

    rng = random.Random(case.case_id)
    chosen = rng.choice(candidates)
    return case.query_text, [f"era:{chosen}"]
