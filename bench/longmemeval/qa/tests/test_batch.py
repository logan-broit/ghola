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
    fingerprint,
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


# --- Issue 6: fingerprint folds in request params ---------------------------


def test_fingerprint_changes_when_params_change():
    # Same custom_id set, different bodies (e.g. changed K / prompt) -> the
    # fingerprint must differ so resume does NOT reuse the stale batch.
    a = fingerprint([reader_request("q1", "SYS", "user text A")])
    b = fingerprint([reader_request("q1", "SYS", "user text B")])
    assert a != b


def test_fingerprint_stable_for_identical_work():
    a = fingerprint([reader_request("q1", "SYS", "same"), reader_request("q2", "SYS", "x")])
    b = fingerprint([reader_request("q1", "SYS", "same"), reader_request("q2", "SYS", "x")])
    assert a == b


def test_same_ids_changed_params_resubmits(server, tmp_path):
    state = tmp_path / "state.json"
    d = BatchDriver(_client(server), state)
    d.submit("reader", [reader_request("q1", "SYS", "u1")])
    # Identical question id but a different rendered prompt -> resubmit.
    d.submit("reader", [reader_request("q1", "SYS", "u1-CHANGED")])
    assert len(server.created_payloads) == 2


# --- Issue 1: orphaned-paid-batch window ------------------------------------


def test_pending_marker_written_before_create(server, tmp_path):
    # The state file must hold a {pending: true} marker with the request count
    # and created_after timestamp the instant before create() is called, so a
    # crash mid-submit leaves a trail to adopt from.
    state = tmp_path / "state.json"
    captured: dict = {}

    real_create = server.state  # not used; we hook the driver instead

    d = BatchDriver(_client(server), state)
    # Hook: capture the state file contents at create() time.
    orig = d._client.messages.batches.create

    def spy_create(*a, **k):
        captured["at_create"] = json.loads(state.read_text())
        return orig(*a, **k)

    d._client.messages.batches.create = spy_create
    d.submit("reader", [reader_request("q1", "SYS", "u1")])

    marker = captured["at_create"]["reader"]
    assert marker.get("pending") is True
    assert marker.get("n_requests") == 1
    assert "created_after" in marker
    assert "batch_id" not in marker  # not yet known

    # After submit returns, the marker is replaced with the real record.
    final = json.loads(state.read_text())["reader"]
    assert final.get("pending") is not True
    assert "batch_id" in final


def test_adopts_orphaned_batch_on_pending_marker(server, tmp_path, capsys):
    # Simulate a crash between create() and the state rewrite: the state file
    # has only a pending marker, but a paid batch exists matching it. Resume
    # must adopt it (NOT resubmit) and warn loudly.
    state = tmp_path / "state.json"
    reqs = [reader_request("q1", "SYS", "u1")]
    fp = fingerprint(reqs)

    orphan = server.register_external_batch(n_requests=1, created_at="2026-06-10T00:00:01Z")
    state.write_text(json.dumps({
        "reader": {
            "pending": True,
            "fingerprint": fp,
            "n_requests": 1,
            "created_after": "2026-06-10T00:00:00Z",
        }
    }))

    created_before = len(server.created_payloads)
    d = BatchDriver(_client(server), state)
    batch_id = d.submit("reader", reqs)
    assert batch_id == orphan
    assert len(server.created_payloads) == created_before  # adopted, not resubmitted
    err = capsys.readouterr().err
    assert "adopt" in err.lower()
    # The state file now records the adopted batch as a committed (non-pending)
    # record.
    saved = json.loads(state.read_text())["reader"]
    assert saved["batch_id"] == orphan
    assert saved.get("pending") is not True


def test_pending_marker_no_match_resubmits_with_warning(server, tmp_path, capsys):
    # Pending marker but no batch matches (none created after the timestamp /
    # wrong count) -> resubmit, and warn that no orphan was found.
    state = tmp_path / "state.json"
    reqs = [reader_request("q1", "SYS", "u1")]
    fp = fingerprint(reqs)
    state.write_text(json.dumps({
        "reader": {
            "pending": True,
            "fingerprint": fp,
            "n_requests": 1,
            "created_after": "2030-01-01T00:00:00Z",  # in the future, nothing after it
        }
    }))
    d = BatchDriver(_client(server), state)
    d.submit("reader", reqs)
    assert len(server.created_payloads) == 1  # resubmitted
    err = capsys.readouterr().err
    assert "warning" in err.lower() or "no" in err.lower()


def test_adopt_override_skips_create(server, tmp_path):
    # Manual --adopt: caller passes a batch_id; submit must use it verbatim and
    # never call create().
    state = tmp_path / "state.json"
    orphan = server.register_external_batch(n_requests=1, created_at="2026-06-10T00:00:01Z")
    d = BatchDriver(_client(server), state)
    batch_id = d.submit("reader", [reader_request("q1", "SYS", "u1")], adopt=orphan)
    assert batch_id == orphan
    assert len(server.created_payloads) == 0
    saved = json.loads(state.read_text())["reader"]
    assert saved["batch_id"] == orphan


# --- Issue 2: stale state 404 self-heals ------------------------------------


def test_stale_batch_id_404_resubmits(server, tmp_path, capsys):
    # A committed state record whose batch_id the API no longer knows must
    # self-heal: drop the stale entry, warn loudly, resubmit.
    state = tmp_path / "state.json"
    reqs = [reader_request("q1", "SYS", "u1")]
    d = BatchDriver(_client(server), state)
    old_id = d.submit("reader", reqs)
    server.mark_gone(old_id)  # API forgets it

    # run() begins with retrieve on the recorded id -> 404 -> resubmit + collect.
    d2 = BatchDriver(_client(server), state)
    results = d2.run("reader", reqs, interval_s=0)
    assert {r.custom_id for r in results} == {"q1"}
    assert len(server.created_payloads) == 2  # resubmitted after the 404
    err = capsys.readouterr().err
    assert old_id in err
    assert "no longer exists" in err.lower() or "resubmit" in err.lower()


# --- Issue 3: poll heartbeat + 24h wall-clock bound -------------------------


def test_poll_emits_heartbeat(server, tmp_path, capsys):
    server.state.default_polls_until_ended = 2
    d = BatchDriver(_client(server), tmp_path / "state.json")
    bid = d.submit("reader", [reader_request("q1", "SYS", "u1")])
    d.poll(bid, interval_s=0)
    err = capsys.readouterr().err
    # Heartbeat renders request_counts: succeeded / errored / processing.
    assert "succeeded" in err.lower()
    assert "processing" in err.lower()


def test_poll_raises_past_wall_clock_bound(server, tmp_path):
    # A batch that never ends, with a tiny max-wall-clock, must raise (not spin
    # forever) and name the batch_id.
    server.state.default_polls_until_ended = 10_000
    d = BatchDriver(_client(server), tmp_path / "state.json")
    bid = d.submit("reader", [reader_request("q1", "SYS", "u1")])
    with pytest.raises(TimeoutError) as ei:
        d.poll(bid, interval_s=0, max_wall_clock_s=0)
    assert bid in str(ei.value)
