"""Context building: alignment, chronological ordering, truncation, unknowns."""

from __future__ import annotations

from lme_qa.context import DEFAULT_MAX_SESSION_CHARS, build_context


def test_top_k_sessions_rendered_chronologically(
    answerable_entry, answerable_result_line
):
    ctx = build_context(answerable_entry, answerable_result_line, k=10)
    # 3 known sessions used, 1 stale id skipped.
    assert ctx.used_session_ids == [
        "sharegpt_aaa_0",
        "answer_280352e9",
        "sharegpt_bbb_1",
    ] or set(ctx.used_session_ids) == {
        "sharegpt_aaa_0",
        "answer_280352e9",
        "sharegpt_bbb_1",
    }
    assert ctx.unknown_session_ids == ["stale_id_not_in_haystack"]

    # Date headers appear oldest-first regardless of retrieval rank order.
    text = ctx.text
    i_05_10 = text.index("2023/05/10")
    i_05_20 = text.index("2023/05/20")
    i_05_29 = text.index("2023/05/29")
    assert i_05_10 < i_05_20 < i_05_29


def test_alignment_session_id_maps_to_right_text(
    answerable_entry, answerable_result_line
):
    ctx = build_context(answerable_entry, answerable_result_line, k=10)
    # The evidence session (answer_280352e9, dated 05/20) must carry the
    # graduation text, not a neighbor's puzzle/weather text.
    block = ctx.text.split("=== Session dated 2023/05/20")[1].split("=== Session")[0]
    assert "Business Administration degree" in block
    assert "river crossing" not in block
    assert "weather" not in block


def test_turns_render_as_user_assistant_lines(
    answerable_entry, answerable_result_line
):
    ctx = build_context(answerable_entry, answerable_result_line, k=10)
    assert "USER: I just graduated!" in ctx.text
    assert "ASSISTANT: Congratulations!" in ctx.text


def test_k_limits_number_of_sessions(answerable_entry, answerable_result_line):
    ctx = build_context(answerable_entry, answerable_result_line, k=1)
    # Only rank-1 (answer_280352e9) survives the top-k slice.
    assert ctx.used_session_ids == ["answer_280352e9"]
    assert ctx.unknown_session_ids == []  # stale id was rank 4, sliced off
    assert "2023/05/10" not in ctx.text
    assert "2023/05/29" not in ctx.text


def test_unknown_session_ids_skipped_and_counted(answerable_entry):
    line = {
        "results": [
            {"session_id": "answer_280352e9", "rank": 1},
            {"session_id": "ghost_a", "rank": 2},
            {"session_id": "ghost_b", "rank": 3},
        ]
    }
    ctx = build_context(answerable_entry, line, k=10)
    assert ctx.used_session_ids == ["answer_280352e9"]
    assert ctx.unknown_session_ids == ["ghost_a", "ghost_b"]


def test_max_session_chars_truncates_only_via_cap(
    answerable_entry, answerable_result_line
):
    # With a tiny cap, every multi-turn session body exceeds it.
    ctx = build_context(
        answerable_entry, answerable_result_line, k=10, max_session_chars=20
    )
    assert set(ctx.truncated_session_ids) == set(ctx.used_session_ids)
    # No truncation at the generous default for these small fixtures.
    ctx2 = build_context(
        answerable_entry,
        answerable_result_line,
        k=10,
        max_session_chars=DEFAULT_MAX_SESSION_CHARS,
    )
    assert ctx2.truncated_session_ids == []


def test_abstention_entry_renders_only_noise(
    abstention_entry, abstention_result_line
):
    ctx = build_context(abstention_entry, abstention_result_line, k=10)
    assert ctx.used_session_ids == ["sharegpt_ccc_0"]
    assert "cat Luna" in ctx.text
    # The asked-about evidence (dog breed) is genuinely absent from context.
    assert "dog" not in ctx.text.lower()


def test_duplicate_result_ids_deduped(answerable_entry):
    line = {
        "results": [
            {"session_id": "answer_280352e9", "rank": 1},
            {"session_id": "answer_280352e9", "rank": 2},
        ]
    }
    ctx = build_context(answerable_entry, line, k=10)
    assert ctx.used_session_ids == ["answer_280352e9"]
    assert ctx.text.count("=== Session dated 2023/05/20") == 1
