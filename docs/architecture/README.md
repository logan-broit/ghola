# Architecture diagrams

Sources for the [archify](https://github.com/tt-a1i/archify) diagrams. Each
`*.archify.json` cites the files and lines it describes, pinned to the commit
in `meta.repository.revision`; re-pin and re-render when that code moves.

| Source | Shows |
|---|---|
| `build.archify.json` | one Go module and two images, and which compose service runs which binary |
| `write-path.archify.json` | record -> working tier (sietch) -> episodic -> semantic, with timers and writers |
| `recall.archify.json` | parallel tier fan-out, RRF, settle expansion, rerank, three-channel score |

Render (HTML is gitignored; about 850 KB each):

```sh
node <archify>/bin/archify.mjs finalize dataflow docs/architecture/recall.archify.json \
  docs/architecture/recall.html --repo-root . --quality showcase
```

Use `architecture` instead of `dataflow` for `build.archify.json`.
