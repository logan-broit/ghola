"""Minimal OpenAI-compatible embedding server using Qwen3-Embedding-0.6B.

Serves POST /v1/embeddings for the ch-server to generate query embeddings
that match the 1024d stored embeddings.

Usage:
    cd ~/longmemeval-ghola && .venv/bin/python ~/pg_ghola/analysis/embed_server.py
"""

import json
import time
import sys
from http.server import HTTPServer, BaseHTTPRequestHandler
from sentence_transformers import SentenceTransformer

MODEL_NAME = "Qwen/Qwen3-Embedding-0.6B"
PORT = 8082
HOST = "0.0.0.0"

print(f"Loading {MODEL_NAME}...", flush=True)
model = SentenceTransformer(MODEL_NAME, trust_remote_code=True)
print(f"Model loaded, dims={model.get_sentence_embedding_dimension()}", flush=True)


class EmbeddingHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path in ("/v1/embeddings", "/embed"):
            content_length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(content_length))

            # OpenAI format: input can be string or list
            inputs = body.get("input", body.get("inputs", []))
            if isinstance(inputs, str):
                inputs = [inputs]

            embeddings = model.encode(inputs, normalize_embeddings=True)

            if self.path == "/embed":
                # TEI format: return list of lists
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(embeddings.tolist()).encode())
            else:
                # OpenAI format
                data = []
                for i, emb in enumerate(embeddings):
                    data.append({
                        "object": "embedding",
                        "index": i,
                        "embedding": emb.tolist(),
                    })
                resp = {
                    "object": "list",
                    "data": data,
                    "model": MODEL_NAME,
                    "usage": {"prompt_tokens": 0, "total_tokens": 0},
                }
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(resp).encode())
        elif self.path == "/health":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')
        else:
            self.send_response(404)
            self.end_headers()

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, format, *args):
        pass  # Suppress request logging


if __name__ == "__main__":
    server = HTTPServer((HOST, PORT), EmbeddingHandler)
    print(f"Embedding server listening on {HOST}:{PORT}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("Shutting down.")
        server.shutdown()
