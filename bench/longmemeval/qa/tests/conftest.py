"""Shared fixtures: small dataset entries + retrieve lines mirroring the real
LongMemEval-S schema (verified against
~/longmemeval-ghola/data/longmemeval_s_cleaned.json).
"""

from __future__ import annotations

import pytest


@pytest.fixture
def answerable_entry() -> dict:
    """A non-abstention entry. Aligned haystack lists; the evidence session
    carries an ``answer_*`` id that IS present in the haystack (verified true
    for all 470 non-abs entries in the real dataset).
    """
    return {
        "question_id": "e47becba",
        "question_type": "single-session-user",
        "question": "What degree did I graduate with?",
        "question_date": "2023/05/30 (Tue) 23:40",
        "answer": "Business Administration",
        "answer_session_ids": ["answer_280352e9"],
        "haystack_session_ids": [
            "sharegpt_aaa_0",          # noise, earliest
            "answer_280352e9",         # evidence
            "sharegpt_bbb_1",          # noise, latest
        ],
        "haystack_dates": [
            "2023/05/10 (Wed) 09:00",
            "2023/05/20 (Sat) 02:21",
            "2023/05/29 (Mon) 18:00",
        ],
        "haystack_sessions": [
            [
                {"role": "user", "content": "Solve this river crossing puzzle."},
                {"role": "assistant", "content": "Sure, here is the plan..."},
            ],
            [
                {"role": "user", "content": "I just graduated!"},
                {"role": "assistant", "content": "Congratulations!"},
                {"role": "user", "content": "I earned a Business Administration degree."},
            ],
            [
                {"role": "user", "content": "What's the weather like?"},
                {"role": "assistant", "content": "I can't check live weather."},
            ],
        ],
    }


@pytest.fixture
def abstention_entry() -> dict:
    """An ``_abs`` entry. answer_session_ids point at an ``answer_*_abs`` id
    that is NOT in the haystack (the asked-about evidence is genuinely absent);
    the gold answer is the abstention explanation string.
    """
    return {
        "question_id": "0862e8bf_abs",
        "question_type": "single-session-user",
        "question": "What breed is my dog?",
        "question_date": "2023/06/01 (Thu) 12:00",
        "answer": "You did not mention this information. You mentioned your cat Luna but not a dog.",
        "answer_session_ids": ["answer_c6fd8ebd_abs"],
        "haystack_session_ids": ["sharegpt_ccc_0"],
        "haystack_dates": ["2023/05/15 (Mon) 08:00"],
        "haystack_sessions": [
            [
                {"role": "user", "content": "My cat Luna is so playful."},
                {"role": "assistant", "content": "Cats are wonderful companions."},
            ],
        ],
    }


@pytest.fixture
def answerable_result_line() -> dict:
    """Retrieve line for answerable_entry. ranks deliberately out of order to
    exercise rank-sorting; an unknown id to exercise the skip+count path.
    """
    return {
        "question_id": "e47becba",
        "question_type": "single-session-user",
        "query": "What degree did I graduate with?",
        "results": [
            {"session_id": "sharegpt_bbb_1", "score": 0.40, "rank": 3},
            {"session_id": "answer_280352e9", "score": 0.95, "rank": 1},
            {"session_id": "sharegpt_aaa_0", "score": 0.55, "rank": 2},
            {"session_id": "stale_id_not_in_haystack", "score": 0.10, "rank": 4},
        ],
    }


@pytest.fixture
def abstention_result_line() -> dict:
    return {
        "question_id": "0862e8bf_abs",
        "question_type": "single-session-user",
        "query": "What breed is my dog?",
        "results": [
            {"session_id": "sharegpt_ccc_0", "score": 0.30, "rank": 1},
        ],
    }
