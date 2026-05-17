"""Wire-shape tests for GholaClient.recall.

The header in ``ghola_client.py`` calls out that the wrapper itself is
"observed end-to-end via the D6 integration test, matching the
github_client.py pattern (no mocks)." That stayed true while the wire
body was static. D4 introduces a *conditional* field (``primitives``)
on the request body, and the only place to verify the conditional is
the wire — so we add a narrow test against ``httpx.MockTransport`` (a
real httpx transport that captures the outgoing request, not a mock of
the client class). This is the same "real-ish HTTP transport" stance
the rest of the suite takes — no new dependency, no monkey-patching.
"""
from __future__ import annotations

import json

import httpx

from seeding_eval.ghola_client import GholaClient


def _capturing_client(captured: list[httpx.Request]) -> GholaClient:
    """GholaClient wired to a MockTransport that records requests and
    replies with an empty hits payload. The caller inspects ``captured``
    after the call to assert on the wire body."""

    def handler(request: httpx.Request) -> httpx.Response:
        captured.append(request)
        return httpx.Response(200, json={"hits": [], "tier_counts": {}})

    transport = httpx.MockTransport(handler)
    client = GholaClient(base_url="http://ghola.test")
    # Swap the underlying httpx.Client for one wired to MockTransport.
    # The constructor's auth/timeout/header logic is incidental to what
    # we're testing — what matters is that ``recall`` builds the right
    # request body.
    client._client = httpx.Client(
        base_url="http://ghola.test",
        transport=transport,
    )
    return client


def test_recall_default_omits_primitives_field() -> None:
    """The wire body has no ``primitives`` key when the caller doesn't
    pass the flag. This matches the server's omitempty contract: absence
    is treated as ``false`` and we keep older callers byte-identical."""
    captured: list[httpx.Request] = []
    with _capturing_client(captured) as client:
        client.recall(
            query="hello",
            workspace_id="ws-1",
            user_id="user-1",
            k=20,
        )

    assert len(captured) == 1
    body = json.loads(captured[0].content)
    assert "primitives" not in body


def test_recall_primitives_false_omits_primitives_field() -> None:
    """Explicit ``primitives=False`` is also omitted — same omitempty
    contract. Anything else would silently change the wire shape for
    callers that thread a default through a config object."""
    captured: list[httpx.Request] = []
    with _capturing_client(captured) as client:
        client.recall(
            query="hello",
            workspace_id="ws-1",
            user_id="user-1",
            k=20,
            primitives=False,
        )

    assert len(captured) == 1
    body = json.loads(captured[0].content)
    assert "primitives" not in body


def test_recall_passes_primitives_flag() -> None:
    """``primitives=True`` puts ``"primitives": true`` on the wire."""
    captured: list[httpx.Request] = []
    with _capturing_client(captured) as client:
        client.recall(
            query="hello",
            workspace_id="ws-1",
            user_id="user-1",
            k=20,
            primitives=True,
        )

    assert len(captured) == 1
    body = json.loads(captured[0].content)
    assert body.get("primitives") is True
    # Sanity: existing fields still on the wire.
    assert body["query_text"] == "hello"
    assert body["workspace"] == "ws-1"
    assert body["user_id"] == "user-1"
    assert body["limit"] == 20
