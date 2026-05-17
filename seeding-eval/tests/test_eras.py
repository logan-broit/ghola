"""Tests for the era_for() pure function.

Boundary semantics: lower-bound inclusive — a timestamp at the exact
release second belongs to the new era, not the prior one.
"""
from __future__ import annotations

from datetime import datetime, timezone

import pytest

from seeding_eval.eras import era_for


def test_era_v14_mid_window():
    # 2024-03-15: between v14 (2023-10-26) and v15 (2024-10-21)
    assert era_for(datetime(2024, 3, 15, tzinfo=timezone.utc)) == "v14"


def test_era_boundary_inclusive_lower():
    # exactly v15 release → v15
    assert era_for(datetime(2024, 10, 21, tzinfo=timezone.utc)) == "v15"


def test_era_boundary_exclusive_upper():
    # one minute before v15 → v14
    assert era_for(datetime(2024, 10, 20, 23, 59, tzinfo=timezone.utc)) == "v14"


def test_era_pre_v12():
    assert era_for(datetime(2020, 1, 1, tzinfo=timezone.utc)) == "pre-v12"


def test_era_v12_at_release():
    assert era_for(datetime(2021, 10, 26, tzinfo=timezone.utc)) == "v12"


def test_era_v16_after_release():
    assert era_for(datetime(2026, 1, 1, tzinfo=timezone.utc)) == "v16"


def test_era_aware_timestamp_required():
    # Naive datetimes are ambiguous — must reject loudly, not silently coerce.
    with pytest.raises((ValueError, TypeError)):
        era_for(datetime(2024, 3, 15))
