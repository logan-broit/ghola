"""Result dataclasses + JSON round-trip for the eval run.

The `RunReport` is the eval's deliverable: H1/H2/H3 numbers + failures
+ enough metadata to reproduce or longitudinally compare runs. Frozen
dataclasses keep the result immutable once aggregation has finished --
post-aggregation drift is a real bug class in eval pipelines.
"""
from __future__ import annotations

import json
import typing
from dataclasses import MISSING, dataclass, asdict, fields, is_dataclass


@dataclass(frozen=True)
class CaseFailure:
    """A case that errored during eval. Recorded; never silently dropped."""
    case_id: str
    variant: str
    error: str


@dataclass(frozen=True)
class H1Result:
    """Aggregate H1.c results across the held-out set."""
    avg_entropy: float
    n_cases: int
    per_bucket: dict[str, float]


@dataclass(frozen=True)
class H2Result:
    """Aggregate H2 results."""
    p_at_5_none: float
    p_at_5_correct_era: float
    p_at_5_wrong_era: float
    n_cases: int


@dataclass(frozen=True)
class H3PerEra:
    """Per-era slice of H3 lifts."""
    n_cases: int
    l_correct: float
    l_decay: float


@dataclass(frozen=True)
class H3Result:
    """H3 lifts (derived from H2's per-variant aggregates)."""
    l_correct: float
    l_decay: float
    per_era: dict[str, H3PerEra]


@dataclass(frozen=True)
class RunReport:
    """Top-level eval run output."""
    run_id: str
    config_hash: str
    n_cases: int
    n_held_out: int
    h1: H1Result
    h2: H2Result
    h3: H3Result
    failures: tuple[CaseFailure, ...]
    # Settle config (P4, Task 7): recorded verbatim so runs are
    # self-describing -- an operator reading report.json can tell which
    # run-matrix cell ({baseline, expand, channel@w}) produced it without
    # reversing config_hash. Defaults keep pre-P4 reports constructible
    # and their JSON round-trippable (missing keys reconstruct to these).
    settle: str = "off"
    activation_weight: float | None = None


def report_to_json(report: RunReport) -> str:
    """Serialize `report` to a JSON string. Tuple subfields become JSON arrays;
    dict subfields are preserved verbatim."""
    return json.dumps(asdict(report), indent=2)


def report_from_json(s: str) -> RunReport:
    """Reconstruct a `RunReport` from JSON. Raises (KeyError/TypeError/ValueError)
    on malformed input -- we want loud failures, not silent garbage."""
    data = json.loads(s)
    return _reconstruct(RunReport, data)


def _reconstruct(cls, data):
    """Generic dataclass reconstruction by walking declared field types.

    `from __future__ import annotations` makes field annotations strings,
    so resolve them via `typing.get_type_hints()` once per class."""
    if not is_dataclass(cls):
        return data
    hints = typing.get_type_hints(cls)
    kwargs = {}
    for f in fields(cls):
        if f.name not in data and f.default is not MISSING:
            # Fields added after a report was written (e.g. the P4 settle
            # block) reconstruct to their declared default so legacy
            # report.json files stay loadable. Required (default-less)
            # fields still fail loud below.
            kwargs[f.name] = f.default
            continue
        v = data[f.name]  # KeyError on missing field -- raise loud
        kwargs[f.name] = _coerce(hints[f.name], v)
    return cls(**kwargs)


def _coerce(typ, v):
    """Coerce a raw JSON value back into the typed shape declared on a field."""
    origin = getattr(typ, "__origin__", None)
    # tuple[CaseFailure, ...] -> tuple of reconstructed CaseFailures
    if origin is tuple:
        (inner_t, *_) = typ.__args__
        return tuple(_reconstruct(inner_t, x) for x in v)
    # dict[str, H3PerEra] -> dict with reconstructed values
    if origin is dict:
        _, value_t = typ.__args__
        return {k: _reconstruct(value_t, x) for k, x in v.items()}
    # nested dataclass field
    if is_dataclass(typ):
        return _reconstruct(typ, v)
    # leaf (str, int, float, etc.)
    return v
