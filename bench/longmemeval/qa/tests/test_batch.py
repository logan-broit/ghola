"""SDK-boundary tests: anthropic client against a real fake HTTP server.

No mock objects, no patching SDK internals — the client talks to a stdlib
http.server over a socket via base_url, so submit/poll/collect exercise the
real wire path.
"""

from __future__ import annotations

import json
from pathlib import Path

import anthropic
import pytest

from lme_qa.batch import (
    BatchDriver,
    judge_request,
    reader_request,
)
from tests.fake_batches_server import FakeBatchesServer


@pytest.fixture
def server():
    with FakeBatchesServer() as s:
        yield s


def _client(server: FakeBatchesServer) -> anthropic.Anthropic:
    # Real client; a dummy key satisfies construction, base_url points at fake.
    return anthropic.Anthropic(api_key="test-key", base_url=server.base_url)


def test_submit_shape_model_thinking_custom_id(server, tmp_path):
    driver = BatchDriver(_client(server), tmp_path / "state.json")
    reqs = [
        reader_request("q1", "SYS", "user text 1"),
        reader_request("q2", "SYS", "user text 2"),
    ]
    driver.submit("reader", reqs)

    assert len(server.created_payloads) == 1
    sent = server.created_payloads[0]["requests"]
    assert [r["custom_id"] for r in sent] == ["q1", "q2"]
    for r in sent:
        params = r["params"]
        assert params["model"] == "claude-opus-4-8"
        assert params["thinking"] == {"type": "adaptive"}
        # No sampling parameters (would 400 on Opus 4.8).
        assert "temperature" not in params
        assert "top_p" not in params
        assert "top_k" not in params
        assert params["max_tokens"] == 8000  # reader headroom
        assert params["system"] == "SYS"


def test_judge_request_shape(server, tmp_path):
    driver = BatchDriver(_client(server), tmp_path / "state.json")
    driver.submit("judge", [judge_request("q1", "JUDGE PROMPT")])
    params = server.created_payloads[0]["requests"][0]["params"]
    assert params["model"] == "claude-opus-4-8"
    assert params["thinking"] == {"type": "adaptive"}
    assert params["max_tokens"] == 2048
    assert "system" not in params  # judge is a single user turn
    assert params["messages"] == [{"role": "user", "content": "JUDGE PROMPT"}]


def test_resume_from_state_no_resubmit(server, tmp_path):
    state = tmp_path / "state.json"
    reqs = [reader_request("q1", "SYS", "u1")]
    d1 = BatchDriver(_client(server), state)
    id1 = d1.submit("reader", reqs)
    assert len(server.created_payloads) == 1

    # Second driver, same state + same request set -> resume, no new submit.
    d2 = BatchDriver(_client(server), state)
    id2 = d2.submit("reader", reqs)
    assert id2 == id1
    assert len(server.created_payloads) == 1  # still just one create

    # State file records the batch id + fingerprint.
    saved = json.loads(state.read_text())
    assert saved["reader"]["batch_id"] == id1
    assert "fingerprint" in saved["reader"]


def test_fresh_forces_resubmit(server, tmp_path):
    state = tmp_path / "state.json"
    reqs = [reader_request("q1", "SYS", "u1")]
    d = BatchDriver(_client(server), state)
    d.submit("reader", reqs)
    d.submit("reader", reqs, fresh=True)
    assert len(server.created_payloads) == 2


def test_fingerprint_mismatch_resubmits(server, tmp_path):
    state = tmp_path / "state.json"
    d = BatchDriver(_client(server), state)
    d.submit("reader", [reader_request("q1", "SYS", "u1")])
    # Different question set -> fingerprint changes -> resubmit.
    d.submit("reader", [reader_request("q2", "SYS", "u2")])
    assert len(server.created_payloads) == 2


def test_poll_loops_until_ended(server, tmp_path):
    server.state.default_polls_until_ended = 3  # 3 in-progress polls, then ended
    driver = BatchDriver(_client(server), tmp_path / "state.json")
    batch_id = driver.submit("reader", [reader_request("q1", "SYS", "u1")])
    batch = driver.poll(batch_id, interval_s=0)  # interval 0 to keep the test fast
    assert batch.processing_status == "ended"
    # 3 in-progress + 1 ending retrieve == 4 (poll() only counts; results() not called here)
    assert server.state.poll_counts[batch_id] == 4


def test_collect_maps_custom_id_and_usage(server, tmp_path):
    driver = BatchDriver(_client(server), tmp_path / "state.json")
    batch_id = driver.submit(
        "reader",
        [reader_request("q1", "SYS", "u1"), reader_request("q2", "SYS", "u2")],
    )
    results = driver.collect(batch_id)
    by_id = {r.custom_id: r for r in results}
    assert set(by_id) == {"q1", "q2"}
    assert by_id["q1"].status == "succeeded"
    assert by_id["q1"].text == "answer for q1"
    assert by_id["q1"].input_tokens == 11
    assert by_id["q1"].output_tokens == 3


def test_collect_tolerates_per_item_errors(server, tmp_path):
    # Mixed results: one succeeded, one errored — collect must not abort.
    server.state.default_results = [
        server.succeeded_row("q1", "good answer"),
        server.errored_row("q2"),
    ]
    driver = BatchDriver(_client(server), tmp_path / "state.json")
    batch_id = driver.submit(
        "reader",
        [reader_request("q1", "SYS", "u1"), reader_request("q2", "SYS", "u2")],
    )
    results = driver.collect(batch_id)
    by_id = {r.custom_id: r for r in results}
    assert by_id["q1"].status == "succeeded"
    assert by_id["q1"].text == "good answer"
    assert by_id["q2"].status == "errored"
    assert by_id["q2"].text == ""
    assert by_id["q2"].error  # carries detail


def test_end_to_end_run_two_questions(server, tmp_path):
    server.state.default_polls_until_ended = 1
    driver = BatchDriver(_client(server), tmp_path / "state.json")
    reqs = [reader_request("q1", "SYS", "u1"), reader_request("q2", "SYS", "u2")]
    results = driver.run("reader", reqs, interval_s=0)
    assert len(results) == 2
    assert all(r.status == "succeeded" for r in results)
    assert {r.text for r in results} == {"answer for q1", "answer for q2"}
