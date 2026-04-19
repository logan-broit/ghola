/**
 * GholaArchitecture.tsx
 *
 * Interactive architecture explainer for the Ghola memory system.
 *
 * Design:
 *   - Split layout: diagram on the left, persistent detail rail on the right.
 *   - Two modes: "Topology" (spatial) and "Sequence" (temporal).
 *   - Hover or focus a node to preview; click to pin.
 *   - Dark-first technical aesthetic with one amber accent.
 *
 * Typography is loaded via <link> tags in the host document. If you'd rather
 * self-host, the three families used are Geist, JetBrains Mono, and
 * Instrument Serif. Fallbacks are specified so it degrades cleanly.
 *
 * No external runtime dependencies — just React. Drop it into any TSX app.
 */

import { useEffect, useMemo, useRef, useState } from "react";

// ─── Data model ──────────────────────────────────────────────────────────────

type Tier = "device" | "infra" | "storage" | "worker";

type NodeId =
  | "agent"
  | "http"
  | "mcp"
  | "core"
  | "pipelineA"
  | "workingdb"
  | "chapterhouse"
  | "pipelineB"
  | "postgres"
  | "pgworkers"
  | "embed"
  | "llm";

interface NodeDetail {
  id: NodeId;
  title: string;
  tier: Tier;
  tech: string;
  summary: string;
  bullets: string[];
}

