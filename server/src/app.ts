// HTTP + WebSocket surface of the control plane.
//   /daemon/*  — called by helmd (machine token)
//   /api/*     — called by the board (M0: single user on the tailnet, no auth; TODO(M0-S5) session auth)
import { Hono, type Context } from "hono";
import { streamSSE } from "hono/streaming";
import type { UpgradeWebSocket, WSContext } from "hono/ws";
import { z } from "zod";
import * as P from "./protocol.js";
import * as S from "./store.js";
import { HttpError, newId, type Sql } from "./store.js";

type Env = { Variables: { machineId: string } };

/** Fan-out of Postgres NOTIFY payloads to WebSockets (daemons) and SSE (board). */
export class Hub {
  private daemons = new Map<string, WSContext>();
  private boards = new Set<(msg: string) => void>();

  attachDaemon(machineId: string, ws: WSContext) {
    this.daemons.get(machineId)?.close(4000, "replaced by newer connection");
    this.daemons.set(machineId, ws);
  }
  detachDaemon(machineId: string, ws: WSContext) {
    if (this.daemons.get(machineId) === ws) this.daemons.delete(machineId);
  }
  wake(machineId: string | null, reason: P.Envelope["payload"] & { reason: string }) {
    const msg = JSON.stringify(P.envelope("wake", reason, newId("msg")));
    const targets = machineId ? [this.daemons.get(machineId)] : [...this.daemons.values()];
    for (const ws of targets) ws?.send(msg);
  }
  subscribeBoard(fn: (msg: string) => void) {
    this.boards.add(fn);
    return () => this.boards.delete(fn);
  }
  /** Called with every `helm` NOTIFY payload. */
  onNotify(raw: string) {
    for (const fn of this.boards) fn(raw);
    const n = JSON.parse(raw) as { table: string; op: string; machine_id?: string | null };
    if (n.table === "run" && n.op === "INSERT") this.wake(null, { reason: "run_queued" });
    if (n.table === "directive") this.wake(n.machine_id ?? null, { reason: "directive_queued" });
  }
  get onlineMachines() {
    return [...this.daemons.keys()];
  }
}

