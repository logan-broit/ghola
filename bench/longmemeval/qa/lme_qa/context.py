"""Build reader context from a question's top-K retrieved sessions.

The retrieve stage emits one JSONL line per question:
``{question_id, question_type, query, results: [{session_id, score, rank}]}``.
This module maps those retrieved session_ids back onto the dataset entry's
aligned ``haystack_session_ids`` / ``haystack_sessions`` / ``haystack_dates``
lists, renders the matched sessions chronologically (by haystack date) with a
per-session date header, and turns each session's turns into ``USER:`` /
``ASSISTANT:`` lines.

Verified against the real LongMemEval-S dataset:
  - the aligned haystack lists are equal-length (session_id[i] <-> sessions[i]
    <-> dates[i]);
  - ``answer_*``-prefixed session_ids ARE present in the haystack lists, so a
    retrieved evidence session maps cleanly — only genuinely-absent ids are
    skipped, with a count returned to the caller for visibility.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Any

# Haystack dates look like "2023/05/20 (Sat) 02:21". Parse for chronological
# ordering; fall back to lexicographic on the raw string if a date ever fails
# to parse (the field is still YYYY/MM/... leading, so lexicographic order is a
# safe degradation rather than a crash).
_DATE_FMT = "%Y/%m/%d (%a) %H:%M"

# Generous per-session character cap. p99 of real LongMemEval-S sessions is
# ~20.7k chars and the Opus 4.8 context window is 1M tokens, so this rarely
# bites; it exists only to bound a pathological runaway session. Pinned in the
# bench README.
DEFAULT_MAX_SESSION_CHARS = 24_000


@dataclass(frozen=True)
class Session:
    """One rendered haystack session: its date and conversational turns."""

    session_id: str
    date: str
    turns: list[dict[str, Any]]


@dataclass(frozen=True)
class BuiltContext:
    """Result of build_context: the rendered text plus diagnostics."""

    text: str
    used_session_ids: list[str]
    unknown_session_ids: list[str]
    truncated_session_ids: list[str]


def _sort_key(date: str) -> tuple[int, Any]:
    """Chronological sort key for a haystack date string.

    Returns (0, datetime) when parseable so real timestamps sort ahead of
    unparseable ones; (1, raw_string) otherwise, keeping ordering total and
    deterministic without raising.
    """
    try:
        return (0, datetime.strptime(date, _DATE_FMT))
    except ValueError:
        return (1, date)


def _render_turn(turn: dict[str, Any]) -> str:
    """Render one turn as a ``ROLE: content`` line.

    LongMemEval turns are ``{"role": "user"|"assistant", "content": ...}``.
    Anything other than assistant is labelled USER (the dataset only carries
    user/assistant turns; an unexpected role degrades to USER rather than
    leaking a raw role string into the reader prompt).
    """
    role = (turn.get("role") or "user").lower()
    label = "ASSISTANT" if role == "assistant" else "USER"
    content = turn.get("content", "")
    return f"{label}: {content}"


def _render_session(session: Session, max_session_chars: int) -> tuple[str, bool]:
    """Render a session to text. Returns (text, truncated?).

    Truncation applies only via the explicit ``max_session_chars`` cap, on the
    joined turn body (the date header is never truncated).
    """
    header = f"=== Session dated {session.date} ==="
    body = "\n".join(_render_turn(t) for t in session.turns)
    truncated = False
    if max_session_chars and len(body) > max_session_chars:
        body = body[:max_session_chars]
        truncated = True
    return f"{header}\n{body}", truncated


def render_sessions(
    sessions: list[Session],
    max_session_chars: int = DEFAULT_MAX_SESSION_CHARS,
) -> tuple[str, list[str]]:
    """Render a list of sessions to the canonical reader text.

    Returns ``(text, truncated_session_ids)``. The text is the per-session
    ``=== Session dated ... ===`` blocks joined by a blank line. This is the
    single source of truth for the reader's rendering: both ``build_context``
    and the rate-distortion compressors call it so their output cannot diverge.
    Sessions are rendered in the order given (callers sort chronologically
    before calling).
    """
    rendered: list[str] = []
    truncated: list[str] = []
    for s in sessions:
        text, was_truncated = _render_session(s, max_session_chars)
        rendered.append(text)
        if was_truncated:
            truncated.append(s.session_id)
    return "\n\n".join(rendered), truncated


def build_context(
    entry: dict[str, Any],
    result_line: dict[str, Any],
    k: int = 10,
    max_session_chars: int = DEFAULT_MAX_SESSION_CHARS,
) -> BuiltContext:
    """Assemble reader context for one question.

    ``entry`` is a dataset record (dict with the aligned ``haystack_*`` lists).
    ``result_line`` is the retrieve JSONL line for the same question. We take
    the top-``k`` results (by their ``rank``, ascending), map each onto the
    haystack, render the matched sessions chronologically, and join them.

    A retrieved session_id absent from the haystack is dropped and recorded in
    ``unknown_session_ids`` — this should be empty in practice (verified: the
    evidence ``answer_*`` ids live in the haystack lists), but a backend that
    emits a stale or cross-question id won't silently corrupt the context.
    """
    # Aligned lookup: session_id -> (date, turns). Equal-length lists are an
    # invariant of the dataset; zip stops at the shortest as a defensive guard.
    by_id: dict[str, tuple[str, list[dict[str, Any]]]] = {}
    for sid, date, turns in zip(
        entry["haystack_session_ids"],
        entry["haystack_dates"],
        entry["haystack_sessions"],
    ):
        # First occurrence wins (session_ids are unique within an entry).
        by_id.setdefault(sid, (date, turns))

    # Top-k retrieved results, ordered by rank ascending. We sort defensively
    # rather than trusting input order, then slice.
    results = sorted(
        result_line.get("results", []),
        key=lambda r: r.get("rank", 10**9),
    )[:k]

    used: list[str] = []
    unknown: list[str] = []
    sessions: list[Session] = []
    seen: set[str] = set()
    for r in results:
        sid = r.get("session_id")
        if sid is None or sid in seen:
            continue
        seen.add(sid)
        hit = by_id.get(sid)
        if hit is None:
            unknown.append(sid)
            continue
        date, turns = hit
        used.append(sid)
        sessions.append(Session(session_id=sid, date=date, turns=turns))

    # Render chronologically by haystack date (oldest first) so the reader sees
    # the conversation timeline in order — matters for temporal-reasoning and
    # knowledge-update questions where recency disambiguates the answer.
    sessions.sort(key=lambda s: _sort_key(s.date))

    text, truncated = render_sessions(sessions, max_session_chars)

    return BuiltContext(
        text=text,
        used_session_ids=used,
        unknown_session_ids=unknown,
        truncated_session_ids=truncated,
    )
