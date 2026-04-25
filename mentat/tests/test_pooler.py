import math

from mentat.pooler import TYPE_WEIGHTS, type_weighted_mean_pool
from mentat.schemas import Event


def test_type_weighted_mean_matches_documented_weights():
    # Three events with known embeddings; check post-normalization output
    # matches the spec: weights {u:1.0, a:0.5, t:0.1}, sum=1.6.
    events = [
        Event(type="user",        embedding=[1.0, 0.0, 0.0]),
        Event(type="assistant",   embedding=[0.0, 1.0, 0.0]),
        Event(type="tool_result", embedding=[0.0, 0.0, 1.0]),
    ]
    out = type_weighted_mean_pool(events)
    expected = [1.0/1.6, 0.5/1.6, 0.1/1.6]
    n = math.sqrt(sum(x*x for x in expected))
    expected = [x/n for x in expected]
    for a, b in zip(out, expected):
        assert abs(a - b) < 1e-6


def test_all_system_events_fall_back_to_uniform():
    # weight-sum == 0 is the one branch worth pinning; a silent 0-vector
    # would poison HNSW indexes.
    events = [Event(type="system", embedding=[1.0, 2.0, 3.0])]
    out = type_weighted_mean_pool(events)
    assert any(abs(v) > 0 for v in out)
