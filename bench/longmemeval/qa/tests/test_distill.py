"""Claude distiller + disk cache — pure-logic tests with an INJECTED fake
``call`` callable.

USER POLICY (strict): these tests exercise only the pure pieces (prompt
building, structured JSON render, cache roundtrip, cache-hit short-circuit) and
the Distiller's call-seam with a deterministic fake. NO real ``claude -p``
invocation, NO subprocess mocking — the fake ``call(prompt)->str`` is injected
at the seam. Real subprocess use lives only in scripts/distill-smoke.py (not run
in CI).
"""

from __future__ import annotations

from lme_qa import distill


def test_render_structured_joins_facts():
    assert distill.render_structured('{"facts":["a","b"]}') == "- a\n- b"


def test_render_structured_bad_json_falls_back_to_prose():
    assert distill.render_structured("not json") == "not json"


def test_render_structured_wrong_shape_falls_back_to_prose():
    # Valid JSON but no "facts" list -> treat the raw text as prose, never raise.
    assert distill.render_structured('{"summary":"x"}') == '{"summary":"x"}'
    assert distill.render_structured('{"facts": "not a list"}') == '{"facts": "not a list"}'
    assert distill.render_structured("[1, 2, 3]") == "[1, 2, 3]"


def test_cache_roundtrip(tmp_path):
    c = distill.Cache(tmp_path)
    c.put("k", "v")
    assert c.get("k") == "v"
    assert c.get("missing") is None


def test_cache_key_includes_all_dimensions():
    # The key must vary with EVERY dimension: compressor, query_mode, output_form,
    # budget, context_text. Two keys differing in exactly one dimension must
    # differ; the same inputs must collide (deterministic).
    base = dict(
        compressor="llm_distill",
        query_mode="agnostic",
        output_form="prose",
        budget=100,
        context_text="ctx",
    )
    k0 = distill.Cache.key_for(**base)
    assert k0 == distill.Cache.key_for(**base)  # deterministic
    for field, alt in [
        ("compressor", "other"),
        ("query_mode", "aware"),
        ("output_form", "structured"),
        ("budget", 200),
        ("context_text", "ctx2"),
    ]:
        assert distill.Cache.key_for(**{**base, field: alt}) != k0


def test_distill_uses_cache(tmp_path):
    calls = []
    d = distill.Distiller(
        call=lambda p: (calls.append(p) or "distilled"),
        cache=distill.Cache(tmp_path),
    )
    a = d.distill(
        "ctx", query="q", query_mode="agnostic", output_form="prose", budget=100
    )
    b = d.distill(
        "ctx", query="q", query_mode="agnostic", output_form="prose", budget=100
    )
    assert a == b == "distilled" and len(calls) == 1  # second is cached


def test_agnostic_prompt_omits_question():
    p = distill.build_prompt(
        "CTX", query="SECRET_Q", query_mode="agnostic", output_form="prose", budget=50
    )
    assert "SECRET_Q" not in p


def test_aware_prompt_includes_question():
    p = distill.build_prompt(
        "CTX", query="SECRET_Q", query_mode="aware", output_form="prose", budget=50
    )
    assert "SECRET_Q" in p


def test_prompt_includes_context_and_budget():
    p = distill.build_prompt(
        "THE_CONTEXT", query="q", query_mode="agnostic", output_form="prose", budget=42
    )
    assert "THE_CONTEXT" in p
    assert "42" in p


def test_structured_distill_renders_facts(tmp_path):
    d = distill.Distiller(
        call=lambda p: '{"facts":["x","y"]}', cache=distill.Cache(tmp_path)
    )
    out = d.distill(
        "ctx", query="q", query_mode="agnostic", output_form="structured", budget=100
    )
    assert out == "- x\n- y"


def test_structured_distill_bad_json_falls_back(tmp_path):
    # A malformed structured response must NOT crash the reader loop: it falls
    # back to the raw (stripped) text treated as prose.
    d = distill.Distiller(
        call=lambda p: "  not valid json  ", cache=distill.Cache(tmp_path)
    )
    out = d.distill(
        "ctx", query="q", query_mode="agnostic", output_form="structured", budget=100
    )
    assert out == "not valid json"


def test_distill_caches_rendered_text_not_raw(tmp_path):
    # The cache stores the RENDERED text (post structured-render), so a cache hit
    # returns the rendered form without re-calling and without re-rendering.
    calls = []
    cache = distill.Cache(tmp_path)
    d = distill.Distiller(
        call=lambda p: (calls.append(p) or '{"facts":["x","y"]}'), cache=cache
    )
    first = d.distill(
        "ctx", query="q", query_mode="agnostic", output_form="structured", budget=100
    )
    second = d.distill(
        "ctx", query="q", query_mode="agnostic", output_form="structured", budget=100
    )
    assert first == second == "- x\n- y"
    assert len(calls) == 1
    # The cached file holds the rendered text, not the raw JSON.
    key = distill.Cache.key_for(
        compressor="llm_distill",
        query_mode="agnostic",
        output_form="structured",
        budget=100,
        context_text="ctx",
    )
    assert cache.get(key) == "- x\n- y"


def test_distill_works_without_cache():
    # cache=None: no disk, every call re-invokes (no caching), still returns text.
    calls = []
    d = distill.Distiller(call=lambda p: (calls.append(p) or "out"), cache=None)
    a = d.distill(
        "ctx", query="q", query_mode="agnostic", output_form="prose", budget=10
    )
    b = d.distill(
        "ctx", query="q", query_mode="agnostic", output_form="prose", budget=10
    )
    assert a == b == "out"
    assert len(calls) == 2  # no cache -> called twice
