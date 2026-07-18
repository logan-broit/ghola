"""Local LM logprob client. Pure parse-function tests + a request-shape guard
for the echo path -- no network here (the urllib client mirrors scorer.py's
_post_json, already covered against a real socket in test_scorer.py). The live
completions path gets a separate manual smoke script (scripts/oracle-smoke.py).

Regression context: an earlier version parsed vLLM ``prompt_logprobs`` and
picked the ``rank == 1`` entry per position -- that is the model's ARGMAX token
logprob, NOT the actual prompt token's logprob (verified divergent at ~6/14
positions on a real prompt: argmax ' following' -0.34 vs actual ' capital'
-11.13). The surprisal we want is of the ACTUAL text, so the client now uses the
standard OpenAI echo path (echo=True, max_tokens=0, logprobs=0), whose
``choices[0].logprobs.token_logprobs`` are the actual prompt-token logprobs."""

from __future__ import annotations

import pytest

from lme_qa.local_lm import LocalLMClient, parse_echo_logprobs


# A trimmed but faithful vLLM echo response (echo=True, max_tokens=0,
# logprobs=0) for the prompt "The capital of France is ...", captured from
# Qwen2.5-1.5B-Instruct. ``logprobs.token_logprobs`` holds the ACTUAL prompt
# tokens' logprobs aligned with ``logprobs.tokens``; position 0 is null (nothing
# precedes the first token). Note ' capital' scores -11.128: the model's argmax
# there was ' following' (-0.34), so a rank==1/argmax parse would have reported
# -0.34 -- the whole bug this module now avoids.
_ECHO_RESP = {
    "choices": [
        {
            "logprobs": {
                "tokens": ["The", " capital", " of", " France", " is"],
                "token_logprobs": [None, -11.128, -0.485, -4.864, -0.693],
                "top_logprobs": [None, {" following": -0.34}, {}, {}, {}],
                "text_offset": [0, 3, 11, 14, 21],
            }
        }
    ]
}


def test_parse_echo_returns_actual_token_logprobs():
    # The ACTUAL prompt tokens' logprobs, in prompt order, null position 0 dropped.
    assert parse_echo_logprobs(_ECHO_RESP) == [
        (" capital", -11.128),
        (" of", -0.485),
        (" France", -4.864),
        (" is", -0.693),
    ]


def test_parse_echo_skips_null_first_position():
    # token_logprobs[0] is None (no context precedes the first token) -> dropped.
    assert parse_echo_logprobs(_ECHO_RESP)[0] == (" capital", -11.128)


def test_parse_echo_empty_or_allnull():
    assert parse_echo_logprobs({"choices": []}) == []
    assert parse_echo_logprobs({}) == []
    assert (
        parse_echo_logprobs(
            {"choices": [{"logprobs": {"tokens": [], "token_logprobs": []}}]}
        )
        == []
    )
    assert (
        parse_echo_logprobs(
            {"choices": [{"logprobs": {"tokens": ["The"], "token_logprobs": [None]}}]}
        )
        == []
    )


def test_token_logprobs_uses_echo_path_and_returns_actual_logprobs(monkeypatch):
    # Guards against regressing to the prompt_logprobs+argmax bug: the request
    # must be the echo path (echo=True, max_tokens=0, logprobs=0) and NOT ask for
    # prompt_logprobs, and the result must be the actual-token logprobs.
    captured = {}

    def fake_post(url, body, timeout):
        captured["url"] = url
        captured["body"] = body
        captured["timeout"] = timeout
        return _ECHO_RESP

    monkeypatch.setattr("lme_qa.local_lm._post_json", fake_post)

    client = LocalLMClient(base_url="http://oracle:8000", model="m")
    out = client.token_logprobs("The capital of France is")

    assert out == [
        (" capital", -11.128),
        (" of", -0.485),
        (" France", -4.864),
        (" is", -0.693),
    ]
    assert captured["url"] == "http://oracle:8000/v1/completions"
    body = captured["body"]
    assert body["echo"] is True
    assert body["max_tokens"] == 0
    assert body["logprobs"] == 0
    assert "prompt_logprobs" not in body
    assert body["prompt"] == "The capital of France is"


def test_token_logprobs_empty_text_short_circuits_without_request(monkeypatch):
    # vLLM rejects an empty prompt outright (HTTP 400 "Either prompt or
    # prompt_embeds must be provided and non-empty"), and LongMemEval-S contains
    # exactly-empty turn contents (12 in the cleaned dataset), so scoring a leaf
    # crashed mid-build. An empty text has no tokens, hence no logprobs: return
    # [] WITHOUT any HTTP call — the same value a whitespace-only prompt already
    # yields via parse (single token, null logprob), and the scorer's existing
    # `if not lp` guard maps it to surprisal 0.0.
    def fake_post(url, body, timeout):  # pragma: no cover - must not be reached
        raise AssertionError("empty text must not reach the oracle")

    monkeypatch.setattr("lme_qa.local_lm._post_json", fake_post)

    client = LocalLMClient(base_url="http://oracle:8000", model="m")
    assert client.token_logprobs("") == []


