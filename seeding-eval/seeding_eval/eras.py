"""Map a UTC timestamp to a Next.js major-version era.

Boundary dates are major-version release dates sourced from the
Next.js GitHub releases page (https://github.com/vercel/next.js/releases).
Boundaries are inclusive on the lower bound: a timestamp exactly at
the release date belongs to that era, not the prior one — this matches
how downstream metrics (H1/H2/H3) bucket events for per-era analysis.

Naive datetimes are rejected; silent UTC coercion is exactly the bug
class that breaks Ebbinghaus decay months from now.
"""
from __future__ import annotations

from datetime import datetime, timezone

# Major-version release dates. Add new versions here as they ship.
ERA_BOUNDARIES: list[tuple[str, datetime]] = [
    ("v12", datetime(2021, 10, 26, tzinfo=timezone.utc)),
    ("v13", datetime(2022, 10, 25, tzinfo=timezone.utc)),
    ("v14", datetime(2023, 10, 26, tzinfo=timezone.utc)),
    ("v15", datetime(2024, 10, 21, tzinfo=timezone.utc)),
    ("v16", datetime(2025, 10, 21, tzinfo=timezone.utc)),
]


def era_for(ts: datetime) -> str:
    """Return the Next.js major-version era for a UTC timestamp.

    Returns "pre-v12" for timestamps before the v12 release. Raises
    ValueError if `ts` is naive (no tzinfo) — callers must pass aware
    datetimes so era assignment is unambiguous across timezones.
    """
    if ts.tzinfo is None:
        raise ValueError("era_for requires a timezone-aware datetime")
    era = "pre-v12"
    for name, boundary in ERA_BOUNDARIES:
        if ts >= boundary:
            era = name
        else:
            break
    return era
