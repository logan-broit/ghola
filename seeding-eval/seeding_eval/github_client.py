"""Thin PyGithub wrapper — token resolution, PR filtering, rate-limit retries.

Token resolution prefers `GITHUB_TOKEN` env, falls back to `gh auth token`
subprocess. The fallback exists because interactive workstation use leans on
gh CLI's keyring, while CI sets the env var directly — both paths must work
without the caller knowing which one fired.

The closed-issues endpoint returns PRs as well (GitHub treats PRs as a kind
of issue server-side); `pull_request` is non-None on those records. Filtering
here — not at the extract layer — keeps the boundary clean: callers consuming
.issues() get only true issues.

Rate-limit handling sleeps until reset rather than backing off geometrically.
PyGithub surfaces the reset time on the exception, so we honor it directly
and retry once. A second hit re-raises — running into rate limits twice in
one extract pass means the workload is too large for the token, not a
transient blip.
"""
from __future__ import annotations

import logging
import os
import subprocess
import time
from collections.abc import Iterator
from typing import TYPE_CHECKING

from github import Auth, Github
from github.GithubException import RateLimitExceededException

if TYPE_CHECKING:
    from github.Commit import Commit
    from github.Issue import Issue
    from github.PullRequest import PullRequest
    from github.TimelineEvent import TimelineEvent

logger = logging.getLogger(__name__)

# How often to log remaining requests during a pull. 25 issues per log line
# is enough to notice rate-limit pressure without spamming.
_RATE_LOG_INTERVAL = 25


def _get_github_token() -> str:
    """Resolve a GitHub token from env or the gh CLI's keyring.

    Raises RuntimeError if neither path yields a token — better to fail at
    construction time than to fire an unauthenticated request and get a 401
    deep inside a paginated walk.
    """
    token = os.environ.get("GITHUB_TOKEN")
    if token:
        return token
    try:
        result = subprocess.run(
            ["gh", "auth", "token"],
            capture_output=True,
            text=True,
            check=True,
        )
    except (FileNotFoundError, subprocess.CalledProcessError) as e:
        raise RuntimeError(
            "No GITHUB_TOKEN env var and `gh auth token` failed — "
            "log in with `gh auth login` or set GITHUB_TOKEN."
        ) from e
    token = result.stdout.strip()
    if not token:
        raise RuntimeError("`gh auth token` returned empty output")
    return token


