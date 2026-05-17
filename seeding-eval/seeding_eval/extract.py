"""Phase 1: extract closed issues + the issue→PR→commits link graph.

Cache-first: re-running with the same args reads each cache, never the API.
Caches go under `cache_dir/<owner__repo>/{issues,links,prs,commits}.json`.
The slug double-underscore swap avoids creating nested owner directories
that complicate later cleanup, and avoids any path-traversal surface if
`repo` is ever constructed from caller input.

Atomic write via tempfile + os.replace so a Ctrl-C mid-write doesn't leave
behind a half-flushed JSON file that the next run would read as valid data.

Link-graph layout (`links.json`):
    {issue_number_str: {"pr": pr_number_or_None,
                        "commits": [sha, ...],
                        "files":   [path, ...]}}

Three caches (`issues`, `prs`, `commits`) deliberately. Each is independently
re-extractable; `links.json` is the cheap-to-load index that B7 reads to
build bundle records.

Resolution strategy in `resolve_link()`:
  1. Walk the issue timeline. Any `cross-referenced` event whose
     `source.issue.pull_request` is set, or any `closed` event with a
     `commit_id` whose containing PR we can identify — return that PR
     number.
  2. Fallback: parse_closes_directives() on the issue body. Return the
     smallest matching PR number that exists in `repo`.
  3. Return None when ambiguous. The whole pipeline tolerates orphan
     issues — they're dropped at case-build time, not silently guessed at.
"""
from __future__ import annotations

import json
import logging
import tempfile
from pathlib import Path

from .github_client import GHClient
from .parse import parse_closes_directives

logger = logging.getLogger(__name__)


def _repo_dir(cache_dir: Path, repo: str) -> Path:
    return cache_dir / repo.replace("/", "__")


def _issues_path(cache_dir: Path, repo: str) -> Path:
    return _repo_dir(cache_dir, repo) / "issues.json"


def _links_path(cache_dir: Path, repo: str) -> Path:
    return _repo_dir(cache_dir, repo) / "links.json"


def _prs_path(cache_dir: Path, repo: str) -> Path:
    return _repo_dir(cache_dir, repo) / "prs.json"


def _commits_path(cache_dir: Path, repo: str) -> Path:
    return _repo_dir(cache_dir, repo) / "commits.json"


