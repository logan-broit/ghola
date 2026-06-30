#!/usr/bin/env python3
"""Manual smoke test for the claude distiller (NOT part of CI).

WARNING: this makes REAL ``claude -p`` calls and CONSUMES SUBSCRIPTION QUOTA
(one call per output form). Run OFF-HOURS only, never in CI, never on a hot
usage window. The unit tests cover the pure logic with an injected fake call;
this script exercises the live cc.py-backed path the tests deliberately do not.

It distills one short hardcoded context with a real ``distill.Distiller()`` in
both forms (prose, then structured), prints each result, and prints the cache
directory so you can confirm outputs were persisted (a re-run is then free).

Run from the qa/ project root:

    .venv/bin/python scripts/distill-smoke.py

Override the cache location with LME_DISTILL_CACHE if you want an isolated dir:

    LME_DISTILL_CACHE=/tmp/distill-smoke .venv/bin/python scripts/distill-smoke.py
"""

from __future__ import annotations

import sys

# Make the package importable when run as scripts/distill-smoke.py from the qa/
# project root (mirrors scripts/oracle-smoke.py).
sys.path.insert(0, ".")

from lme_qa import distill  # noqa: E402

CONTEXT = (
    "=== Session dated 2023/05/20 (Sat) 02:21 ===\n"
    "USER: I adopted a tabby cat named Luna on May 18th 2023.\n"
    "ASSISTANT: Congratulations on adopting Luna!\n\n"
    "=== Session dated 2023/06/01 (Thu) 09:00 ===\n"
    "USER: Luna had her first vet visit; she weighs 3.2 kg.\n"
    "ASSISTANT: Good to hear Luna is healthy at 3.2 kg.\n"
)
QUERY = "What is my cat's name?"
BUDGET = 60


def main() -> int:
    d = distill.Distiller()  # real cc.py-backed call + default disk cache

    print(f"context ({len(CONTEXT)} chars), budget={BUDGET} tokens\n")

    prose = d.distill(
        CONTEXT,
        query=QUERY,
        query_mode="agnostic",
        output_form="prose",
        budget=BUDGET,
    )
    print("--- prose (agnostic) ---")
    print(prose)
    print()

    structured = d.distill(
        CONTEXT,
        query=QUERY,
        query_mode="aware",
        output_form="structured",
        budget=BUDGET,
    )
    print("--- structured (aware) ---")
    print(structured)
    print()

    print(f"cache dir: {d.cache.root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
