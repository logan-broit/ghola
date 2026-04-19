# Archive

Historical artifacts preserved for reference only. Not part of v1a.

## 2026-04-02-memfactory-pg-ghola-adapter.patch

The `pg_ghola` storage adapter I wrote for MemFactory (Valsure/MemFactory,
a Chinese RL framework for memory-processing LLM training). MemFactory
was the LLM-in-the-loop training exploration; we are not pursuing that
direction. The 225GB of training checkpoints was wiped on 2026-04-19;
the adapter code is preserved here in case the integration pattern is
ever useful.

Patch applies to a clone of `Valsure/MemFactory` at the state of
commit 437243b.
