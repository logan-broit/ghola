import json
from pathlib import Path
import pytest

from mentat.weights import WeightsLoader, flip_to


def test_load_current_cold_when_symlink_missing(tmp_path):
    state = WeightsLoader(tmp_path).load_current()
    assert state.cold_start is True
    assert state.version is None


def test_load_current_cold_when_metadata_malformed(tmp_path):
    (tmp_path / "v1").mkdir()
    (tmp_path / "v1" / "metadata.json").write_text("{ not valid json")
    (tmp_path / "current").symlink_to("v1")
    state = WeightsLoader(tmp_path).load_current()
    assert state.cold_start is True


def test_load_current_cold_when_version_key_missing(tmp_path):
    (tmp_path / "v1").mkdir()
    (tmp_path / "v1" / "metadata.json").write_text(json.dumps({"trained_at": "..."}))
    (tmp_path / "current").symlink_to("v1")
    state = WeightsLoader(tmp_path).load_current()
    assert state.cold_start is True


def test_load_current_returns_version_when_metadata_valid(tmp_path):
    (tmp_path / "v1").mkdir()
    (tmp_path / "v1" / "metadata.json").write_text(json.dumps({"version": "v1"}))
    (tmp_path / "current").symlink_to("v1")
    state = WeightsLoader(tmp_path).load_current()
    assert state.cold_start is False
    assert state.version == "v1"


def test_flip_to_rejects_missing_directory(tmp_path):
    with pytest.raises(FileNotFoundError):
        flip_to(tmp_path, "v999")


def test_flip_to_rejects_directory_without_metadata(tmp_path):
    (tmp_path / "v1").mkdir()
    with pytest.raises(FileNotFoundError):
        flip_to(tmp_path, "v1")


def test_flip_to_atomic_swap(tmp_path):
    for v in ("v1", "v2"):
        (tmp_path / v).mkdir()
        (tmp_path / v / "metadata.json").write_text(json.dumps({"version": v}))

    flip_to(tmp_path, "v1")
    assert (tmp_path / "current").resolve().name == "v1"

    flip_to(tmp_path, "v2")
    assert (tmp_path / "current").resolve().name == "v2"
