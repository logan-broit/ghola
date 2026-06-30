"""Local LM logprob client. Pure parse-function tests only -- the urllib client
mirrors scorer.py's _post_json (already covered against a real socket in
test_scorer.py), so there is NO network here. The live completions path gets a
separate manual smoke script (scripts/oracle-smoke.py)."""

from __future__ import annotations

from lme_qa.local_lm import parse_prompt_logprobs


def test_parse_extracts_token_logprobs():
    resp = {"choices": [{"prompt_logprobs": [None,
        {"464": {"logprob": -2.5, "decoded_token": "Port", "rank": 1}}]}]}
    assert parse_prompt_logprobs(resp) == [("Port", -2.5)]


def test_parse_picks_rank1_when_multiple():
    resp = {"choices": [{"prompt_logprobs": [None,
        {"5": {"logprob": -0.1, "decoded_token": "the", "rank": 2},
         "9": {"logprob": -3.0, "decoded_token": "Quux", "rank": 1}}]}]}
    assert parse_prompt_logprobs(resp) == [("Quux", -3.0)]


def test_parse_empty_or_allnull():
    assert parse_prompt_logprobs({"choices": [{"prompt_logprobs": [None]}]}) == []


def test_parse_no_rank1_falls_back_to_max_logprob():
    # No entry has rank == 1 -> pick the max-logprob entry (least surprising
    # among the candidates we were given). -0.5 > -3.0, so "the" wins.
    resp = {"choices": [{"prompt_logprobs": [None,
        {"5": {"logprob": -0.5, "decoded_token": "the", "rank": 2},
         "9": {"logprob": -3.0, "decoded_token": "Quux", "rank": 3}}]}]}
    assert parse_prompt_logprobs(resp) == [("the", -0.5)]


def test_parse_multiple_positions_in_order():
    resp = {"choices": [{"prompt_logprobs": [None,
        {"1": {"logprob": -1.0, "decoded_token": "a", "rank": 1}},
        {"2": {"logprob": -2.0, "decoded_token": "b", "rank": 1}}]}]}
    assert parse_prompt_logprobs(resp) == [("a", -1.0), ("b", -2.0)]
