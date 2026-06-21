"""Tokenizers for the rate-distortion instrument.

Two roles, ONE unit. The same tokenizer instance both (a) budgets the
compressors (count text, truncate to a target) and (b) measures the rate axis
(count the emitted context). Using one tokenizer for both means "budget 1000"
and the measured rate share a unit — they are the same count function.

The rate axis is the EMITTED-CONTEXT token count, measured here at reader build
time. (It is NOT the reader's ``usage.input_tokens``: on the claude-code backend
that field is Claude Code's fixed harness overhead — ~3279 tokens regardless of
payload size — so it cannot serve as a rate axis. See rd_aggregate / the bench
README for the root cause.)

``default_tokenizer()`` selects the best available impl: a real cl100k BPE count
(``TiktokenTokenizer``, a proxy for Claude's tokenizer) when the optional
``[rate]`` extra is installed, else the dependency-free ``CharRatioTokenizer``.
CI installs ``[dev]`` only, so the char-ratio fallback is the always-tested
path. Both expose ``.name`` so a recorded rate carries which tokenizer produced
it.
"""

from __future__ import annotations

import importlib.util
from typing import Protocol, runtime_checkable


@runtime_checkable
class Tokenizer(Protocol):
    """Counts and truncates text against a token budget."""

    def count(self, text: str) -> int:
        ...

    def truncate(self, text: str, max_tokens: int) -> str:
        ...

    @property
    def name(self) -> str:
        ...


class CharRatioTokenizer:
    """Estimates tokens as ``len(text) // chars_per_token``.

    Default ratio 4 is a reasonable English-ish chars-per-token. As a budgeting
    knob it is approximate; as the rate axis it is the dependency-free fallback
    (what CI runs, since tiktoken is the optional ``[rate]`` extra). When the
    same instance budgets AND measures, the rate is in the same unit the budget
    was expressed in, so the approximation is internally consistent.
    """

    def __init__(self, chars_per_token: int = 4) -> None:
        # A non-positive ratio would make count() meaningless / divide-by-zero.
        if chars_per_token <= 0:
            raise ValueError("chars_per_token must be positive")
        self.chars_per_token = chars_per_token

    @property
    def name(self) -> str:
        # Encode the ratio so a recorded rate is interpretable after the fact.
        return f"char-ratio:{self.chars_per_token}"

    def count(self, text: str) -> int:
        return len(text) // self.chars_per_token

    def truncate(self, text: str, max_tokens: int) -> str:
        # Already under budget: leave intact so identity paths stay byte-exact.
        if self.count(text) <= max_tokens:
            return text
        return text[: max_tokens * self.chars_per_token]


class TiktokenTokenizer:
    """Real BPE token counts via tiktoken (cl100k_base by default).

    cl100k is OpenAI's encoding, not Claude's — but it is a far closer proxy for
    Claude's tokenizer than a fixed chars-per-token ratio, so it is the better
    rate axis when available. tiktoken is the optional ``[rate]`` extra; it is
    imported LAZILY (inside __init__) so importing this module never requires it
    and ``default_tokenizer()`` can fall back without an import error escaping.
    """

    def __init__(self, encoding: str = "cl100k_base") -> None:
        import tiktoken  # lazy: only the [rate] extra path pays this import

        self._enc = tiktoken.get_encoding(encoding)
        self._encoding = encoding

    @property
    def name(self) -> str:
        # Short, stable label for the curve header / row provenance.
        return "cl100k"

    def count(self, text: str) -> int:
        return len(self._enc.encode(text))

    def truncate(self, text: str, max_tokens: int) -> str:
        ids = self._enc.encode(text)
        # No-op when under budget so identity paths stay byte-exact.
        if len(ids) <= max_tokens:
            return text
        # Cut on a token boundary (slice the ids, then decode), not a byte cut.
        return self._enc.decode(ids[:max_tokens])


def default_tokenizer() -> Tokenizer:
    """The rate/budget tokenizer to use: tiktoken when installed, else char-ratio.

    Checked via ``importlib.util.find_spec`` (no import side effect) so the
    fallback is silent when the ``[rate]`` extra is absent — the CI path. The
    SAME returned instance should drive both budgeting and the rate count so the
    two share a unit.
    """
    if importlib.util.find_spec("tiktoken") is not None:
        return TiktokenTokenizer()
    return CharRatioTokenizer()