export function buildApp(sql: Sql, hub: Hub, upgradeWebSocket: UpgradeWebSocket) {
  const app = new Hono<Env>();

  app.onError((err, c) => {
    if (err instanceof HttpError) return c.json({ error: err.message }, err.status);
    if (err instanceof z.ZodError) return c.json({ error: "invalid body", issues: err.issues }, 400);
    console.error(err);
    return c.json({ error: "internal error" }, 500);
  });

  app.get("/healthz", (c) => c.json({ ok: true, daemons: hub.onlineMachines.length }));

  // ---------------- daemon side ----------------

  app.post("/daemon/register", async (c) => {
    const body = z.object({ code: z.string(), name: z.string().min(1) }).parse(await c.req.json());
    return c.json(await S.registerMachine(sql, body.code, body.name));
  });

  const daemon = new Hono<Env>();
  daemon.use("*", async (c, next) => {
    const token = c.req.header("Authorization")?.replace(/^Bearer /, "");
    const machineId = token ? await S.authMachine(sql, token) : null;
    if (!machineId) return c.json({ error: "unauthorized" }, 401);
    c.set("machineId", machineId);
    await next();
  });

  daemon.put("/capabilities", async (c) => {
    await S.putCapabilities(sql, c.var.machineId, P.Capabilities.parse(await c.req.json()));
    return c.body(null, 204);
  });

  daemon.post("/claim", async (c) => {
    const { slots } = z.object({ slots: z.number().int().min(0).max(64) }).parse(await c.req.json());
    return c.json({ runs: await S.claimRuns(sql, c.var.machineId, slots) });
  });

  daemon.get("/directives", async (c) =>
    c.json({ directives: await S.pendingDirectives(sql, c.var.machineId, c.req.query("after") ?? "") }));

  daemon.post("/directives/:id/ack", async (c) => {
    const ack = P.DirectiveAck.parse(await c.req.json());
    if (ack.directive_id !== c.req.param("id")) throw new HttpError(400, "id mismatch");
    await S.ackDirective(sql, c.var.machineId, ack.directive_id, ack.status, ack.detail);
    return c.body(null, 204);
  });

  daemon.post("/events", async (c) => {
    const b = P.RunEvents.parse(await c.req.json());
    return c.json({ inserted: await S.ingestEvents(sql, c.var.machineId, b.run_id, b.events) });
  });

  daemon.get(
    "/ws",
    upgradeWebSocket((c: Context<Env>) => {
      const machineId = c.var.machineId;
      return {
        onOpen: (_e, ws) => hub.attachDaemon(machineId, ws),
        onClose: (_e, ws) => hub.detachDaemon(machineId, ws),
        onMessage: async (e, ws) => {
          try {
            const env = P.Envelope.parse(JSON.parse(String(e.data)));
            if (env.type === "heartbeat") {
              const hb = P.Heartbeat.parse(env.payload);
              await S.heartbeat(sql, machineId, hb.running_runs, hb);
            } else if (env.type === "run_events") {
              const b = P.RunEvents.parse(env.payload);
              await S.ingestEvents(sql, machineId, b.run_id, b.events);
            }
          } catch (err) {
            // Never drop the socket for one bad message; the daemon resends from its spool.
            console.warn("daemon ws message rejected", machineId, err instanceof Error ? err.message : err);
            void ws;
          }
        },
      };
    }),
  );

  app.route("/daemon", daemon);

  // ---------------- board side (M0 minimum) ----------------

  const api = new Hono();

  api.get("/machines", async (c) =>
    c.json(await sql.query(`SELECT id, name, labels, status, last_seen_at, capabilities, last_heartbeat FROM machine ORDER BY name`)));

  api.post("/machines/registration-codes", async (c) => c.json({ code: await S.createRegistrationCode(sql) }));

  api.get("/goals", async (c) => c.json(await sql.query(`SELECT * FROM goal WHERE status <> 'archived' ORDER BY created_at DESC`)));

  api.post("/goals", async (c) => {
    const b = z.object({ title: z.string().min(1), intent: z.string().default("") }).parse(await c.req.json());
    const id = newId("gol");
    await sql.query(`INSERT INTO goal (id, title, intent) VALUES ($1, $2, $3)`, [id, b.title, b.intent]);
    return c.json({ id }, 201);
  });

  // Board query: tasks of a goal with their latest run, ordered for columns.
  api.get("/goals/:id/tasks", async (c) =>
    c.json(await sql.query(
      `SELECT t.*, r.id AS run_id, r.state AS run_state, r.machine_id, r.state_reason
         FROM task t
         LEFT JOIN LATERAL (SELECT * FROM run WHERE task_id = t.id ORDER BY created_at DESC LIMIT 1) r ON true
        WHERE t.goal_id = $1 ORDER BY t.status, t.position`, [c.req.param("id")])));

  // M0: the human acts as the brain — create a task together with its first contract.
  api.post("/goals/:id/tasks", async (c) => {
    const b = z.object({
      title: z.string().min(1),
      parent_id: z.string().optional(),
      executor: P.Executor.default("codex"),
      target_machine_id: z.string().optional(),
      target_labels: z.array(z.string()).default([]),
      contract: P.Contract.omit({ version: true }),
    }).parse(await c.req.json());
    const id = newId("tsk");
    await sql.tx(async (tx) => {
      await tx.query(
        `INSERT INTO task (id, goal_id, parent_id, title, executor, target_machine_id, target_labels, current_contract_version, status)
         VALUES ($1,$2,$3,$4,$5,$6,$7,1,'awaiting_approval')`,
        [id, c.req.param("id"), b.parent_id ?? null, b.title, b.executor, b.target_machine_id ?? null, b.target_labels]);
      await tx.query(`INSERT INTO contract (task_id, version, body, author) VALUES ($1, 1, $2, 'human')`,
        [id, JSON.stringify({ ...b.contract, version: 1 })]);
    });
    return c.json({ id }, 201);
  });

  api.get("/tasks/:id", async (c) => {
    const id = c.req.param("id");
    const [task] = await sql.query(`SELECT * FROM task WHERE id = $1`, [id]);
    if (!task) throw new HttpError(404, "task not found");
    const [contracts, runs, children] = await Promise.all([
      sql.query(`SELECT version, body, author, reason, created_at FROM contract WHERE task_id = $1 ORDER BY version DESC`, [id]),
      sql.query(`SELECT r.*, m.name AS machine_name FROM run r LEFT JOIN machine m ON m.id = r.machine_id
                  WHERE r.task_id = $1 ORDER BY r.created_at DESC`, [id]),
      sql.query(`SELECT id, title, status FROM task WHERE parent_id = $1 ORDER BY position`, [id]),
    ]);
    return c.json({ task, contracts, runs, children });
  });

  // Drag on the board: change column and/or position (fractional index computed by the client).
  api.patch("/tasks/:id", async (c) => {
    const b = z.object({
      status: z.enum(["inbox", "planning", "awaiting_approval", "running", "verifying", "done", "blocked", "cancelled"]).optional(),
      position: z.number().optional(),
      title: z.string().min(1).optional(),
    }).parse(await c.req.json());
    const rows = await sql.query(
      `UPDATE task SET status = COALESCE($2, status), position = COALESCE($3, position),
              title = COALESCE($4, title), updated_at = now()
        WHERE id = $1 RETURNING id`, [c.req.param("id"), b.status ?? null, b.position ?? null, b.title ?? null]);
    if (rows.length === 0) throw new HttpError(404, "task not found");
    return c.body(null, 204);
  });

  // Runs that need the human, across all goals (the board's top strip).
  api.get("/attention", async (c) =>
    c.json(await sql.query(
      `SELECT r.id AS run_id, r.state, r.state_reason, r.task_id, t.title, t.goal_id, m.name AS machine_name, r.created_at
         FROM run r JOIN task t ON t.id = r.task_id LEFT JOIN machine m ON m.id = r.machine_id
        WHERE r.state IN ('GATED','VERIFYING','ORPHANED') ORDER BY r.created_at`)));

  api.post("/tasks/:id/dispatch", async (c) => c.json({ run_id: await S.dispatchRun(sql, c.req.param("id")) }, 201));

  api.get("/runs/:id/events", async (c) =>
    c.json(await sql.query(
      `SELECT seq, ts, type, data FROM run_event WHERE run_id = $1 AND seq > $2 ORDER BY seq LIMIT 500`,
      [c.req.param("id"), Number(c.req.query("after") ?? 0)])));

  api.post("/runs/:id/directives", async (c) => {
    const b = z.object({ kind: P.DirectiveKind, args: z.record(z.string(), z.unknown()).optional() }).parse(await c.req.json());
    return c.json({ id: await S.issueDirective(sql, { runId: c.req.param("id"), kind: b.kind, args: b.args, issuedBy: "human" }) }, 201);
  });

  api.get("/approvals", async (c) => {
    const runId = c.req.query("run");
    return c.json(await sql.query(
      `SELECT a.*, t.title AS task_title FROM approval a LEFT JOIN task t ON t.id = a.task_id
        WHERE a.decided_at IS NULL AND ($1::text IS NULL OR a.run_id = $1) ORDER BY a.created_at`, [runId ?? null]));
  });

  api.post("/approvals/:id/decide", async (c) => {
    const b = z.object({ option: z.enum(["allow_dir", "reject"]), note: z.string().optional() }).parse(await c.req.json());
    return c.json(await S.decideApproval(sql, c.req.param("id"), b));
  });

  // Live updates for the board: one SSE stream carrying every NOTIFY payload.
  api.get("/stream", (c) =>
    streamSSE(c, async (stream) => {
      const unsubscribe = hub.subscribeBoard((msg) => void stream.writeSSE({ event: "change", data: msg }));
      stream.onAbort(() => void unsubscribe());
      while (!stream.aborted) await stream.sleep(25_000).then(() => stream.writeSSE({ event: "ping", data: "" }));
    }));

  app.route("/api", api);
  return app;
}
