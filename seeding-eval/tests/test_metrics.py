from __future__ import annotations

import math

import pytest

from seeding_eval.metrics import (
    p_at_5,
    shannon_entropy,
    compute_h3_lifts,
)


# ---------- C3: P@5 ----------

def test_p_at_5_hit_at_position_0():
    assert p_at_5(["e1", "e2"], ["e1", "x", "y", "z", "w"]) == 1.0


def test_p_at_5_hit_at_position_4():
    assert p_at_5(["e9"], ["a", "b", "c", "d", "e9"]) == 1.0


def test_p_at_5_miss_just_outside_top5():
    # ground truth at position 5 (0-indexed) → not in [0:5] → miss
    assert p_at_5(["e9"], ["a", "b", "c", "d", "e", "e9"]) == 0.0


def test_p_at_5_no_intersection():
    assert p_at_5(["x"], ["a", "b", "c", "d", "e"]) == 0.0


def test_p_at_5_multiple_ground_truths_any_hit():
    # Any one of the GT ids in top-5 → hit
    assert p_at_5(["x", "y", "e3"], ["e1", "e2", "e3", "e4", "e5"]) == 1.0


def test_p_at_5_empty_top_k():
    assert p_at_5(["e1"], []) == 0.0


def test_p_at_5_empty_ground_truth_raises():
    with pytest.raises(ValueError):
        p_at_5([], ["a", "b", "c", "d", "e"])


def test_p_at_5_top_k_shorter_than_5():
    # No padding — just check intersection with however many results we have
    assert p_at_5(["e1"], ["e1", "x"]) == 1.0
    assert p_at_5(["e9"], ["a", "b"]) == 0.0


# ---------- C4: Shannon entropy ----------

def test_h1_uniform_4_distinct_buckets_is_max_entropy():
    # 4 distinct, uniform → log2(4) = 2.0
    assert math.isclose(shannon_entropy(["a", "b", "c", "d"]), 2.0)


def test_h1_uniform_2_distinct_50_50():
    # 50/50 → log2(2) = 1.0
    assert math.isclose(shannon_entropy(["a", "b", "a", "b"]), 1.0)


def test_h1_all_same_bucket_is_zero():
    assert shannon_entropy(["a", "a", "a", "a"]) == 0.0


def test_h1_uneven_distribution():
    # 3:1 → -(0.75*log2(0.75) + 0.25*log2(0.25))
    expected = -(0.75 * math.log2(0.75) + 0.25 * math.log2(0.25))
    assert math.isclose(shannon_entropy(["a", "a", "a", "b"]), expected)


def test_h1_empty_raises():
    with pytest.raises(ValueError):
        shannon_entropy([])


def test_h1_single_element_is_zero():
    assert shannon_entropy(["only"]) == 0.0


# ---------- C5: H3 lifts ----------

def test_h3_lifts_zero_when_all_equal():
    lifts = compute_h3_lifts(p_none=0.5, p_correct=0.5, p_wrong=0.5)
    assert lifts.l_correct == 0.0
    assert lifts.l_decay == 0.0


def test_h3_lifts_correct_helps():
    lifts = compute_h3_lifts(p_none=0.4, p_correct=0.7, p_wrong=0.4)
    assert math.isclose(lifts.l_correct, 0.3)
    assert math.isclose(lifts.l_decay, 0.3)


def test_h3_lifts_negative_decay():
    # System surfaces wrong-era results MORE often than correct-era → bad
    lifts = compute_h3_lifts(p_none=0.5, p_correct=0.4, p_wrong=0.6)
    assert math.isclose(lifts.l_correct, -0.1)
    assert math.isclose(lifts.l_decay, -0.2)


def test_h3_lifts_input_validation():
    # P@5 values must be in [0, 1]
    with pytest.raises(ValueError):
        compute_h3_lifts(p_none=1.1, p_correct=0.5, p_wrong=0.5)
    with pytest.raises(ValueError):
        compute_h3_lifts(p_none=-0.1, p_correct=0.5, p_wrong=0.5)