# --- context-length 400: halve-and-retry (crash-loop regression) ---------
#
# Live incident: the echo-path request (echo=True, max_tokens=0) 400s with
# "This model's maximum context length is 4096 tokens. However, you
# requested 0 output tokens and your prompt contains at least 4097 input
# tokens" on token-dense text (code/ids run closer to ~2 chars/token than
# English prose's ~4), even under compress.py's proactive char cap. The
# sweep driver misread the uncaught RuntimeError as a usage-window crash and
# retried the whole run every 30m, hitting the identical crash each time.
# Fix mirrors ghola_backend.py's record-path halve-retry: on an HTTP 400
# whose body mentions "maximum context length", halve the text and retry,
# bounded, and return [] (the scorer's existing `if not lp` guard -> 0.0)
# if it still won't fit.


def _context_length_error(prompt_tokens: int = 4097) -> RuntimeError:
    """A RuntimeError shaped exactly like scorer.py's _post_json output for
    the real vLLM context-length 400 body captured in the live crash log."""
    return RuntimeError(
        "scorer request to http://oracle:8000/v1/completions failed: "
        "HTTP 400 b'{\"error\":{\"message\":\"This model\\'s maximum context "
        "length is 4096 tokens. However, you requested 0 output tokens and "
        f"your prompt contains at least {prompt_tokens} input tokens, for a "
        "total of at least 4097 tokens\"}}'"
    )


def test_token_logprobs_halves_on_context_length_400_then_succeeds(monkeypatch):
    calls = []

    def fake_post(url, body, timeout):
        calls.append(body["prompt"])
        if len(calls) == 1:
            raise _context_length_error()
        return _ECHO_RESP

    monkeypatch.setattr("lme_qa.local_lm._post_json", fake_post)

    client = LocalLMClient(base_url="http://oracle:8000", model="m")
    long_text = "token " * 2000
    out = client.token_logprobs(long_text)

    assert out == parse_echo_logprobs(_ECHO_RESP)
    assert len(calls) == 2
    # Second attempt is a shorter prefix of the first, halved.
    assert long_text.startswith(calls[0])
    assert calls[0].startswith(calls[1])
    assert len(calls[1]) < len(calls[0])
    assert len(calls[1]) <= len(calls[0]) // 2 + 1


def test_token_logprobs_persistent_400_returns_empty_after_bounded_halvings(
    monkeypatch,
):
    # A context-length 400 that never clears (pathological token density)
    # must NOT retry forever -- bounded halvings, then [] rather than a
    # crash. [] is exactly what the scorer's `if not lp` guard already
    # treats as a neutral surprisal of 0.0.
    calls = []

    def fake_post(url, body, timeout):
        calls.append(body["prompt"])
        raise _context_length_error()

    monkeypatch.setattr("lme_qa.local_lm._post_json", fake_post)

    client = LocalLMClient(base_url="http://oracle:8000", model="m")
    out = client.token_logprobs("x" * 256)

    assert out == []
    # Initial attempt + at most ~6 halvings -- bounded, not unbounded retry.
    assert 1 < len(calls) <= 7


def test_token_logprobs_non_context_length_400_still_raises(monkeypatch):
    # A 400 for an unrelated reason must not be swallowed by the halving
    # logic -- only context-length 400s get special handling.
    def fake_post(url, body, timeout):
        raise RuntimeError(
            "scorer request to http://oracle:8000/v1/completions failed: "
            "HTTP 400 b'{\"error\":{\"message\":\"invalid request: bad "
            "param\"}}'"
        )

    monkeypatch.setattr("lme_qa.local_lm._post_json", fake_post)

    client = LocalLMClient(base_url="http://oracle:8000", model="m")
    with pytest.raises(RuntimeError, match="bad param"):
        client.token_logprobs("normal text")


def test_token_logprobs_normal_path_untouched(monkeypatch):
    # No error: exactly one call, prompt unmodified -- existing behavior
    # preserved precisely.
    calls = []

    def fake_post(url, body, timeout):
        calls.append(body["prompt"])
        return _ECHO_RESP

    monkeypatch.setattr("lme_qa.local_lm._post_json", fake_post)

    client = LocalLMClient(base_url="http://oracle:8000", model="m")
    out = client.token_logprobs("The capital of France is")

    assert out == parse_echo_logprobs(_ECHO_RESP)
    assert calls == ["The capital of France is"]
