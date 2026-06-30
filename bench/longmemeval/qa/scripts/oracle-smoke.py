#!/usr/bin/env python3
"""Manual smoke test for the local-LM logprob oracle (NOT part of CI).

This exercises the live network path that the unit tests deliberately do not:
a running vLLM OpenAI-compatible completions server with prompt_logprobs. It
calls ``LocalLMClient().token_logprobs(...)`` against a real socket and prints
the (token, surprisal) pairs, asserting the rare token ("9931") lands among the
highest-surprisal tokens -- the property perplexity_prune relies on.

Launch the oracle first (prompt_logprobs is requested per-call, not at launch):

    vllm serve Qwen2.5-1.5B-Instruct --port 8000

Then point this script at it and run:

    ORACLE_URL=http://localhost:8000 \
    ORACLE_MODEL=Qwen2.5-1.5B-Instruct \
    .venv/bin/python scripts/oracle-smoke.py

ORACLE_URL / ORACLE_MODEL default to http://localhost:8000 and
Qwen2.5-1.5B-Instruct (the LocalLMClient defaults). Exits non-zero if the rare
token is not among the most-surprising tokens, or if the server is unreachable
(the RuntimeError from the client propagates loudly).
"""

from __future__ import annotations

import sys

# Make the package importable when run as scripts/oracle-smoke.py from the qa/
# project root (which is what the launch comment above assumes).
sys.path.insert(0, ".")

from lme_qa.local_lm import LocalLMClient  # noqa: E402

PROMPT = "the database runs on port 9931"
RARE = "9931"


def main() -> int:
    client = LocalLMClient()
    print(f"oracle: {client.base_url}  model: {client.model}")
    print(f"prompt: {PROMPT!r}\n")

    # token_logprobs returns [(decoded_token, logprob), ...]. Surprisal is the
    # negative logprob -- higher = more surprising / informative.
    pairs = client.token_logprobs(PROMPT)
    if not pairs:
        print("FAIL: oracle returned no prompt_logprobs", file=sys.stderr)
        return 1

    scored = [(tok, -lp) for tok, lp in pairs]
    print(f"{'token':<20} surprisal")
    print(f"{'-----':<20} ---------")
    for tok, surprisal in scored:
        print(f"{tok!r:<20} {surprisal:8.3f}")

    by_surprisal = sorted(scored, key=lambda ts: ts[1], reverse=True)
    # The rare token may be split across subword pieces; match on substring so a
    # "99" / "31" tokenization still counts. Assert it sits in the top half of
    # the surprisal ranking -- a rare numeric literal should be hard to predict.
    rank_threshold = max(1, len(by_surprisal) // 2)
    top = by_surprisal[:rank_threshold]
    hit = any(RARE in tok or tok in RARE for tok, _ in top)

    print()
    if not hit:
        print(
            f"FAIL: rare token {RARE!r} not among the top-{rank_threshold} "
            f"most-surprising tokens",
            file=sys.stderr,
        )
        return 1

    print(
        f"OK: rare token {RARE!r} is among the top-{rank_threshold} "
        f"most-surprising tokens"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
