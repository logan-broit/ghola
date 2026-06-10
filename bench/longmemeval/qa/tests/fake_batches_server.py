"""A hand-rolled fake Anthropic Batches endpoint (stdlib http.server).

Serves the three calls the SDK's batches client makes — create, retrieve, and
the JSONL results stream — against a real anthropic.Anthropic client pointed at
it via base_url. No mock objects, no patching of SDK internals: requests cross a
real socket and the SDK parses real HTTP responses, so the test exercises the
actual create/poll/collect wire path.

The server records submitted batches in memory and lets a test script its
behavior: how many retrieve polls return "in_progress" before "ended", and what
result rows the JSONL stream yields.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


class FakeBatchesState:
    """Mutable, test-controllable state shared with the request handler."""

    def __init__(self) -> None:
        # batch_id -> {"requests": [...], "polls_remaining": int, "results": [rows]}
        self.batches: dict[str, dict[str, Any]] = {}
        self.created_payloads: list[dict[str, Any]] = []  # raw create bodies seen
        self.poll_counts: dict[str, int] = {}
        self._next = 0
        # default behavior for a freshly created batch
        self.default_polls_until_ended = 0
        self.default_results: list[dict[str, Any]] = []

    def new_batch_id(self) -> str:
        self._next += 1
        return f"msgbatch_fake_{self._next:03d}"


def _message_batch_json(batch_id: str, ended: bool) -> dict[str, Any]:
    """A MessageBatch envelope the SDK can parse."""
    return {
        "id": batch_id,
        "type": "message_batch",
        "processing_status": "ended" if ended else "in_progress",
        "request_counts": {
            "processing": 0 if ended else 1,
            "succeeded": 1 if ended else 0,
            "errored": 0,
            "canceled": 0,
            "expired": 0,
        },
        "created_at": "2026-06-10T00:00:00Z",
        "expires_at": "2026-06-11T00:00:00Z",
        "ended_at": "2026-06-10T00:05:00Z" if ended else None,
        "archived_at": None,
        "cancel_initiated_at": None,
        # The SDK reads results_url off the retrieve response, then GETs it.
        "results_url": (
            f"/v1/messages/batches/{batch_id}/results" if ended else None
        ),
    }


def _succeeded_row(custom_id: str, text: str, in_tok: int = 11, out_tok: int = 3) -> dict[str, Any]:
    return {
        "custom_id": custom_id,
        "result": {
            "type": "succeeded",
            "message": {
                "id": f"msg_{custom_id}",
                "type": "message",
                "role": "assistant",
                "model": "claude-opus-4-8",
                "content": [{"type": "text", "text": text}],
                "stop_reason": "end_turn",
                "stop_sequence": None,
                "usage": {"input_tokens": in_tok, "output_tokens": out_tok},
            },
        },
    }


def _errored_row(custom_id: str) -> dict[str, Any]:
    return {
        "custom_id": custom_id,
        "result": {
            "type": "errored",
            "error": {
                "type": "error",
                "error": {"type": "api_error", "message": "boom"},
            },
        },
    }


def make_handler(state: FakeBatchesState):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args: Any) -> None:  # silence test noise
            pass

        def _send(self, code: int, body: bytes, content_type: str = "application/json") -> None:
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self) -> None:
            if self.path == "/v1/messages/batches":
                length = int(self.headers.get("Content-Length", "0"))
                payload = json.loads(self.rfile.read(length) or b"{}")
                state.created_payloads.append(payload)
                batch_id = state.new_batch_id()
                custom_ids = [r["custom_id"] for r in payload.get("requests", [])]
                results = state.default_results or [
                    _succeeded_row(cid, f"answer for {cid}") for cid in custom_ids
                ]
                state.batches[batch_id] = {
                    "requests": payload.get("requests", []),
                    "polls_remaining": state.default_polls_until_ended,
                    "results": results,
                }
                state.poll_counts[batch_id] = 0
                self._send(200, json.dumps(_message_batch_json(batch_id, ended=state.default_polls_until_ended == 0)).encode())
                return
            self._send(404, b'{"error":"not found"}')

        def do_GET(self) -> None:
            # results stream
            if self.path.endswith("/results"):
                batch_id = self.path.split("/v1/messages/batches/")[1].rsplit("/results", 1)[0]
                rows = state.batches.get(batch_id, {}).get("results", [])
                body = "\n".join(json.dumps(r) for r in rows).encode()
                self._send(200, body, content_type="application/x-jsonl")
                return
            # retrieve
            if self.path.startswith("/v1/messages/batches/"):
                batch_id = self.path.split("/v1/messages/batches/")[1]
                rec = state.batches.get(batch_id)
                if rec is None:
                    self._send(404, b'{"error":"not found"}')
                    return
                state.poll_counts[batch_id] = state.poll_counts.get(batch_id, 0) + 1
                if rec["polls_remaining"] > 0:
                    rec["polls_remaining"] -= 1
                    ended = False
                else:
                    ended = True
                self._send(200, json.dumps(_message_batch_json(batch_id, ended=ended)).encode())
                return
            self._send(404, b'{"error":"not found"}')

    return Handler


class FakeBatchesServer:
    """Context-manager wrapper: starts the server on an ephemeral port."""

    def __init__(self) -> None:
        self.state = FakeBatchesState()
        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(self.state))
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)

    @property
    def base_url(self) -> str:
        host, port = self._httpd.server_address
        return f"http://{host}:{port}"

    def __enter__(self) -> "FakeBatchesServer":
        self._thread.start()
        return self

    def __exit__(self, *exc: Any) -> None:
        self._httpd.shutdown()
        self._httpd.server_close()
        self._thread.join(timeout=2)

    # helpers (also exposed via .state) ------------------------------------

    @property
    def created_payloads(self) -> list[dict[str, Any]]:
        return self.state.created_payloads

    def succeeded_row(self, custom_id: str, text: str) -> dict[str, Any]:
        return _succeeded_row(custom_id, text)

    def errored_row(self, custom_id: str) -> dict[str, Any]:
        return _errored_row(custom_id)
