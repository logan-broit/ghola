"""Minimal OpenAI-compatible /v1/embeddings stub for dev compose.

Returns a deterministic-but-text-derived fake vector so similar texts
still recover via cosine in the stubbed semantic recall path. Good
enough for Gate 9 smoke tests; NOT a model — use real guild / vLLM
in any setup that actually cares about retrieval quality.
"""

import hashlib
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DIM = int(os.getenv("STUB_EMBEDDING_DIM", "1024"))


def fake_embed(text: str) -> list[float]:
    """Hash-seeded pseudo-embedding. Same text => same vector."""
    seed = int.from_bytes(hashlib.sha256(text.encode()).digest()[:8], "big")
    out = []
    for i in range(DIM):
        seed = (seed * 6364136223846793005 + 1442695040888963407) & ((1 << 64) - 1)
        out.append(((seed >> 11) & 0xFFFF) / 65535.0 * 2 - 1)
    norm = sum(x * x for x in out) ** 0.5 or 1.0
    return [x / norm for x in out]


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a, **kw):
        pass

    def do_GET(self):
        if self.path == "/health":
            self._json(200, {"status": "ok"})
            return
        self.send_error(404)

    def do_POST(self):
        if self.path != "/v1/embeddings":
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        inp = body.get("input", [])
        if isinstance(inp, str):
            inp = [inp]
        self._json(200, {
            "object": "list",
            "data": [
                {"object": "embedding", "index": i, "embedding": fake_embed(t)}
                for i, t in enumerate(inp)
            ],
            "model": body.get("model", "stub"),
            "usage": {"prompt_tokens": 0, "total_tokens": 0},
        })

    def _json(self, code, payload):
        data = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    port = int(os.getenv("PORT", "8082"))
    print(f"guild-stub listening on :{port} (dim={DIM})", flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(0)
