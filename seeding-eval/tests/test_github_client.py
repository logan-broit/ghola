"""Tests for the pure-function bits of seeding_eval.github_client.

GHClient itself is a thin PyGithub wrapper exercised by integration paths
(extract_via_merged_prs cache-miss is the production caller). The pure
ranking logic (rank_top_comment) is tested here against synthetic comment
objects — these are inputs to the function, not mocks of the function
under test.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone

from seeding_eval.github_client import rank_top_comment


@dataclass
class _StubUser:
    login: str


@dataclass
class _StubComment:
    body: str
    user: _StubUser | None
    created_at: datetime
    reactions: dict = field(default_factory=dict)


def _c(
    body: str,
    user: str,
    created: str,
    plus_one: int = 0,
    minus_one: int = 0,
    extra: dict | None = None,
) -> _StubComment:
    """Build a comment-shaped object the ranker can consume."""
    reactions = {"+1": plus_one, "-1": minus_one}
    if extra:
        reactions.update(extra)
    return _StubComment(
        body=body,
        user=_StubUser(login=user),
        created_at=datetime.fromisoformat(created).replace(tzinfo=timezone.utc),
        reactions=reactions,
    )


def test_rank_top_comment_returns_none_on_empty_input():
    assert rank_top_comment([]) is None


def test_rank_top_comment_returns_none_when_no_plus_ones():
    """Comments with zero +1 reactions are not signal — return None even
    when many comments exist."""
    comments = [
        _c("me too", "alice", "2024-03-15T10:00:00", plus_one=0),
        _c("same here", "bob", "2024-03-16T10:00:00", plus_one=0, minus_one=2),
    ]
    assert rank_top_comment(comments) is None


def test_rank_top_comment_picks_max_plus_one():
    """Highest +1 count wins. Ignore other reaction kinds."""
    comments = [
        _c("first", "alice", "2024-03-15T10:00:00", plus_one=2),
        _c("winner", "carol", "2024-03-15T11:00:00", plus_one=12),
        _c("third", "dave", "2024-03-15T12:00:00", plus_one=5, extra={"laugh": 99}),
    ]
    top = rank_top_comment(comments)
    assert top is not None
    assert top["user"] == "carol"
    assert top["body"] == "winner"
    assert top["plus_one_count"] == 12
    assert top["created_at"] == "2024-03-15T11:00:00+00:00"


def test_rank_top_comment_breaks_ties_by_most_recent():
    """When two comments have the same +1 count, prefer the more recent one."""
    comments = [
        _c("older", "alice", "2024-03-15T10:00:00", plus_one=5),
        _c("newer", "bob", "2024-03-16T10:00:00", plus_one=5),
        _c("oldest", "carol", "2024-03-14T10:00:00", plus_one=5),
    ]
    top = rank_top_comment(comments)
    assert top is not None
    assert top["user"] == "bob"


def test_rank_top_comment_skips_bot_authors():
    """Bot-authored comments are skipped regardless of +1 count."""
    comments = [
        _c("auto-cleanup", "github-actions[bot]", "2024-03-15T10:00:00", plus_one=99),
        _c("real signal", "alice", "2024-03-15T11:00:00", plus_one=2),
    ]
    top = rank_top_comment(comments)
    assert top is not None
    assert top["user"] == "alice"
    assert top["plus_one_count"] == 2


def test_rank_top_comment_caps_body_at_2000_chars():
    """Bodies longer than 2000 chars are truncated to keep event size bounded."""
    long_body = "x" * 5000
    comments = [_c(long_body, "alice", "2024-03-15T10:00:00", plus_one=3)]
    top = rank_top_comment(comments)
    assert top is not None
    assert len(top["body"]) == 2000


def test_rank_top_comment_strips_whitespace_before_capping():
    """Leading/trailing whitespace stripped before the 2000-char cap."""
    comments = [
        _c("   hello world   \n", "alice", "2024-03-15T10:00:00", plus_one=3),
    ]
    top = rank_top_comment(comments)
    assert top is not None
    assert top["body"] == "hello world"


def test_rank_top_comment_returned_record_shape():
    """Pin the record shape: exactly four keys."""
    comments = [_c("body", "alice", "2024-03-15T10:00:00", plus_one=3)]
    top = rank_top_comment(comments)
    assert top is not None
    assert set(top.keys()) == {"body", "user", "created_at", "plus_one_count"}
