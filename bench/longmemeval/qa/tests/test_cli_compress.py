"""Reader wiring of the compressor (Task 6), backward-compatible.

Two guarantees this file pins:

  - ``--compressor full`` (the default) builds a reader prompt BYTE-IDENTICAL
    to the pre-change path (``build_context(...).text`` straight into
    ``build_reader_prompt``). This is the golden test: the instrument's right
    edge must reproduce production exactly or the curve's baseline is wrong.
  - ``--compressor truncate_tokens --budget N`` yields a SHORTER reader prompt
    (the budget actually bites) and persists ``compressor`` + ``budget`` into
    each reader row alongside ``k`` for provenance.

Driven through the cc backend against the fake claude binary (no live model).
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lme_qa import cli, context, prompts
from lme_qa.tokenize import CharRatioTokenizer
from tests.fake_claude_bin import read_stdin_log, write_fake_claude


@pytest.fixture
def dataset_file(tmp_path, answerable_entry, abstention_entry) -> Path:
    p = tmp_path / "dataset.json"
    p.write_text(json.dumps([answerable_entry, abstention_entry]))
    return p


@pytest.fixture
def run_file(tmp_path, answerable_result_line, abstention_result_line) -> Path:
    p = tmp_path / "run.jsonl"
    p.write_text(
        json.dumps(answerable_result_line) + "\n" + json.dumps(abstention_result_line) + "\n"
    )
    return p


def _use_fake(monkeypatch, tmp_path, scenario) -> Path:
    bindir = tmp_path / "bin"
    binpath = write_fake_claude(bindir, scenario)
    monkeypatch.setenv("LME_QA_CLAUDE_BIN", str(binpath))
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    return bindir


def _expected_full_prompt(entry: dict, line: dict, k: int = 10) -> str:
    """The reader user prompt the PRE-CHANGE path produced: build_context's
    rendered text straight into build_reader_prompt. The golden reference."""
    built = context.build_context(entry, line, k=k)
    return prompts.build_reader_prompt(
        entry["question"], entry["question_date"], built.text
    )


# --- Task 6: select_sessions exposes the chronologically-sorted Session list


def test_select_sessions_matches_build_context_render(
    answerable_entry, answerable_result_line
):
    sessions, diag = context.select_sessions(
        answerable_entry, answerable_result_line, k=10
    )
    # The selected sessions, rendered, equal build_context's text exactly.
    text, _ = context.render_sessions(sessions)
    assert text == context.build_context(answerable_entry, answerable_result_line, k=10).text
    # Diagnostics carry the same used/unknown ids build_context reports.
    built = context.build_context(answerable_entry, answerable_result_line, k=10)
    assert diag.used_session_ids == built.used_session_ids
    assert diag.unknown_session_ids == built.unknown_session_ids


# --- Task 6: --compressor full is byte-identical to the pre-change path ------


def test_reader_compressor_full_is_byte_identical(
    monkeypatch, dataset_file, run_file, tmp_path, answerable_entry, answerable_result_line
):
    bindir = _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})
    answers = tmp_path / "answers.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--k", "10",
            "--compressor", "full",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0

    # The user prompt that reached the binary on stdin must equal the golden
    # pre-change reader prompt for the answerable question, byte for byte.
    expected = _expected_full_prompt(answerable_entry, answerable_result_line)
    stdins = read_stdin_log(bindir)
    assert expected in stdins, "full compressor reader prompt diverged from pre-change path"

    # Provenance: compressor + budget persisted into each reader row.
    rows = [json.loads(l) for l in answers.read_text().splitlines() if l.strip()]
    assert all(r["compressor"] == "full" for r in rows)
    assert all(r["budget"] is None for r in rows)
    assert all(r["k"] == 10 for r in rows)


def test_reader_default_compressor_is_full(
    monkeypatch, dataset_file, run_file, tmp_path, answerable_entry, answerable_result_line
):
    # No --compressor flag at all: default must be full, byte-identical.
    bindir = _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})
    answers = tmp_path / "answers.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0
    expected = _expected_full_prompt(answerable_entry, answerable_result_line)
    assert expected in read_stdin_log(bindir)
    rows = [json.loads(l) for l in answers.read_text().splitlines() if l.strip()]
    assert all(r["compressor"] == "full" and r["budget"] is None for r in rows)


# --- Task 6: --compressor truncate_tokens --budget N shortens the prompt -----


def test_reader_truncate_budget_shortens_prompt(
    monkeypatch, dataset_file, run_file, tmp_path, answerable_entry, answerable_result_line
):
    bindir = _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})
    answers = tmp_path / "answers.jsonl"

    # A budget far below the full context cost so truncation clearly bites.
    full = _expected_full_prompt(answerable_entry, answerable_result_line)
    budget = 5  # ~20 chars of context

    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--k", "10",
            "--compressor", "truncate_tokens",
            "--budget", str(budget),
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0

    stdins = read_stdin_log(bindir)
    # The answerable question's prompt is now SHORTER than the full prompt.
    answerable_stdins = [s for s in stdins if "What degree did I graduate with?" in s]
    assert answerable_stdins, "answerable prompt not found on stdin"
    assert all(len(s) < len(full) for s in answerable_stdins)

    rows = [json.loads(l) for l in answers.read_text().splitlines() if l.strip()]
    assert all(r["compressor"] == "truncate_tokens" for r in rows)
    assert all(r["budget"] == budget for r in rows)


# --- rate fix: reader records the emitted-context token count ----------------


def _rows(answers: Path) -> list[dict]:
    return [json.loads(l) for l in answers.read_text().splitlines() if l.strip()]


def _answerable_row(answers: Path) -> dict:
    rows = _rows(answers)
    return next(r for r in rows if r["question_id"] == "e47becba")


def test_reader_full_records_context_tokens_equal_to_full_context(
    monkeypatch, dataset_file, run_file, tmp_path, answerable_entry, answerable_result_line
):
    # context_tokens on a `full` row equals the token count of the full rendered
    # context (NOT the reader prompt wrapper, NOT usage.input_tokens) — it tracks
    # exactly the payload the compressor emitted.
    _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})
    answers = tmp_path / "answers.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--k", "10",
            "--compressor", "full",
            "--rate-tokenizer", "char",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0

    row = _answerable_row(answers)
    # The rate tokenizer is recorded for provenance.
    assert row["rate_tokenizer"] == "char-ratio:4"
    # context_tokens == the rate tokenizer's count of the emitted full context.
    full_context, _ = context.render_sessions(
        context.select_sessions(answerable_entry, answerable_result_line, k=10)[0]
    )
    expected = CharRatioTokenizer().count(full_context)
    assert row["context_tokens"] == expected
    # Sanity: it is NOT the fake binary's constant usage.input_tokens (100).
    assert row["context_tokens"] != row["usage"]["input_tokens"]


def test_context_tokens_varies_full_vs_truncate_same_question(
    monkeypatch, dataset_file, run_file, tmp_path
):
    # THE regression test the bug slipped through three reviews: across two
    # settings for the SAME question, the recorded rate (context_tokens) must
    # TRACK the emitted payload — a `full` row's context_tokens is MUCH larger
    # than a `truncate_tokens --budget 10` row's. usage.input_tokens was
    # constant across settings (harness overhead), which is exactly why it
    # failed as a rate axis; context_tokens must not share that defect.
    _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})
    full_answers = tmp_path / "full.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(full_answers),
            "--k", "10",
            "--compressor", "full",
            "--rate-tokenizer", "char",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0
    # Fresh fake binary (new bindir) for the second run so logs/counter reset.
    _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})

    trunc_answers = tmp_path / "trunc.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(trunc_answers),
            "--k", "10",
            "--compressor", "truncate_tokens",
            "--budget", "10",
            "--rate-tokenizer", "char",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0

    full_ct = _answerable_row(full_answers)["context_tokens"]
    trunc_ct = _answerable_row(trunc_answers)["context_tokens"]
    # truncate@10 is bounded by the budget; full is the whole context.
    assert trunc_ct <= 10
    # "MUCH larger" — full carries materially more payload than the squeezed cut.
    assert full_ct > trunc_ct
    # And, the whole point: the rate moved across settings (unlike the constant
    # usage.input_tokens, which the fake binary reports identically for both).
    full_usage = _answerable_row(full_answers)["usage"]["input_tokens"]
    trunc_usage = _answerable_row(trunc_answers)["usage"]["input_tokens"]
    assert full_usage == trunc_usage  # harness-overhead proxy: constant
    assert full_ct != trunc_ct        # the real rate axis: varies


# --- Stage B: compress_kwargs builder (pure function) ------------------------


import argparse

from lme_qa import distill, local_lm


def _args(**overrides):
    """A minimal args namespace with the Stage B defaults the argparse layer
    sets, overridable per test."""
    base = dict(
        compressor="full",
        scorer="truthsayer",
        expansion_reserve=None,
        query_mode="agnostic",
        output_form="prose",
        prune_level=None,
        oracle_model=None,
        edge_metric="jaccard",
    )
    base.update(overrides)
    return argparse.Namespace(**base)


def test_build_compress_kwargs_full_is_empty():
    # full / truncate_tokens / topk_sessions take no extra kwargs.
    assert cli.build_compress_kwargs(_args(compressor="full")) == {}
    assert cli.build_compress_kwargs(_args(compressor="truncate_tokens")) == {}
    assert cli.build_compress_kwargs(_args(compressor="topk_sessions")) == {}


def test_build_compress_kwargs_extractive_relevance_has_scorer_only():
    kw = cli.build_compress_kwargs(_args(compressor="extractive_relevance"))
    assert set(kw) == {"scorer"}


def test_build_compress_kwargs_extractive_expanded_has_scorer_and_reserve():
    kw = cli.build_compress_kwargs(
        _args(compressor="extractive_relevance_expanded", expansion_reserve=0.4)
    )
    assert set(kw) == {"scorer", "expansion_reserve"}
    assert kw["expansion_reserve"] == 0.4
    # reserve None -> not passed (compressor default stands).
    kw2 = cli.build_compress_kwargs(
        _args(compressor="extractive_relevance_expanded", expansion_reserve=None)
    )
    assert set(kw2) == {"scorer"}


def test_build_compress_kwargs_statistical_prune_query_mode_only():
    kw = cli.build_compress_kwargs(
        _args(compressor="statistical_prune", query_mode="aware")
    )
    assert kw == {"query_mode": "aware"}


def test_build_compress_kwargs_lexical_relevance_is_empty():
    assert cli.build_compress_kwargs(_args(compressor="lexical_relevance")) == {}


def test_build_compress_kwargs_graph_community_query_mode_and_edge_metric():
    kw = cli.build_compress_kwargs(
        _args(compressor="graph_community", query_mode="agnostic", edge_metric="jaccard")
    )
    assert kw == {"query_mode": "agnostic", "edge_metric": "jaccard"}


def test_build_compress_kwargs_perplexity_prune_builds_lm_client():
    kw = cli.build_compress_kwargs(
        _args(
            compressor="perplexity_prune",
            query_mode="agnostic",
            oracle_model="Qwen/Qwen2.5-1.5B-Instruct",
        )
    )
    assert set(kw) == {"lm_client", "query_mode"}
    assert isinstance(kw["lm_client"], local_lm.LocalLMClient)
    # The oracle model flows into the client.
    assert kw["lm_client"].model == "Qwen/Qwen2.5-1.5B-Instruct"
    assert kw["query_mode"] == "agnostic"


def test_build_compress_kwargs_perplexity_prune_default_oracle_model():
    # oracle_model None -> the client's built-in default model is used.
    kw = cli.build_compress_kwargs(
        _args(compressor="perplexity_prune", oracle_model=None)
    )
    assert isinstance(kw["lm_client"], local_lm.LocalLMClient)
    assert kw["lm_client"].model == local_lm.DEFAULT_ORACLE_MODEL


def test_build_compress_kwargs_llm_distill_builds_distiller():
    kw = cli.build_compress_kwargs(
        _args(compressor="llm_distill", output_form="structured", query_mode="aware")
    )
    assert set(kw) == {"distiller", "output_form", "query_mode"}
    assert isinstance(kw["distiller"], distill.Distiller)
    assert kw["output_form"] == "structured"
    assert kw["query_mode"] == "aware"


def test_run_main_parses_stage_b_flags(
    monkeypatch, dataset_file, run_file, tmp_path
):
    # The new flags parse + the run completes through the fake binary. Use
    # statistical_prune (no live oracle / no claude needed) with query_mode.
    _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})
    answers = tmp_path / "answers.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--compressor", "statistical_prune",
            "--budget", "50",
            "--query-mode", "aware",
            "--rate-tokenizer", "char",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0
    rows = _rows(answers)
    assert all(r["compressor"] == "statistical_prune" for r in rows)


def test_reader_rate_tokenizer_auto_default(
    monkeypatch, dataset_file, run_file, tmp_path
):
    # No --rate-tokenizer flag: auto-select (char-ratio here, since CI has no
    # tiktoken). The row still records a context_tokens + a rate_tokenizer name.
    _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})
    answers = tmp_path / "answers.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--compressor", "full",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0
    row = _answerable_row(answers)
    assert row["context_tokens"] > 0
    assert isinstance(row["rate_tokenizer"], str) and row["rate_tokenizer"]
