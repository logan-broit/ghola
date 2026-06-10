"""Prompt rendering: reader shape + verbatim upstream judge templates."""

from __future__ import annotations

import pytest

from lme_qa.prompts import (
    build_reader_prompt,
    get_anscheck_prompt,
    is_abstention,
    parse_judge_label,
)


def test_reader_prompt_carries_question_date_and_context():
    p = build_reader_prompt(
        "What degree did I graduate with?",
        "2023/05/30 (Tue) 23:40",
        "=== Session dated 2023/05/20 ===\nUSER: hi",
    )
    assert "Current date: 2023/05/30 (Tue) 23:40" in p
    assert "Question: What degree did I graduate with?" in p
    assert "=== Session dated 2023/05/20 ===" in p


# --- Judge templates: golden, verbatim upstream ----------------------------


def test_default_template_for_single_and_multi_session():
    for task in ("single-session-user", "single-session-assistant", "multi-session"):
        p = get_anscheck_prompt(task, "Q?", "A", "R")
        assert p.startswith(
            "I will give you a question, a correct answer, and a response from a model."
        )
        assert "Question: Q?" in p
        assert "Correct Answer: A" in p
        assert "Model Response: R" in p
        assert p.endswith("Is the model response correct? Answer yes or no only.")
        # The default template carries no temporal off-by-one clause.
        assert "off-by-one" not in p


def test_temporal_template_has_off_by_one_clause():
    p = get_anscheck_prompt("temporal-reasoning", "Q?", "A", "R")
    assert "do not penalize off-by-one errors for the number of days" in p
    assert "predicting 19 days when the answer is 18" in p


def test_knowledge_update_template_mentions_updated_answer():
    p = get_anscheck_prompt("knowledge-update", "Q?", "A", "R")
    assert "previous information along with an updated answer" in p


def test_preference_template_uses_rubric_label():
    p = get_anscheck_prompt("single-session-preference", "Q?", "RUBRIC", "R")
    # Preference uses "Rubric:" not "Correct Answer:" — verbatim upstream.
    assert "Rubric: RUBRIC" in p
    assert "Correct Answer:" not in p
    assert "rubric for desired personalized response" in p


def test_abstention_template_used_when_flagged():
    p = get_anscheck_prompt(
        "single-session-user", "Q?", "EXPLANATION", "R", abstention=True
    )
    assert "unanswerable question" in p
    assert "Explanation: EXPLANATION" in p
    assert p.endswith(
        "Does the model correctly identify the question as unanswerable? "
        "Answer yes or no only."
    )


def test_unknown_task_raises():
    with pytest.raises(NotImplementedError):
        get_anscheck_prompt("not-a-real-task", "Q?", "A", "R")


def test_is_abstention_matches_upstream_rule():
    assert is_abstention("0862e8bf_abs") is True
    assert is_abstention("e47becba") is False


def test_parse_judge_label_leading_token_rule():
    # Leading-token check (stricter than upstream's bare substring): the reply
    # must START with "yes". Our judge isn't max_tokens-capped to ~10, so
    # adaptive thinking could surface a preamble containing "yes" inside a "no".
    assert parse_judge_label("yes") is True
    assert parse_judge_label("Yes.") is True
    assert parse_judge_label("YES, correct") is True
    assert parse_judge_label("no") is False
    assert parse_judge_label("No, the response is wrong.") is False
    # The crux: a "no" verdict that merely mentions "yes" must parse as False.
    assert parse_judge_label("No, because yes would require the date.") is False
    # Leading punctuation/whitespace is stripped before the token check.
    assert parse_judge_label(" no") is False
    assert parse_judge_label("**Yes**") is True
    assert parse_judge_label('"yes"') is True
    assert parse_judge_label("  Yes, the response matches.") is True
    # Empty / unparseable -> not "yes" -> False.
    assert parse_judge_label("") is False
    assert parse_judge_label("maybe") is False
