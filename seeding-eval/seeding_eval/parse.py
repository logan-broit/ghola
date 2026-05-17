"""Parse 'Closes #N' / 'Fixes #N' directives from issue/PR bodies.

GitHub auto-closes issues when a PR with one of these keywords lands:
close[d|s], fix[ed|es], resolve[d|s]. Comparison is case-insensitive,
keyword + space + #digits, with a word-boundary check on the keyword.

Used as a fallback when the timeline-events path doesn't surface the
resolving PR cleanly. Returns the set of issue numbers referenced as
closure targets.
"""
from __future__ import annotations

import re

# Word-boundary anchored: \b<keyword> ... \s+ #<digits>
# `close[sd]?` matches close/closes/closed; `fix(?:e[sd])?` matches fix/fixes/fixed;
# `resolve[sd]?` matches resolve/resolves/resolved.
_DIRECTIVE_RE = re.compile(
    r"\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)",
    re.IGNORECASE,
)


def parse_closes_directives(text: str | None) -> set[int]:
    """Return the set of issue numbers referenced via close-keyword directives.

    Returns empty set on None or empty input.
    """
    if not text:
        return set()
    return {int(m.group(1)) for m in _DIRECTIVE_RE.finditer(text)}
