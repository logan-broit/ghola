# Contributing

PRs welcome. The bar is "would this make the recall pipeline better,
the code easier to read, or the docs less wrong."

## Prereqs

- Go 1.25+
- Docker (for the local dev stack)
- A GPU is **recommended** but not required — see below
- Optional: Rust + `cargo pgrx` if you want to build the retired
  `attic/extension/` crate; not in the build graph, not needed for
  normal development

## Build + run

```sh
git clone https://github.com/logan-broit/ghola
cd ghola
make all          # binaries
make dev-up       # dev stack via docker compose (ghola daemon: scripts/dev-up.sh --with-ghola)
```

See [`docs/development.md`](docs/development.md) for component-level
make targets and env knobs.

## Tests

```sh
make test         # go test ./... (one module: ghola + chapterhouse)
```

Chapterhouse's Postgres tests start their own `pgvector/pgvector:pg17`
via testcontainers, so Docker must be running; the dev stack doesn't.
CI (`.github/workflows/ci.yml`) runs three jobs: `go` (`go vet`,
`go build`, `go test -race ./...`), `python` (mentat + truthsayer
pytest) and `python-qa` (`bench/longmemeval/qa` pytest).

If you're changing the recall pipeline, please run the seeding-eval
sweep before opening a PR (smaller, faster than LongMemEval-S):
```sh
cd seeding-eval && .venv/bin/seeding-eval-run ...
```
For pipeline-level changes also run the LongMemEval-S harness per
[`bench/longmemeval/README.md`](bench/longmemeval/README.md) and
include the R@5/R@1/MRR delta in the PR description.

## PR norms

- One concern per PR. Refactors and behavior changes don't mix well.
- Conventional commit prefix (`feat:`, `fix:`, `chore:`, `docs:`,
  `bench:`) — see git log for style.
- Explain the *why* in the commit body, not just the *what*. Code
  shows what changed; the commit message should explain motivation.
- Run `go vet ./...` and `gofmt -w .` before pushing.

## Reporting bugs / asking questions

Open an [issue](https://github.com/logan-broit/ghola/issues). For
security reports, see [`SECURITY.md`](SECURITY.md).
