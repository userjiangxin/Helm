# Helm — guide for coding agents

Helm is a single-user, multi-machine task system with a "brain / hands" split:

- **Brain** (TypeScript, LangGraph.js, M2+): understands intent, writes Contracts, supervises runs. Stateless per wake.
- **Control plane** (`server/`, TypeScript + Hono + Postgres): the only source of truth. Board API, daemon API, NOTIFY fan-out.
- **Hands** (`daemon/`, Go, `helmd`): one per machine. Claims Runs, drives Codex / Claude Code, enforces policy, streams evidence.
- **Board** (`web/`, React, M0): the human's view — tasks, live runs, approvals, machines.

Full design: the "Helm v1 架构与 MVP 设计" doc (linked in `docs/architecture.md`). Read `docs/m0-tasks.md` to pick work.

## Repository layout

```
protocol/          wire protocol spec + shared JSON fixtures (contract tests for Go AND TS)
daemon/            helmd (Go 1.24, module github.com/helm/helm/daemon)
  cmd/helmd/         entrypoint: run | caps | hook pre-tool-use | version
  internal/wire/       protocol types (must round-trip protocol/fixtures)
  internal/policy/     reflex layer: scope + command rules → allow / deny / hold
  internal/hook/       Claude Code PreToolUse ⇄ unix socket ⇄ policy (fail-closed)
  internal/conn/       WebSocket wake + HTTP pull + spool + heartbeat
  internal/supervisor/ run lifecycle, directives, event numbering
  internal/executor/   Executor interface; codex.go / claude.go are stubs (M0 tasks D4, D5)
  internal/registry/   capability detection (agent CLIs, GPUs)
server/            helm-server (Node 22, TypeScript, Hono, postgres.js, zod 4, vitest)
  src/protocol.ts      zod mirror of the wire protocol
  src/store.ts         all daemon-facing SQL (claim, events, directives, leases) behind a tiny `Sql` interface
  src/app.ts           routes: /daemon/* (machine token), /api/* (board), SSE /api/stream
  src/server.ts        startServer() — used by index.ts (postgres.js) and tests (PGlite)
  test/                PGlite-backed tests; e2e.test.ts builds and runs the real helmd binary
db/migrations/     plain SQL, applied in filename order
web/               board (to be scaffolded in task W1)
```

## Commands

```bash
make test            # everything: go test, vitest (incl. e2e with real helmd), typecheck
cd daemon && go test ./...
cd server && pnpm test && pnpm typecheck
make dev-db          # Postgres 17 + pgvector in docker
```

Go module fetches: if `sum.golang.org` is unreachable in your sandbox, use `GOPROXY=direct GONOSUMDB=<module>`; never disable TLS.

## Invariants — do not break these

1. **Postgres is the single source of truth.** helmd keeps only caches and a spool; the brain keeps nothing between wakes.
2. **WebSocket is a hint channel.** Work is pulled over HTTP and claimed atomically (`FOR UPDATE SKIP LOCKED`, epoch bump). Never put authoritative state only in a WS message.
3. **Everything is idempotent.** Events dedupe on `(run_id, seq)`; directives carry `contract_version` and stale ones are rejected; AMEND sends a full scope, never a delta.
4. **Control is enforced, not requested.** Scope and risky commands are decided by `internal/policy` before a tool runs. Prompts only explain. Never add a code path that lets an agent act without passing the policy engine.
5. **Fail closed.** If helmd is unreachable, the hook denies. If a directive cannot be parsed, reject it with an ack.
6. **Agent claims are not evidence.** `claim` events (e.g. "done") must be verified by sensors or acceptance scripts before a Run can be SUCCEEDED.
7. **Contracts are versioned and immutable.** Changing scope = new `contract` row + AMEND directive.
8. **Protocol changes start in `protocol/`.** Edit README + fixtures first, then Go types, then TS schemas; both test suites must pass.

## Conventions

- IDs: `<prefix>_<ulid>` — `mch_ gol_ tsk_ run_ dir_ apr_`.
- Go: standard library + `github.com/coder/websocket` only unless a task says otherwise; `log/slog` JSON logs; no cgo (helmd must cross-compile to linux/amd64, linux/arm64, darwin/arm64).
- TS: raw SQL through the `Sql` interface (no ORM); zod `strictObject` at every boundary; ESM; `noUncheckedIndexedAccess` is on.
- Tests: Go table tests; TS tests use PGlite via `test/pg.ts` (real Postgres semantics, no mocks of SQL).
- User-facing strings in the board are Chinese; code, comments and logs are English.
- Keep TODOs tagged with the task id, e.g. `TODO(M0-D3)`.

## Definition of done for a task

- Tests added for the new behaviour; `make test` passes.
- `docs/m0-tasks.md` updated (tick the task, note anything deferred).
- If you touched the protocol, DB schema, or an invariant above, say so explicitly in the PR description.
