from fastapi.testclient import TestClient

from mentat.app import app
from mentat.weights import WeightsLoader

client = TestClient(app)


def test_health_reports_cold_start_when_no_weights(tmp_path, monkeypatch):
    monkeypatch.setattr("mentat.app.weights_loader", WeightsLoader(tmp_path))
    r = client.get("/v1/health")
    assert r.status_code == 200
    body = r.json()
    assert body["cold_start"] is True
    assert body["weights_version"] is None
    assert body["status"] == "ok"


def test_pool_endpoint_returns_1024_dim():
    dim = 1024
    r = client.post("/v1/pool", json={
        "workspace_id": "00000000-0000-0000-0000-000000000010",
        "events": [
            {"type": "user",      "embedding": [0.1]*dim},
            {"type": "assistant", "embedding": [0.2]*dim},
        ],
    })
    assert r.status_code == 200
    assert len(r.json()["embedding"]) == dim


def test_predict_cold_start_is_identity():
    last = [0.5] * 1024
    r = client.post("/v1/predict", json={
        "workspace_id": "00000000-0000-0000-0000-000000000010",
        "history": [[0.1]*1024, last],
    })
    assert r.status_code == 200
    assert r.json()["embedding"] == last
