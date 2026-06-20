"""Hand-rolled fake truthsayer (rerank) + guild (embeddings) endpoints.

Mirrors ``fake_batches_server.py``: a stdlib ``http.server`` on a thread, with
the real scorer client pointed at it via ``base_url``. No mock objects, no
patching of client internals: requests cross a real socket and the client
parses real HTTP responses, so the test exercises the actual urllib wire path.

Two surfaces:
  - ``/v1/rerank``  -> ``{"scores": [{"id", "score"}]}`` (truthsayer)
  - ``/v1/embeddings`` -> ``{"data": [{"embedding": [...]}]}`` (guild fallback)

Scoring is deterministic and test-controllable. The rerank handler scores each
candidate by a stable hash of its text so distinct texts get distinct scores
(the contract the scorer-by-id test asserts). A ``status_override`` lets a test
force a non-2xx response to prove errors are raised, not swallowed.
"""

from __future__ import annotations

import hashlib
import json
import math
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


def _text_score(text: str) -> float:
    """Deterministic [0, 1) score from the candidate text. Distinct texts get
    distinct scores so the by-id mapping test can assert ``out["a"] != out["b"]``
    without depending on any live model."""
    h = hashlib.sha256(text.encode("utf-8")).digest()
    return int.from_bytes(h[:4], "big") / 2**32


def _embed(text: str, dim: int = 8) -> list[float]:
    """Deterministic unit-ish embedding from the text hash. Distinct texts get
    distinct vectors so cosine ordering is meaningful in the guild path test."""
    h = hashlib.sha256(text.encode("utf-8")).digest()
    vec = [((h[i % len(h)] + i) % 17) - 8 for i in range(dim)]
    norm = math.sqrt(sum(v * v for v in vec)) or 1.0
    return [v / norm for v in vec]


class FakeScorerState:
    """Mutable, test-controllable state shared with the request handler."""

    def __init__(self) -> None:
        self.rerank_payloads: list[dict[str, Any]] = []
        self.embed_payloads: list[dict[str, Any]] = []
        # When set, every request gets this HTTP status (to prove errors raise).
        self.status_override: int | None = None


def make_handler(state: FakeScorerState):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args: Any) -> None:  # silence test noise
            pass

        def _send(self, code: int, body: bytes) -> None:
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _read_json(self) -> dict[str, Any]:
            length = int(self.headers.get("Content-Length", "0"))
            return json.loads(self.rfile.read(length) or b"{}")

        def do_POST(self) -> None:
            if state.status_override is not None:
                self._send(state.status_override, b'{"error":"forced"}')
                return
            if self.path == "/v1/rerank":
                payload = self._read_json()
                state.rerank_payloads.append(payload)
                scores = [
                    {"id": c["id"], "score": _text_score(c["text"])}
                    for c in payload.get("candidates", [])
                ]
                self._send(200, json.dumps({"scores": scores}).encode())
                return
            if self.path == "/v1/embeddings":
                payload = self._read_json()
                state.embed_payloads.append(payload)
                inputs = payload.get("input", [])
                if isinstance(inputs, str):
                    inputs = [inputs]
                data = [
                    {"index": i, "object": "embedding", "embedding": _embed(t)}
                    for i, t in enumerate(inputs)
                ]
                self._send(200, json.dumps({"data": data}).encode())
                return
            self._send(404, b'{"error":"not found"}')

    return Handler


class FakeScorerServer:
    """Context-manager wrapper: starts the server on an ephemeral port."""

    def __init__(self, status_override: int | None = None) -> None:
        self.state = FakeScorerState()
        self.state.status_override = status_override
        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(self.state))
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)

    @property
    def url(self) -> str:
        host, port = self._httpd.server_address
        return f"http://{host}:{port}"

    # alias so tests can read either name
    base_url = url

    def __enter__(self) -> "FakeScorerServer":
        self._thread.start()
        return self

    def __exit__(self, *exc: Any) -> None:
        self._httpd.shutdown()
        self._httpd.server_close()
        self._thread.join(timeout=2)
