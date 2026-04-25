"""Weights loader: `<root>/current` symlink with atomic flip via os.replace."""
import json
import os
from dataclasses import dataclass
from pathlib import Path


@dataclass
class WeightsState:
    root: Path
    version: str | None
    cold_start: bool


class WeightsLoader:
    def __init__(self, root: Path | str):
        self.root = Path(root)

    def load_current(self) -> WeightsState:
        current = self.root / "current"
        if not current.exists():
            return WeightsState(root=self.root, version=None, cold_start=True)
        target = current.resolve()
        if not target.is_dir():
            return WeightsState(root=self.root, version=None, cold_start=True)
        meta_path = target / "metadata.json"
        if not meta_path.exists():
            return WeightsState(root=self.root, version=None, cold_start=True)
        try:
            meta = json.loads(meta_path.read_text())
            version = meta["version"]
        except (json.JSONDecodeError, KeyError):
            return WeightsState(root=self.root, version=None, cold_start=True)
        return WeightsState(root=target, version=version, cold_start=False)


def flip_to(root: Path, new_version: str) -> None:
    root = Path(root)
    target = root / new_version
    if not target.is_dir():
        raise FileNotFoundError(f"weights: cannot flip to {new_version!r}; {target} is not a directory")
    if not (target / "metadata.json").is_file():
        raise FileNotFoundError(f"weights: cannot flip to {new_version!r}; {target / 'metadata.json'} missing")

    tmp = root / f".current.{new_version}.tmp"
    if tmp.exists():
        tmp.unlink()
    tmp.symlink_to(new_version, target_is_directory=True)
    os.replace(tmp, root / "current")  # atomic on POSIX