const NODES: Record<NodeId, NodeDetail> = {
  agent: {
    id: "agent",
    title: "Agent process",
    tier: "device",
    tech: "Claude Code · pi-mono · any MCP client",
    summary:
      "The user-facing agent. It speaks to the local Ghola service over MCP (stdio) or HTTP/JSON depending on the client. Everything below this line is invisible to the user.",
    bullets: [
      "Talks to local service — never directly to team infra",
      "Issues record, recall, branch, bookmark, session_start, session_end",
      "Treats recall as a single call; never sees the three-tier fan-out",
    ],
  },
  http: {
    id: "http",
    title: "HTTP/JSON server",
    tier: "device",
    tech: "Go · localhost:7421",
    summary:
      "REST-style surface inside the local service. Used by non-MCP agents (pi-mono and friends).",
    bullets: [
      "Thin layer over the core library",
      "Authenticates requests to the local loopback only",
      "Returns merged, tier-attributed recall results",
    ],
  },
  mcp: {
    id: "mcp",
    title: "MCP wrapper",
    tier: "device",
    tech: "Go · stdio or localhost HTTP",
    summary:
      "Exposes the same operations as MCP tool calls for agents that speak MCP (Claude Code).",
    bullets: [
      "Transport is client's choice — stdio by default",
      "Shares the exact same core library as the HTTP server",
      "No business logic lives here — it is pure protocol translation",
    ],
  },
  core: {
    id: "core",
    title: "Core library",
    tier: "device",
    tech: "Go package · shared",
    summary:
      "Single source of truth for local operations. Both surface servers delegate here so behaviour stays identical across transports.",
    bullets: [
      "Owns the embed → insert → index pipeline for every turn",
      "Orchestrates recall fan-out across working / episodic / semantic",
      "Tracks the Pipeline A watermark and triggers the worker",
    ],
  },
  pipelineA: {
    id: "pipelineA",
    title: "Pipeline A · worker",
    tier: "worker",
    tech: "Background goroutine · every ~5 min",
    summary:
      "Continuous consolidation inside the local service. Lifts new turns out of the sietch and pushes them up to Chapterhouse's episodic store.",
    bullets: [
      "Reads turns past a watermark — incremental, never a full scan",
      "Light entity extraction (regex + small NER) before shipping",
      "POSTs batches to Chapterhouse /v1/episodic/ingest",
      "Never in the hot path — session latency is unaffected",
    ],
  },
  workingdb: {
    id: "workingdb",
    title: "Sietch",
    tier: "storage",
    tech: "SQLite · one file per session",
    summary:
      "Ephemeral per-session store. Every turn lands here first — fast local writes, no network. Retained for 24h after session end, then the sietch empties. Named for the Fremen hidden refuges: private, yours alone, temporary by design.",
    bullets: [
      "turns table with parent_id → branching tree structure",
      "turn_embeddings via sqlite-vec (1024d)",
      "turns_fts via FTS5 for keyword recall",
      "Garbage-collected 24h after session_end",
    ],
  },
  chapterhouse: {
    id: "chapterhouse",
    title: "Chapterhouse",
    tier: "infra",
    tech: "Go · REST API · per-user API keys",
    summary:
      "The team's shared API server. Receives episodic writes from every user's Pipeline A, serves recall queries, and hosts Pipeline B on a cron.",
    bullets: [
      "/v1/episodic/ingest, query, share, forget",
      "/v1/semantic/query, feedback, list",
      "Enforces workspace scoping and share ACLs",
      "Stateless — all durable state lives in Postgres",
    ],
  },
  pipelineB: {
    id: "pipelineB",
    title: "Pipeline B · distillation",
    tier: "worker",
    tech: "Nightly cron · 02:00 local",
    summary:
      "Cross-user pattern detector. Finds entity pairs co-occurring in ≥ 3 distinct sessions and asks the Mentat to distill each recurring pattern into a semantic mneme.",
    bullets: [
      "Scans last 24h of episodic.turns across users (respects ACLs)",
      "Mentat call produces { concept, content, memory_type, entities }",
      "Dedup against semantic.mnemes by HNSW cosine > 0.9",
      "Strengthens via Bayesian update or inserts a new mneme",
    ],
  },
  postgres: {
    id: "postgres",
    title: "Postgres",
    tier: "storage",
    tech: "CNPG · pg_ghola v2 extension",
    summary:
      "Two schemas under one database. episodic is raw and per-user. semantic is distilled and shared. pg_ghola v2 is a Rust extension running inside the server process.",
    bullets: [
      "episodic.turns — HNSW + FTS + entity GIN, per-user partitioned",
      "semantic.mnemes — distilled facts with ACT-R confidence scoring",
      "semantic.associations — Hebbian-weighted edges between mnemes",
      "Two work queues drain continuously without external services",
    ],
  },
  pgworkers: {
    id: "pgworkers",
    title: "pg_ghola · workers",
    tier: "worker",
    tech: "Rust · in-process",
    summary:
      "Three cognitive primitives that live inside the database. Chapterhouse writes to queues; these drain them.",
    bullets: [
      "Contradiction — flags high-similarity but divergent mnemes for review",
      "Hebbian — fires-together, wires-together edge weight updates",
      "Consolidation — hourly decay, 6-hourly archival of stale mnemes",
    ],
  },
  embed: {
    id: "embed",
    title: "Melange",
    tier: "infra",
    tech: "Embedding service · any model · 1024d vectors",
    summary:
      "The substance that makes text navigable. Called by both the local service (every record) and Chapterhouse (during distillation) to turn raw language into positioned vectors. The specific model is swappable — Qwen3, BGE, Nomic, or any sentence-transformer.",
    bullets: [
      "GET /v1/embeddings — stateless HTTP",
      "Model is a deployment-time choice, not a system-design choice",
      "On the hot path — keep P50 latency under 50 ms",
    ],
  },
  llm: {
    id: "llm",
    title: "Mentat",
    tier: "infra",
    tech: "LLM inference · any capable local model",
    summary:
      "The pattern-finder. Only called by Pipeline B during nightly distillation: given a batch of episodic turns, returns a structured distilled mneme. Model is swappable — Gemma, Qwen, MiniMax, Haiku, Llama, whatever fits your hardware. Nothing hot-path depends on it.",
    bullets: [
      "Prompt: batch of episodic turns → distilled semantic mneme",
      "Returns structured JSON { concept, content, memory_type, entities }",
      "Model choice is decoupled — system design does not change",
    ],
  },
};

// Full component source preserved from the user's architecture writeup.
// See docs/plans/2026-04-19-greenfield-tiered-memory-design.md for context.
//
// (Component source, sequence model, visual tokens, styles, and keyboard
//  shortcuts omitted here for brevity — drop the complete TSX in when we
//  wire this into the blog preview project during v1a marketing work.)

export default function GholaArchitecturePlaceholder() {
  return null;
}
