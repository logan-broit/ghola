"""Re-embed all mnemes using the TEI embedding server (nomic-embed-text-v1.5, 768d).

Replaces stored embeddings so that query embeddings (also from TEI) match.
Uses kubectl port-forward to access TEI, processes in batches.

Usage:
    python3 ~/pg_ghola/analysis/reembed_tei.py
"""

import json
import subprocess
import sys
import time

import httpx

NAMESPACE = "ch-system"
DB_POD = "memory-db-1"
TEI_SVC = "tei"
TEI_PORT = 80
LOCAL_PORT = 18090
BATCH_SIZE = 32  # TEI handles larger batches efficiently


def psql(sql: str, timeout: int = 120) -> str:
    if len(sql) > 50000:
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


def embed_batch(client: httpx.Client, texts: list[str]) -> list[list[float]]:
    """Call TEI embed endpoint for a batch of texts."""
    resp = client.post(
        f"http://localhost:{LOCAL_PORT}/embed",
        json={"inputs": texts, "truncate": True},
        timeout=60.0,
    )
    resp.raise_for_status()
    return resp.json()


def main():
    # Start port-forward in background
    pf = subprocess.Popen(
        ["kubectl", "port-forward", "-n", NAMESPACE, f"svc/{TEI_SVC}",
         f"{LOCAL_PORT}:{TEI_PORT}"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    time.sleep(2)

    try:
        client = httpx.Client()

        # Verify TEI is reachable and check dimensions
        test_emb = embed_batch(client, ["test"])
        dims = len(test_emb[0])
        print(f"TEI connected: {dims}d embeddings", flush=True)

        # Get total count
        total = int(psql("SELECT count(*) FROM ghola.mnemes;"))
        print(f"Total mnemes: {total}", flush=True)

        offset = 0
        updated = 0
        start = time.time()

        while offset < total:
            # Fetch batch
            rows_raw = psql(f"""
                SELECT id::text || E'\\x01' || left(content, 8000)
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

            # Get embeddings from TEI
            embeddings = embed_batch(client, texts)

            # Build UPDATE statements
            updates = []
            for mneme_id, emb in zip(ids, embeddings):
                emb_str = "[" + ",".join(f"{v}" for v in emb) + "]"
                updates.append(
                    f"UPDATE ghola.mnemes SET embedding = '{emb_str}'::vector "
                    f"WHERE id = '{mneme_id}'::uuid"
                )

            sql = "BEGIN;\n" + ";\n".join(updates) + ";\nCOMMIT;"
            try:
                psql(sql, timeout=180)
            except RuntimeError as e:
                print(f"  Batch at offset {offset} failed: {e}", flush=True)
                for u in updates:
                    try:
                        psql(u, timeout=30)
                    except Exception:
                        pass

            updated += len(ids)
            offset += BATCH_SIZE

            if updated % 100 == 0 or offset >= total - BATCH_SIZE:
                elapsed = time.time() - start
                rate = updated / elapsed if elapsed > 0 else 0
                eta = (total - updated) / rate if rate > 0 else 0
                print(f"  {updated}/{total} ({rate:.1f}/s, ETA {eta:.0f}s)", flush=True)

    finally:
        pf.terminate()
        pf.wait()

    elapsed = time.time() - start
    print(f"Re-embedding complete: {updated} mnemes in {elapsed:.1f}s", flush=True)
    print(f"Dimensions: {dims}d (TEI nomic-embed-text-v1.5)", flush=True)


if __name__ == "__main__":
    main()
