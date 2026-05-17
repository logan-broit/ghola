"""Module-path bucketing for H1.c discrete-context entropy.

Bucket = path segments joined to depth=3, with the leaf (a filename
or anything past depth-3) stripped. Captures "which submodule of
the codebase did this entity touch" without exploding into per-file
buckets.

primary_bucket() picks one bucket from a list of paths (e.g. files
touched by a single PR): most-frequent, ties broken alphabetical-first
for determinism.
"""
from __future__ import annotations

from collections import Counter

DEPTH = 3

# extensionless leaves we still treat as files (not directories)
_FILE_LIKE_NAMES = {"makefile", "dockerfile", "rakefile"}


def bucket_for(path: str) -> str:
    """Return the module bucket for a file path, depth=DEPTH."""
    norm = path.lstrip("/").removeprefix("./").strip()
    if not norm:
        return "."
    segments = norm.split("/")
    # more than DEPTH segments: take the first DEPTH dirs as the bucket
    if len(segments) > DEPTH:
        return "/".join(segments[:DEPTH])
    # exactly DEPTH segments: ambiguous (dir or dir+file?). use leaf shape
    # to decide — extension or known file-like name → file, else directory.
    if len(segments) == DEPTH:
        leaf = segments[-1]
        if "." in leaf or leaf.lower() in _FILE_LIKE_NAMES:
            return "/".join(segments[: DEPTH - 1])
        return "/".join(segments)
    # fewer than DEPTH segments: single token → root, else dir part
    if len(segments) == 1:
        return "."
    return "/".join(segments[:-1])


def primary_bucket(paths: list[str]) -> str:
    """Return the most-frequent bucket among `paths`.

    Tiebreak: alphabetical first. Raises ValueError on empty input.
    """
    if not paths:
        raise ValueError("primary_bucket requires non-empty paths")
    counts = Counter(bucket_for(p) for p in paths)
    max_count = max(counts.values())
    candidates = sorted(b for b, c in counts.items() if c == max_count)
    return candidates[0]
