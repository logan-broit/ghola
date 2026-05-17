"""Pure-function tests for the close-directive parser.

The parser is the only piece of B6 worth unit-testing — link resolution as a
whole is observed end-to-end via the real-API smoke. These tests pin the
exact regex contract: keyword as a word, whitespace, then `#<digits>`.
"""
from __future__ import annotations

from seeding_eval.parse import parse_closes_directives


def test_single_closes():
    assert parse_closes_directives("Closes #123") == {123}


def test_single_fixes():
    assert parse_closes_directives("Fixes #456") == {456}


def test_resolves_keyword():
    assert parse_closes_directives("Resolves #789") == {789}


def test_multiple_in_one_message():
    body = "Fixes #123. Also resolves #456."
    assert parse_closes_directives(body) == {123, 456}


def test_case_insensitive():
    assert parse_closes_directives("CLOSES #1, fixes #2, RESOLVES #3") == {1, 2, 3}


def test_word_boundary():
    # Don't match words that contain the keyword as a substring
    assert parse_closes_directives("foreclosesthing #99") == set()
    assert parse_closes_directives("closes#100") == set()  # need a space


def test_ignores_other_text():
    body = """
    This PR refactors the foo module.

    Some context: see #50 for background (NOT a close-link).

    Fixes #51
    Closes #52
    """
    assert parse_closes_directives(body) == {51, 52}


def test_handles_pr_close_directives_too():
    # GH supports: close, closes, closed, fix, fixes, fixed, resolve, resolves, resolved
    text = "close #1; closed #2; fix #3; fixed #4; resolve #5; resolved #6"
    assert parse_closes_directives(text) == {1, 2, 3, 4, 5, 6}


def test_no_directives():
    assert parse_closes_directives("Just a normal message about #5") == set()


def test_empty_input():
    assert parse_closes_directives("") == set()
    assert parse_closes_directives(None) == set()  # tolerate None — issue bodies can be null