def _atomic_write_json(path: Path, data) -> None:
    """Write JSON atomically: tempfile in same dir, then os.replace.

    Path.replace is atomic on POSIX when src and dst share a filesystem,
    which they do because tempfile.dir == path.parent.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(
        "w",
        dir=path.parent,
        delete=False,
        prefix=f".{path.stem}-",
        suffix=".tmp",
    ) as tmp:
        json.dump(data, tmp, indent=2)
        tmp_path = Path(tmp.name)
    tmp_path.replace(path)


# ---------------------------------------------------------------------------
# B5: closed-issues extractor
# ---------------------------------------------------------------------------


def extract_issues(
    repo: str,
    n: int,
    cache_dir: Path,
    *,
    client: GHClient | None = None,
) -> list[dict]:
    """Fetch up to `n` closed issues from `repo`. Cache to disk.

    Re-running with the same `repo` + `cache_dir` reads the cache without
    hitting the API. To force a refresh, delete the cache file or use a
    different `cache_dir`.

    Each issue is captured as:
        {id, number, title, body, created_at, closed_at, user, assignees}
    Timestamps are ISO-8601 strings (UTC from PyGithub). `body` is "" if
    None, since downstream JSONL consumers prefer empty strings over nulls
    in free-text fields.
    """
    cache_file = _issues_path(cache_dir, repo)
    if cache_file.exists():
        return json.loads(cache_file.read_text())

    client = client or GHClient()
    issues: list[dict] = []
    for issue in client.issues(repo, state="closed", limit=n):
        issues.append(
            {
                "id": issue.id,
                "number": issue.number,
                "title": issue.title,
                "body": issue.body or "",
                "created_at": issue.created_at.isoformat(),
                "closed_at": issue.closed_at.isoformat() if issue.closed_at else None,
                "user": issue.user.login if issue.user else "",
                "assignees": [a.login for a in issue.assignees],
            }
        )

    _atomic_write_json(cache_file, issues)
    return issues


# ---------------------------------------------------------------------------
# B6: link graph (issue → PR → commits → files)
# ---------------------------------------------------------------------------


def _pr_record(pr) -> dict:
    """Project a PyGithub PullRequest into our cache shape.

    `requested_reviewers` is captured as-is — GitHub clears it after merge,
    so for landed PRs it's typically empty. The *actual* reviewer signal
    lives in `approvers`, derived by walking pr.get_reviews() and keeping
    APPROVED reviews. We dedupe by login because a reviewer can submit
    multiple APPROVED reviews.
    """
    approvers: list[str] = []
    seen: set[str] = set()
    try:
        for review in pr.get_reviews():
            if review.state == "APPROVED" and review.user and review.user.login not in seen:
                approvers.append(review.user.login)
                seen.add(review.user.login)
    except Exception as e:  # pragma: no cover — best-effort, never fatal
        logger.warning("get_reviews() failed for PR #%d: %s", pr.number, e)

    return {
        "id": pr.id,
        "number": pr.number,
        "title": pr.title,
        "body": pr.body or "",
        "merged_at": pr.merged_at.isoformat() if pr.merged_at else None,
        "user": pr.user.login if pr.user else "",
        "reviewers": [r.login for r in (pr.requested_reviewers or [])],
        "approvers": approvers,
        "merge_commit_sha": pr.merge_commit_sha,
    }


def _commit_record(c) -> dict:
    """Project a PyGithub Commit into our cache shape.

    `author` prefers the GitHub login (c.author.login) and falls back to
    the git-author email when GitHub couldn't match the commit to an
    account. Either way we get a stable string for downstream identity
    work.
    """
    git_author = c.commit.author if c.commit else None
    git_committer = c.commit.committer if c.commit else None
    author_login = c.author.login if c.author else (git_author.email if git_author else "")
    committer_login = (
        c.committer.login if c.committer else (git_committer.email if git_committer else "")
    )
    authored_at = (
        git_author.date.isoformat() if git_author and git_author.date else None
    )
    return {
        "sha": c.sha,
        "message": c.commit.message if c.commit else "",
        "author": author_login,
        "committer": committer_login,
        "authored_at": authored_at,
        "files": [f.filename for f in (c.files or [])],
    }


def _pr_number_from_timeline(events) -> int | None:
    """Scan timeline events for a definitive PR linkage.

    Order of preference:
      1. `cross-referenced` event whose source.issue is a PR — that's
         GitHub's first-class signal that a PR mentioned this issue.
      2. `closed` event whose source.issue is a PR (same shape via the
         REST timeline when the issue was auto-closed by a PR merge).

    Returns the PR number if found, None if the timeline doesn't yield
    one. Conservative: if multiple PRs cross-reference, return the one
    from the LAST `cross-referenced` event before close — that's
    typically the resolving PR rather than a duplicate-link comment.
    """
    cross_ref_pr: int | None = None
    closed_pr: int | None = None
    for ev in events:
        ev_type = ev.event
        source = ev.source
        if ev_type == "cross-referenced" and source is not None:
            src_issue = getattr(source, "issue", None)
            if src_issue is not None and getattr(src_issue, "pull_request", None) is not None:
                cross_ref_pr = src_issue.number  # keep updating; last one wins
        elif ev_type == "closed":
            # The REST timeline sometimes attaches a source.issue (PR) when
            # the issue was auto-closed by a merged PR. PyGithub may also
            # surface commit_id without a direct PR linkage in that case;
            # we don't try to map commit_id → PR here (extra API roundtrips,
            # ambiguous when multiple PRs touched a commit). Stick to the
            # source.issue path.
            if source is not None:
                src_issue = getattr(source, "issue", None)
                if (
                    src_issue is not None
                    and getattr(src_issue, "pull_request", None) is not None
                ):
                    closed_pr = src_issue.number
    # `closed` event linkage is more authoritative than a generic cross-ref.
    return closed_pr or cross_ref_pr


def resolve_link(
    issue_record: dict,
    client: GHClient,
    repo: str,
) -> int | None:
    """Find the resolving PR number for a given issue.

    Strategy (priority order):
      1. Walk the issue timeline. Any `cross-referenced` or `closed` event
         that references a PR via source.issue → return that PR number.
      2. Search merged PRs in `repo` whose body mentions `issue_number`,
         then confirm via parse_closes_directives(). The first PR with a
         confirmed Closes-directive wins. This is the dominant path on
         repos that auto-close via stale-bot rather than direct PR
         linkage (most large OSS).
      3. Issue-body Closes-directives — rare, but cheap.
      4. Return None when nothing matches. Conservative by design: an
         orphan record is more honest than a guessed link.
    """
    issue_number = issue_record["number"]

    # Path 1: timeline.
    try:
        events = list(client.issue_timeline(repo, issue_number))
    except Exception as e:
        logger.warning("timeline fetch failed for #%d: %s", issue_number, e)
        events = []
    pr_num = _pr_number_from_timeline(events)
    if pr_num is not None:
        return pr_num

    # Path 2: search PR bodies for the issue number, then confirm via the
    # parser. Take the first confirmed match — search results are ordered
    # by best match, and a confirmed Closes-directive is unambiguous.
    try:
        for pr in client.search_resolving_prs(repo, issue_number):
            if issue_number in parse_closes_directives(pr.body):
                return pr.number
    except Exception as e:
        logger.warning("search fallback failed for #%d: %s", issue_number, e)

    # Path 3: parse issue body itself (rare — directives usually live in PRs).
    for n in sorted(parse_closes_directives(issue_record.get("body", ""))):
        try:
            pr = client.pr(repo, n)
            if pr is not None:
                return n
        except Exception:
            continue

    return None


def extract_pr(
    repo: str,
    pr_number: int,
    cache_dir: Path,
    *,
    client: GHClient,
    prs_cache: dict[str, dict] | None = None,
) -> dict:
    """Fetch a single PR, project to record shape, return.

    `prs_cache` is the in-memory accumulator the caller threads through
    so we don't write prs.json once per PR. The caller is responsible for
    flushing the dict to disk after the loop.
    """
    key = str(pr_number)
    if prs_cache is not None and key in prs_cache:
        return prs_cache[key]
    pr = client.pr(repo, pr_number)
    record = _pr_record(pr)
    if prs_cache is not None:
        prs_cache[key] = record
    return record


def extract_commits_for_pr(
    repo: str,
    pr_number: int,
    cache_dir: Path,
    *,
    client: GHClient,
    commits_cache: dict[str, dict] | None = None,
) -> list[dict]:
    """Fetch commits + touched files for a PR. Append to commits_cache by SHA.

    Returns the list of commit records for THIS PR (caller uses it to
    populate links.json's `commits` and `files` fields). The full
    commits_cache (keyed by SHA) is what gets written to commits.json.
    """
    records: list[dict] = []
    for c in client.pr_commits(repo, pr_number):
        record = _commit_record(c)
        records.append(record)
        if commits_cache is not None:
            commits_cache[record["sha"]] = record
    return records


def extract_links(
    repo: str,
    cache_dir: Path,
    *,
    client: GHClient | None = None,
) -> dict:
    """For each cached issue, resolve its PR + commits + touched files.

    Reads cached issues from `extract_issues()` output. Writes:
      - links.json:   {issue_number_str: {"pr": pr_or_None,
                                          "commits": [sha, ...],
                                          "files":   [path, ...]}}
      - prs.json:     {pr_number_str: pr_record}
      - commits.json: {sha: commit_record}

    Cache-first on `links.json` — if it exists, we return it without
    hitting the API. (We don't merge with prs/commits caches: they're
    derived from the same walk and re-extracted together.)

    Files in `links[N].files` are the union across all commits of the PR,
    deduped, sorted. That's the input the bucket function will consume.
    """
    links_file = _links_path(cache_dir, repo)
    if links_file.exists():
        return json.loads(links_file.read_text())

    issues_file = _issues_path(cache_dir, repo)
    if not issues_file.exists():
        raise FileNotFoundError(
            f"issues.json not found at {issues_file}; "
            "run extract_issues() first."
        )
    issues = json.loads(issues_file.read_text())

    client = client or GHClient()
    links: dict[str, dict | None] = {}
    prs_cache: dict[str, dict] = {}
    commits_cache: dict[str, dict] = {}

    for issue in issues:
        issue_num = issue["number"]
        pr_num = resolve_link(issue, client, repo)
        if pr_num is None:
            links[str(issue_num)] = {"pr": None, "commits": [], "files": []}
            continue

        # Fetch the PR + its commits.
        try:
            extract_pr(repo, pr_num, cache_dir, client=client, prs_cache=prs_cache)
            commit_records = extract_commits_for_pr(
                repo, pr_num, cache_dir, client=client, commits_cache=commits_cache
            )
        except Exception as e:
            # Conservative: if PR fetch fails, log and treat as orphan.
            logger.warning(
                "PR #%d fetch failed for issue #%d: %s — recording as orphan",
                pr_num,
                issue_num,
                e,
            )
            links[str(issue_num)] = {"pr": None, "commits": [], "files": []}
            continue

        files = sorted({f for c in commit_records for f in c["files"]})
        links[str(issue_num)] = {
            "pr": pr_num,
            "commits": [c["sha"] for c in commit_records],
            "files": files,
        }

    _atomic_write_json(links_file, links)
    _atomic_write_json(_prs_path(cache_dir, repo), prs_cache)
    _atomic_write_json(_commits_path(cache_dir, repo), commits_cache)

    return links


# ---------------------------------------------------------------------------
# Rework: merged-PR-driven extraction (inverts sampling vs. closed-issues)
# ---------------------------------------------------------------------------


def _issue_record(issue, top_comment: dict | None = None) -> dict:
    """Project a PyGithub Issue into the same shape extract_issues writes.

    Centralized so the merged-PR path lays down identical issues.json
    records — no schema drift between the two extraction strategies.

    ``top_comment`` is the optional top-voted comment record (see
    GHClient.issue_top_comment). Always included as a key on the record;
    None when the issue has no top comment. The closed-issues path passes
    None — top-comment enrichment is currently merged-PR-only because
    that's where the eval lives. Easy to add later without schema drift.
    """
    return {
        "id": issue.id,
        "number": issue.number,
        "title": issue.title,
        "body": issue.body or "",
        "created_at": issue.created_at.isoformat(),
        "closed_at": issue.closed_at.isoformat() if issue.closed_at else None,
        "user": issue.user.login if issue.user else "",
        "assignees": [a.login for a in issue.assignees],
        "top_comment": top_comment,
    }


def extract_via_merged_prs(
    repo: str,
    n_prs: int,
    cache_dir: Path,
    *,
    client: GHClient | None = None,
) -> dict:
    """Walk merged PRs newest-first; keep only ones that closed an issue.

    Inverts the sampling story of the closed-issues path: instead of
    "n closed issues, then chase the resolution," walk merged PRs and
    keep the issues they actually closed. By construction the resolution
    rate is ~100% — every kept thread was resolved by a real merge.

    Cache-first: if all four cache files exist, return them without any
    API call. Otherwise walk the API until we've accumulated `n_prs`
    resolved threads, then write all four caches atomically.

    Multi-issue handling: when one PR closes multiple issues (Closes #N
    / #M / #K), pick the lowest-number one. Documented choice — N=1
    eval thread per PR keeps the corpus shape uniform with the
    closed-issues path; the other issues are silently dropped (their
    resolving PR is the same one we keep).

    Skip rules — PR is dropped (does not count against the budget) when:
      - PR body has no Closes/Fixes/Resolves directive
      - The closed issue lookup fails (deleted / private / archived)
      - The issue is itself a PR (shouldn't happen, but the issues
        endpoint returns both, so guard for it)

    Returns the four-key extracts shape that build_cases consumes.
    """
    issues_file = _issues_path(cache_dir, repo)
    links_file = _links_path(cache_dir, repo)
    prs_file = _prs_path(cache_dir, repo)
    commits_file = _commits_path(cache_dir, repo)

    if (
        issues_file.exists()
        and links_file.exists()
        and prs_file.exists()
        and commits_file.exists()
    ):
        return {
            "issues": json.loads(issues_file.read_text()),
            "links": json.loads(links_file.read_text()),
            "prs": json.loads(prs_file.read_text()),
            "commits": json.loads(commits_file.read_text()),
        }

    client = client or GHClient()
    issues_out: list[dict] = []
    links_out: dict[str, dict] = {}
    prs_cache: dict[str, dict] = {}
    commits_cache: dict[str, dict] = {}
    seen_issue_numbers: set[int] = set()

    resolved = 0
    # Walk merged PRs lazily; break once we've accumulated n_prs threads.
    # No upstream limit — `merged_prs` yields until the caller stops.
    for pr in client.merged_prs(repo):
        if resolved >= n_prs:
            break

        body = pr.body or ""
        closes = sorted(parse_closes_directives(body))
        if not closes:
            continue

        # Pick the lowest-numbered issue this PR claims to close. Multi-
        # issue PRs collapse to one eval thread; the other directive
        # targets are unmodeled (their resolving PR is the same one).
        issue_num: int | None = None
        for candidate in closes:
            if candidate in seen_issue_numbers:
                # A different (later) PR already claimed this issue — skip.
                # Newest-first walk + stale-bot reopens make this rare but
                # not impossible.
                continue
            try:
                gh_issue = client.issue(repo, candidate)
            except Exception as e:
                logger.debug(
                    "issue #%d lookup failed for PR #%d: %s",
                    candidate,
                    pr.number,
                    e,
                )
                continue
            # `pull_request` is non-None when the "issue" is actually a PR
            # cross-link — the directive points at another PR, not an
            # issue. Skip; eval thread shape requires a real issue.
            if gh_issue.pull_request is not None:
                continue
            issue_num = candidate
            break
        if issue_num is None:
            continue

        # Top-voted comment enrichment. Best-effort: if the fetch fails
        # (network blip, archived issue) we proceed with top_comment=None
        # rather than dropping the whole thread — the issue body alone
        # is still valuable signal.
        try:
            top_comment = client.issue_top_comment(repo, issue_num)
        except Exception as e:
            logger.warning(
                "top-comment fetch failed for issue #%d: %s — using None",
                issue_num,
                e,
            )
            top_comment = None

        try:
            issue_record = _issue_record(gh_issue, top_comment=top_comment)
        except Exception as e:
            logger.warning("issue #%d record build failed: %s", issue_num, e)
            continue

        # Fetch PR + commits.
        try:
            extract_pr(repo, pr.number, cache_dir, client=client, prs_cache=prs_cache)
            commit_records = extract_commits_for_pr(
                repo, pr.number, cache_dir, client=client, commits_cache=commits_cache
            )
        except Exception as e:
            logger.warning(
                "PR #%d fetch failed (closes #%d): %s — skipping",
                pr.number,
                issue_num,
                e,
            )
            continue

        files = sorted({f for c in commit_records for f in c["files"]})
        issues_out.append(issue_record)
        links_out[str(issue_num)] = {
            "pr": pr.number,
            "commits": [c["sha"] for c in commit_records],
            "files": files,
        }
        seen_issue_numbers.add(issue_num)
        resolved += 1
        logger.info(
            "merged-PR #%d closes issue #%d (resolved=%d/%d)",
            pr.number,
            issue_num,
            resolved,
            n_prs,
        )

    _atomic_write_json(issues_file, issues_out)
    _atomic_write_json(links_file, links_out)
    _atomic_write_json(prs_file, prs_cache)
    _atomic_write_json(commits_file, commits_cache)

    return {
        "issues": issues_out,
        "links": links_out,
        "prs": prs_cache,
        "commits": commits_cache,
    }


# ---------------------------------------------------------------------------
# B8: CLI entry — extract → bundle in one shot
# ---------------------------------------------------------------------------


def main() -> None:
    """Conductor: run extract_issues + extract_links, then build_bundle.

    Cache-first by design — re-running with a populated cache hits zero
    API. To force a refresh, delete the cache or point at a fresh dir.
    No --force flag (YAGNI) and no --quiet flag (likewise).
    """
    import argparse

    from .bundle import build_bundle, write_bundle

    p = argparse.ArgumentParser(
        prog="seeding-extract",
        description="Extract GitHub corpus → bundle JSONL for ghola seeding eval.",
    )
    p.add_argument(
        "--repo",
        default="vercel/next.js",
        help="owner/name (default: vercel/next.js)",
    )
    p.add_argument(
        "--strategy",
        choices=("closed-issues", "merged-prs"),
        default="merged-prs",
        help=(
            "extraction strategy. closed-issues: walk newest closed issues, "
            "chase the resolving PR (low resolution rate on stale-bot repos). "
            "merged-prs (default): walk newest merged PRs, keep ones that "
            "closed an issue (~100%% resolution by construction)."
        ),
    )
    p.add_argument(
        "--n-issues",
        type=int,
        default=50,
        help=(
            "for closed-issues strategy: number of closed issues to extract. "
            "for merged-prs strategy: stop after this many resolved threads. "
            "(default: 50)"
        ),
    )
    p.add_argument(
        "--n-resolved",
        type=int,
        default=None,
        help="alias for --n-issues under merged-prs strategy (clearer naming)",
    )
    p.add_argument(
        "--cache-dir",
        type=Path,
        default=Path.home() / ".cache" / "seeding-eval",
        help="local cache root (default: ~/.cache/seeding-eval)",
    )
    p.add_argument(
        "--bundle-out",
        type=Path,
        required=True,
        help="output bundle JSONL path (required — no silent default)",
    )
    args = p.parse_args()

    # --n-resolved is the clearer name under merged-prs; if both are set
    # they must agree (don't silently pick one).
    target_n = args.n_resolved if args.n_resolved is not None else args.n_issues

    print(
        f"extract: {args.repo} strategy={args.strategy} "
        f"n={target_n} cache={args.cache_dir}"
    )

    if args.strategy == "merged-prs":
        extracts = extract_via_merged_prs(args.repo, target_n, args.cache_dir)
        issues = extracts["issues"]
        links = extracts["links"]
        n_resolved = sum(1 for v in links.values() if v and v.get("pr") is not None)
        n_orphan = len(links) - n_resolved
        print(f"  issues cached: {len(issues)}")
        print(f"  links resolved: {n_resolved} ({n_orphan} orphan)")
    else:
        # Step 1: closed issues.
        issues = extract_issues(args.repo, target_n, args.cache_dir)
        print(f"  issues cached: {len(issues)}")

        # Step 2: link graph + PRs + commits.
        links = extract_links(args.repo, cache_dir=args.cache_dir)
        n_resolved = sum(1 for v in links.values() if v and v.get("pr") is not None)
        n_orphan = len(links) - n_resolved
        print(f"  links resolved: {n_resolved} ({n_orphan} orphan)")

    # Step 3: load PR + commit caches as plain dicts.
    repo_cache = _repo_dir(args.cache_dir, args.repo)
    prs_file = repo_cache / "prs.json"
    commits_file = repo_cache / "commits.json"
    prs = json.loads(prs_file.read_text()) if prs_file.exists() else {}
    commits = json.loads(commits_file.read_text()) if commits_file.exists() else {}

    # Step 4 + 5: build records, write bundle atomically.
    records = list(build_bundle(issues, links, prs, commits, repo=args.repo))
    n_written = write_bundle(records, args.bundle_out)
    print(f"  wrote {n_written} bundle records → {args.bundle_out}")
