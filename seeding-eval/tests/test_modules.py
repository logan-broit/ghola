from __future__ import annotations

import pytest

from seeding_eval.modules import bucket_for, primary_bucket


# bucket_for — depth=3 normalization

def test_bucket_depth_3_normalization():
    assert bucket_for("packages/next/src/server/foo.ts") == "packages/next/src"

def test_bucket_at_exactly_depth_3_dir():
    assert bucket_for("packages/next/src") == "packages/next/src"

def test_bucket_with_file_at_depth_3():
    # packages/next/src/foo.ts: 3-segment dir + file → bucket is "packages/next/src"
    assert bucket_for("packages/next/src/foo.ts") == "packages/next/src"

def test_bucket_2_segment_path():
    # 2-segment dir + file → bucket is the dir part
    assert bucket_for("packages/foo.ts") == "packages"

def test_bucket_3_segments_no_extension():
    # Directory-only 2-segment + file (no extension): treat the file as a leaf
    assert bucket_for("packages/next/Makefile") == "packages/next"

def test_bucket_shallow_root_file():
    assert bucket_for("README.md") == "."

def test_bucket_empty_string():
    assert bucket_for("") == "."

def test_bucket_leading_slash_stripped():
    # GitHub paths don't usually have leading slash, but be defensive
    assert bucket_for("/packages/next/src/server/foo.ts") == "packages/next/src"

def test_bucket_dot_slash_normalized():
    assert bucket_for("./packages/next/src/foo.ts") == "packages/next/src"

# primary_bucket — most-frequent + alphabetical tiebreak

def test_primary_most_frequent():
    paths = [
        "packages/next/src/server/a.ts",
        "packages/next/src/server/b.ts",
        "packages/next/src/client/c.ts",
    ]
    # All three share depth-3 bucket "packages/next/src" → that wins trivially
    assert primary_bucket(paths) == "packages/next/src"

def test_primary_two_buckets_one_more_frequent():
    paths = [
        "packages/next/src/foo.ts",
        "packages/next/src/bar.ts",
        "examples/blog/page.ts",
    ]
    # 2 vs 1 → "packages/next/src" wins
    assert primary_bucket(paths) == "packages/next/src"

def test_primary_tie_alphabetical_first():
    paths = [
        "packages/next/foo.ts",   # bucket: packages/next
        "examples/blog/page.ts",  # bucket: examples/blog
    ]
    # 1 each → alphabetical first → "examples/blog"
    assert primary_bucket(paths) == "examples/blog"

def test_primary_three_way_tie_alphabetical():
    paths = [
        "z/foo.ts",               # bucket: z
        "a/foo.ts",               # bucket: a
        "m/foo.ts",               # bucket: m
    ]
    assert primary_bucket(paths) == "a"

def test_primary_empty_raises():
    with pytest.raises(ValueError):
        primary_bucket([])
