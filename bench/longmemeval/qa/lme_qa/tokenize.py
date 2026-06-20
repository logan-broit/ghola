"""Budgeting tokenizer for the rate-distortion compressors.

This token count is an approximate control knob only: the compressor uses it to
hit a target budget, but the plotted rate axis is the reader's real
``usage.input_tokens`` (Claude's own tokens), never this estimate. Kept
dependency-light and pluggable behind a Protocol so a tiktoken-backed impl can
drop in later without touching the compressors.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable


@runtime_checkable
class Tokenizer(Protocol):
    """Counts and truncates text against a token budget."""

    def count(self, text: str) -> int:
        ...

    def truncate(self, text: str, max_tokens: int) -> str:
        ...


class CharRatioTokenizer:
    """Estimates tokens as ``len(text) // chars_per_token``.

    Default ratio 4 is a reasonable English-ish chars-per-token; it is only an
    approximation (the real rate is measured downstream). A tiktoken-backed
    Tokenizer can replace this behind the Protocol with no compressor changes.
    """

    def __init__(self, chars_per_token: int = 4) -> None:
        # A non-positive ratio would make count() meaningless / divide-by-zero.
        if chars_per_token <= 0:
            raise ValueError("chars_per_token must be positive")
        self.chars_per_token = chars_per_token

    def count(self, text: str) -> int:
        return len(text) // self.chars_per_token

    def truncate(self, text: str, max_tokens: int) -> str:
        # Already under budget: leave intact so identity paths stay byte-exact.
        if self.count(text) <= max_tokens:
            return text
        return text[: max_tokens * self.chars_per_token]
