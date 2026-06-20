"""Relevance scorer client tests. The truthsayer/guild HTTP wire path is
exercised against a stdlib fake server (mirror of ``fake_batches_server.py``) --
the real urllib client crosses a real socket. No mocks."""

from __future__ import annotations

import pytest

from lme_qa.scorer import GuildCosineScorer, TruthsayerScorer, make_scorer

from .fake_truthsayer_server import FakeScorerServer


@pytest.fixture
def fake_truthsayer():
    with FakeScorerServer() as srv:
        yield srv


@pytest.fixture
def fake_truthsayer_500():
    with FakeScorerServer(status_override=500) as srv:
        yield srv


def test_truthsayer_scorer_returns_scores_by_id(fake_truthsayer):
    sc = TruthsayerScorer(base_url=fake_truthsayer.url)
    out = sc.score("q", [("a", "alpha"), ("b", "beta")])
    assert set(out) == {"a", "b"}  # dict id -> float
    assert isinstance(out["a"], float) and isinstance(out["b"], float)
    assert out["a"] != out["b"]


def test_truthsayer_callable_interface(fake_truthsayer):
    # The compressor calls the scorer as ``scorer(query, items)`` -- the client
    # must be usable as that bare callable, not only via .score().
    sc = TruthsayerScorer(base_url=fake_truthsayer.url)
    out = sc("q", [("a", "alpha"), ("b", "beta")])
    assert set(out) == {"a", "b"}


def test_truthsayer_empty_items_no_call(fake_truthsayer):
    sc = TruthsayerScorer(base_url=fake_truthsayer.url)
    assert sc.score("q", []) == {}
    # No request should have been sent for an empty candidate list.
    assert fake_truthsayer.state.rerank_payloads == []


def test_scorer_errors_are_raised_not_swallowed(fake_truthsayer_500):
    sc = TruthsayerScorer(base_url=fake_truthsayer_500.url)
    with pytest.raises(RuntimeError):
        sc.score("q", [("a", "x")])


def test_guild_cosine_scorer_returns_scores_by_id(fake_truthsayer):
    sc = GuildCosineScorer(base_url=fake_truthsayer.url)
    out = sc.score("q", [("a", "alpha"), ("b", "beta")])
    assert set(out) == {"a", "b"}
    assert isinstance(out["a"], float) and isinstance(out["b"], float)
    # Cosine of a unit query against two distinct unit vectors differs.
    assert out["a"] != out["b"]


def test_guild_errors_are_raised_not_swallowed(fake_truthsayer_500):
    sc = GuildCosineScorer(base_url=fake_truthsayer_500.url)
    with pytest.raises(RuntimeError):
        sc.score("q", [("a", "x")])


def test_make_scorer_factory_defaults_truthsayer():
    assert isinstance(make_scorer(), TruthsayerScorer)
    assert isinstance(make_scorer("truthsayer"), TruthsayerScorer)
    assert isinstance(make_scorer("guild"), GuildCosineScorer)


def test_make_scorer_unknown_raises():
    with pytest.raises(ValueError):
        make_scorer("nope")


def test_make_scorer_passes_base_url():
    # The sweep points the factory at the live stack via base_url kwarg.
    sc = make_scorer("truthsayer", base_url="http://example:9999")
    assert sc.base_url == "http://example:9999"
