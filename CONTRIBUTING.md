# Contributing

PRs welcome. The bar is "would this make the recall pipeline better,
the code easier to read, or the docs less wrong."

## Prereqs

- Go 1.22+
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
make dev-up       # full stack via docker compose
```

See [`docs/development.md`](docs/development.md) for component-level
make targets and env knobs.

## Tests

```sh
make test         # Go + Rust, requires the dev stack running
```

The local make target runs the full Go suite (incl. tests that need a
live Postgres). CI runs the subset that doesn't need DB —
`go test ./internal/auth/... ./internal/mneme/...` etc.

If you're changing the recall pipeline, please run the seeding-eval
sweep before opening a PR (smaller, faster than LongMemEval-S):
```sh
cd seeding-eval && .venv/bin/python -m seeding_eval ...
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
- For changes inside `_chapterhouse/ch-server/`, run vet/build from
  inside that dir with `GOWORK=off`.

## Reporting bugs / asking questions

Open an [issue](https://github.com/logan-broit/ghola/issues). For
security reports, see [`SECURITY.md`](SECURITY.md).
