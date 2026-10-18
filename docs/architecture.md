# Architecture

The full design lives in the doc "Helm v1 架构与 MVP 设计":
https://claude.ai/code/artifact/b9aaaf7f-aa43-4bf8-b12d-a259b304f19c

Decisions confirmed on 2026-10-18:

| Topic | Decision |
| --- | --- |
| Board | Build our own; copy components from Paperclip (MIT) and Vibe Kanban (Apache-2.0) where useful |
| Executor protocols | Native: Codex app-server (JSON-RPC over stdio) and Claude Code stream-json + PreToolUse hook; Executor interface shaped like ACP |
| Network | All machines on Tailscale; helmd connects outbound (WebSocket wake + HTTP claim + 30s poll fallback) |
| Brain framework | Keep LangGraph.js; the framework never owns state — the brain reads/writes Postgres through a BrainRuntime interface |
| Data compliance | External models may process code, experiment data and DingTalk docs |

Milestones: M0 skeleton → M1 control (contracts, gates, digests, Job executor) → M2 brain → M3 memory & skills.
