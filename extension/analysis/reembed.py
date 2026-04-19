"""Re-embed all mnemes using local Qwen3-Embedding-0.6B model.

Replaces stored embeddings with ones from the local model so that
query embeddings (also from this model) produce meaningful cosine similarity.

Usage:
    cd ~/longmemeval-ghola && .venv/bin/python ~/pg_ghola/analysis/reembed.py
"""

import json
import subprocess
import sys
import time

NAMESPACE = "ch-system"
DB_POD = "memory-db-1"
BATCH_SIZE = 50  # ~16KB per embedding, 50*16KB < 1MB per batch SQL


def psql(sql: str, timeout: int = 60) -> str:
    """Execute SQL via kubectl exec + psql. Uses stdin for large queries."""
    if len(sql) > 50000:
        # Large SQL: pipe via stdin
        cmd = [
            "kubectl", "exec", "-i", "-n", NAMESPACE, DB_POD, "--",
            "psql", "-U", "postgres", "-d", "memories", "-t", "-A"
        ]
        result = subprocess.run(cmd, input=sql, capture_output=True, text=True, timeout=timeout)
    else:
        cmd = [
            "kubectl", "exec", "-n", NAMESPACE, DB_POD, "--",
            "psql", "-U", "postgres", "-d", "memories", "-t", "-A", "-c", sql
        ]
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if result.returncode != 0 and "ERROR" in result.stderr:
        raise RuntimeError(f"SQL error: {result.stderr[:300]}")
    return result.stdout.strip()


def main():
    from sentence_transformers import SentenceTransformer

    print("Loading model...")
    model = SentenceTransformer('Qwen/Qwen3-Embedding-0.6B', trust_remote_code=True)

    # Get total count
    total = int(psql("SELECT count(*) FROM ghola.mnemes;"))
    print(f"Total mnemes: {total}", flush=True)

    # Process in batches by offset
    offset = 0
    updated = 0
    start = time.time()

    while offset < total:
        # Fetch batch of IDs and content
        rows_raw = psql(f"""
            SELECT id::text || '\x01' || left(content, 8000)
            FROM ghola.mnemes
            ORDER BY id
            LIMIT {BATCH_SIZE} OFFSET {offset};
        """)

        if not rows_raw:
            break

        ids = []
        texts = []
        for line in rows_raw.split("\n"):
            if "\x01" not in line:
                continue
            parts = line.split("\x01", 1)
            ids.append(parts[0])
            texts.append(parts[1])

        if not texts:
            break

        # Batch encode
        embeddings = model.encode(texts, batch_size=len(texts), normalize_embeddings=True)

        # Build UPDATE statements
        updates = []
        for i, (mneme_id, emb) in enumerate(zip(ids, embeddings)):
            emb_str = "[" + ",".join(f"{v}" for v in emb.tolist()) + "]"
            updates.append(
                f"UPDATE ghola.mnemes SET embedding = '{emb_str}'::vector WHERE id = '{mneme_id}'::uuid"
            )

        # Execute batch of updates in a transaction
        sql = "BEGIN;\n" + ";\n".join(updates) + ";\nCOMMIT;"
        try:
            psql(sql, timeout=120)
        except RuntimeError as e:
            print(f"  Batch at offset {offset} failed: {e}")
            # Try one at a time
            for u in updates:
                try:
                    psql(u)
                except Exception:
                    pass

        updated += len(ids)
        offset += BATCH_SIZE

        if updated % 100 == 0 or offset >= total - BATCH_SIZE:
            elapsed = time.time() - start
            rate = updated / elapsed if elapsed > 0 else 0
            eta = (total - updated) / rate if rate > 0 else 0
            print(f"  {updated}/{total} ({rate:.0f}/s, ETA {eta:.0f}s)", flush=True)

    elapsed = time.time() - start
    print(f"Re-embedding complete: {updated} mnemes in {elapsed:.1f}s", flush=True)


if __name__ == "__main__":
    main()
