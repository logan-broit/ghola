"""Filters applied at bundle-build time to keep the corpus signal-rich.

Bot accounts (dependabot, renovate, github-actions) generate large
volumes of low-signal commits/PRs that would dominate co-activation
statistics without contributing to the eval's question. The
seeding-pipeline.md spec mandates skipping them.
"""
from __future__ import annotations

BOT_LOGINS: frozenset[str] = frozenset({
    "dependabot[bot]",
    "renovate[bot]",
    "github-actions[bot]",
})


def is_bot(login: str) -> bool:
    """Return True if `login` is a known automated GitHub account.

    Comparison is case-insensitive (GitHub logins themselves are
    case-insensitive). Empty string is not a bot.
    """
    return login.lower() in BOT_LOGINS
