-- Baseline extensions + namespaces used by every subsequent episodic
-- migration. Idempotent — safe to re-run.

CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS vector;    -- pgvector

CREATE SCHEMA IF NOT EXISTS episodic;