class GHClient:
    """PyGithub wrapper with token fallback and rate-limit retry."""

    def __init__(self, token: str | None = None):
        token = token or _get_github_token()
        self._gh = Github(auth=Auth.Token(token))

    def issues(
        self,
        repo: str,
        *,
        state: str = "closed",
        limit: int | None = None,
    ) -> Iterator["Issue"]:
        """Yield issues from `repo`, skipping PRs, up to `limit` records.

        PaginatedList is lazy — slicing with [:limit] does not pre-fetch all
        pages. The PR filter runs after pagination (we can't tell server-side),
        so the actual request count slightly exceeds `limit` when the repo has
        many closed PRs interleaved.
        """
        repository = self._gh.get_repo(repo)
        # PyGithub's get_issues default sort=created, direction=desc — newest first.
        paginated = repository.get_issues(state=state)

        yielded = 0
        seen = 0
        for issue in self._iter_with_retry(paginated):
            seen += 1
            if seen % _RATE_LOG_INTERVAL == 0:
                self._log_rate()
            if issue.pull_request is not None:
                continue
            yield issue
            yielded += 1
            if limit is not None and yielded >= limit:
                return

    def merged_prs(
        self,
        repo: str,
        *,
        limit: int | None = None,
    ) -> Iterator["PullRequest"]:
        """Yield merged PRs from `repo`, newest-first, up to `limit` records.

        The closed-issues path conflates "issue closed by stale-bot" with
        "issue closed by a fix" — on vercel/next.js the former dominates.
        Walking merged PRs and chasing what they close inverts the
        sampling: every yielded PR was, by construction, merged by a
        human (or merge-queue bot, but the body still shows the closure
        directives we care about).

        PyGithub's `get_pulls(state="closed")` mixes merged + abandoned;
        we filter `merged_at is None` here so callers see only landed
        PRs. The PaginatedList stays lazy — slicing with [:limit] does
        not pre-fetch all pages.
        """
        repository = self._gh.get_repo(repo)
        # state=closed sorted by created desc — newest-first matches the
        # closed-issues iterator's mental model.
        paginated = repository.get_pulls(state="closed", sort="created", direction="desc")

        yielded = 0
        seen = 0
        for pr in self._iter_with_retry(paginated):
            seen += 1
            if seen % _RATE_LOG_INTERVAL == 0:
                self._log_rate()
            # Skip un-merged closed PRs (abandoned / superseded). We want
            # only landed work that actually fixed something.
            if pr.merged_at is None:
                continue
            yield pr
            yielded += 1
            if limit is not None and yielded >= limit:
                return

    def issue(self, repo: str, issue_number: int) -> "Issue":
        """Return an Issue object by number."""
        return self._gh.get_repo(repo).get_issue(issue_number)

    def issue_top_comment(self, repo: str, issue_number: int) -> dict | None:
        """Return the top-voted comment on an issue, or None.

        Definition: the comment with the most ``+1`` reactions wins.
        Ties broken by most-recent ``created_at``. Comments with zero
        ``+1`` reactions are ignored — issues with thousands of "me too"
        comments and no reaction signal still produce None, which is what
        we want (no signal beats noisy signal). Bot-authored comments are
        skipped via ``filters.is_bot``: a bot's comment is not user
        signal regardless of who reacted.

        Returns ``{body, user, created_at, plus_one_count}`` or None.
        Body is capped at 2000 chars (post-strip) to bound event growth.
        """
        repository = self._gh.get_repo(repo)
        gh_issue = repository.get_issue(issue_number)
        comments = list(self._iter_with_retry(gh_issue.get_comments()))
        return rank_top_comment(comments)

    def search_resolving_prs(
        self, repo: str, issue_number: int
    ) -> Iterator["PullRequest"]:
        """Yield merged PRs in `repo` that mention `issue_number` in their body.

        Backstop for the timeline path: vercel/next.js (and many large
        OSS repos) close issues via stale-bot or maintainer comment, leaving
        no `cross-referenced` event with a PR source. The auto-close keyword
        (Closes #N) lives in the *PR* body, but isn't surfaced as an explicit
        timeline event on the issue.

        The caller should still confirm the linkage by running
        parse_closes_directives() on the PR body — this search returns
        candidates, not confirmed closers.
        """
        q = f"repo:{repo} is:pr is:merged {issue_number} in:body"
        # search_issues returns Issue records; convert each to PullRequest.
        results = self._gh.search_issues(q)
        for hit in self._iter_with_retry(results):
            yield self.pr(repo, hit.number)

    def issue_timeline(self, repo: str, issue_number: int) -> Iterator["TimelineEvent"]:
        """Yield TimelineEvent objects for an issue.

        Timeline includes the richer event surface (cross-references, label
        changes, closed-by-commit links) that Issue.get_events() omits. We
        always want timeline for resolution.
        """
        repository = self._gh.get_repo(repo)
        issue = repository.get_issue(issue_number)
        yield from self._iter_with_retry(issue.get_timeline())

    def pr(self, repo: str, pr_number: int) -> "PullRequest":
        """Return a PullRequest object."""
        return self._gh.get_repo(repo).get_pull(pr_number)

    def pr_commits(self, repo: str, pr_number: int) -> Iterator["Commit"]:
        """Yield commits in a PR. Each commit's `.files` contains touched files."""
        pull = self.pr(repo, pr_number)
        yield from self._iter_with_retry(pull.get_commits())

    def _iter_with_retry(self, paginated) -> Iterator:
        """Iterate a PaginatedList (or any iterable), sleeping through one
        rate-limit hit.

        A second hit during the retry walk re-raises — see module docstring.
        Used for Issues, TimelineEvents, PR Commits — any PyGithub paginator.
        """
        retried = False
        it = iter(paginated)
        while True:
            try:
                yield next(it)
            except StopIteration:
                return
            except RateLimitExceededException:
                if retried:
                    raise
                retried = True
                self._sleep_until_reset()
                # Restart iteration; PyGithub caches pages so this is cheap.
                it = iter(paginated)

    def _sleep_until_reset(self) -> None:
        """Block until the core rate limit resets, plus a 5s safety margin."""
        rl = self._gh.get_rate_limit()
        reset = rl.core.reset.timestamp()
        now = time.time()
        wait = max(0.0, reset - now) + 5.0
        logger.warning(
            "rate limit hit; sleeping %.0fs until reset (remaining=%d)",
            wait,
            rl.core.remaining,
        )
        time.sleep(wait)

    def _log_rate(self) -> None:
        try:
            rl = self._gh.get_rate_limit()
            logger.info(
                "github rate: %d/%d remaining",
                rl.core.remaining,
                rl.core.limit,
            )
        except Exception:  # pragma: no cover — logging best-effort
            pass


def rank_top_comment(comments) -> dict | None:
    """Pick the top-voted comment from an iterable of PyGithub-like
    IssueComment objects.

    Pure function — no API surface. Splits the API-fetch step (live in
    GHClient.issue_top_comment) from the ranking logic so the latter is
    testable against synthetic comment objects without a real GitHub
    round-trip. Each comment must expose ``.body``, ``.user.login``,
    ``.created_at`` (datetime), and ``.reactions`` (dict with optional
    ``"+1"`` key).
    """
    from .filters import is_bot

    best: tuple[int, str, dict] | None = None  # (plus_one, created_at_iso, record)
    for c in comments:
        user = c.user.login if getattr(c, "user", None) else ""
        if user and is_bot(user):
            continue
        # PyGithub exposes the cached reactions dict on the comment
        # itself — no extra round-trip per comment. Shape:
        # {"+1": N, "-1": N, "laugh": N, ...}
        reactions = getattr(c, "reactions", None) or {}
        plus_one = int(reactions.get("+1", 0) or 0)
        if plus_one <= 0:
            continue
        created_iso = c.created_at.isoformat() if getattr(c, "created_at", None) else ""
        body = (c.body or "").strip()
        if len(body) > 2000:
            body = body[:2000]
        record = {
            "body": body,
            "user": user,
            "created_at": created_iso,
            "plus_one_count": plus_one,
        }
        # Tie-break: more +1s wins; on equal +1s, most-recent wins.
        key = (plus_one, created_iso)
        if best is None or key > (best[0], best[1]):
            best = (plus_one, created_iso, record)

    return best[2] if best else None
