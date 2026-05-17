from __future__ import annotations

from seeding_eval.filters import is_bot, BOT_LOGINS


def test_known_bots_filtered():
    assert is_bot("dependabot[bot]")
    assert is_bot("renovate[bot]")
    assert is_bot("github-actions[bot]")


def test_real_users_not_filtered():
    assert not is_bot("rauchg")
    assert not is_bot("timneutkens")
    assert not is_bot("leerob")


def test_case_insensitive():
    # GitHub logins are case-insensitive; filter must mirror that.
    assert is_bot("Dependabot[bot]")
    assert is_bot("DEPENDABOT[BOT]")
    assert is_bot("Renovate[Bot]")


def test_empty_string():
    # Empty login -> not a bot (defensive: don't accidentally filter unsigned commits)
    assert not is_bot("")


def test_bot_logins_set_exposed():
    # The constant is part of the public API -- callers may want to extend it.
    assert "dependabot[bot]" in BOT_LOGINS
    assert "renovate[bot]" in BOT_LOGINS
    assert "github-actions[bot]" in BOT_LOGINS


def test_bot_logins_is_frozen():
    # Frozenset to prevent accidental mutation at module-import time.
    import pytest
    with pytest.raises(AttributeError):
        BOT_LOGINS.add("evil[bot]")  # type: ignore[attr-defined]
