// Data operations for the daemon-facing side of the control plane.
// Every function takes a minimal `Sql` executor so it runs on postgres.js in
// production and on PGlite in tests (see test/store.test.ts).
import { createHash, randomBytes } from "node:crypto";
import { ulid } from "ulid";
import type { Capabilities, Contract, Directive, Event, RunAssignment, RunState } from "./protocol.js";
import { TERMINAL_STATES } from "./protocol.js";

export interface Sql {
  query<T = Record<string, unknown>>(text: string, params?: unknown[]): Promise<T[]>;
  /** Runs a multi-statement script without parameters (migrations). */
  exec(script: string): Promise<void>;
  tx<T>(fn: (sql: Sql) => Promise<T>): Promise<T>;
}

export const newId = (prefix: string) => `${prefix}_${ulid()}`;
const sha256 = (s: string) => createHash("sha256").update(s).digest("hex");
const iso = (v: unknown) => (v instanceof Date ? v.toISOString() : new Date(String(v)).toISOString());

// ---------- machine registration & auth ----------

export async function createRegistrationCode(sql: Sql, ttlMinutes = 30): Promise<string> {
  const code = randomBytes(12).toString("base64url");
  await sql.query(
    `INSERT INTO registration_code (code_hash, expires_at) VALUES ($1, now() + make_interval(mins => $2))`,
    [sha256(code), ttlMinutes],
  );
  return code;
}

/** Exchanges a one-time code for a long-lived machine token. */
export async function registerMachine(sql: Sql, code: string, name: string): Promise<{ machineId: string; token: string }> {
  return sql.tx(async (tx) => {
    const rows = await tx.query(
      `UPDATE registration_code SET used_at = now()
        WHERE code_hash = $1 AND used_at IS NULL AND expires_at > now() RETURNING code_hash`,
      [sha256(code)],
    );
    if (rows.length === 0) throw new HttpError(401, "invalid or expired registration code");
    const machineId = newId("mch");
    const token = `hm_${randomBytes(24).toString("base64url")}`;
    await tx.query(
      `INSERT INTO machine (id, name, token_hash) VALUES ($1, $2, $3)
       ON CONFLICT (name) DO UPDATE SET token_hash = EXCLUDED.token_hash RETURNING id`,
      [machineId, name, sha256(token)],
    );
    const [m] = await tx.query<{ id: string }>(`SELECT id FROM machine WHERE name = $1`, [name]);
    await tx.query(`UPDATE registration_code SET machine_id = $1 WHERE code_hash = $2`, [m!.id, sha256(code)]);
    return { machineId: m!.id, token };
  });
}

export async function authMachine(sql: Sql, token: string): Promise<string | null> {
  const rows = await sql.query<{ id: string }>(`SELECT id FROM machine WHERE token_hash = $1`, [sha256(token)]);
  return rows[0]?.id ?? null;
}

export async function putCapabilities(sql: Sql, machineId: string, caps: Capabilities): Promise<void> {
  await sql.query(
    `UPDATE machine SET capabilities = $2,
       labels = ARRAY(SELECT DISTINCT unnest(labels || $3::text[])),
       status = 'online', last_seen_at = now()
     WHERE id = $1`,
    [machineId, JSON.stringify(caps), caps.labels],
  );
}

// ---------- dispatch & claim ----------

/** Creates a PENDING run for the task's current contract (called by the board or the brain). */
export async function dispatchRun(sql: Sql, taskId: string): Promise<string> {
  const [t] = await sql.query<{ executor: string; v: number | null }>(
    `SELECT executor, current_contract_version AS v FROM task WHERE id = $1`, [taskId]);
  if (!t) throw new HttpError(404, "task not found");
  if (t.v == null) throw new HttpError(409, "task has no contract yet");
  const id = newId("run");
  await sql.query(
    `INSERT INTO run (id, task_id, contract_version, executor) VALUES ($1, $2, $3, $4)`,
    [id, taskId, t.v, t.executor],
  );
  await sql.query(`UPDATE task SET status = 'running', updated_at = now() WHERE id = $1`, [taskId]);
  return id;
}

/**
 * Atomically assigns up to `slots` PENDING runs this machine can execute.
 * SKIP LOCKED makes concurrent claims from many daemons safe; epoch is the fencing token.
 */
export async function claimRuns(sql: Sql, machineId: string, slots: number, leaseSeconds = 120): Promise<RunAssignment[]> {
  if (slots <= 0) return [];
  return sql.tx(async (tx) => {
    const rows = await tx.query<{
      id: string; task_id: string; epoch: string | number; lease_until: Date | string;
      executor: string; body: Contract;
    }>(
      `WITH m AS (SELECT labels, capabilities FROM machine WHERE id = $1),
       picked AS (
         SELECT r.id FROM run r JOIN task t ON t.id = r.task_id, m
          WHERE r.state = 'PENDING'
            AND (t.target_machine_id IS NULL OR t.target_machine_id = $1)
            AND t.target_labels <@ m.labels
            AND (r.executor IN ('shell','job') OR EXISTS (
                  SELECT 1 FROM jsonb_array_elements(COALESCE(m.capabilities->'agents','[]'::jsonb)) a
                   WHERE a->>'name' = r.executor))
          ORDER BY r.created_at
          LIMIT $2
          FOR UPDATE OF r SKIP LOCKED)
       UPDATE run r SET machine_id = $1, epoch = r.epoch + 1, state = 'PREPARING',
              lease_until = now() + make_interval(secs => $3), started_at = now(), state_reason = 'claimed'
         FROM picked, contract c
        WHERE r.id = picked.id AND c.task_id = r.task_id AND c.version = r.contract_version
       RETURNING r.id, r.task_id, r.epoch, r.lease_until, r.executor, c.body`,
      [machineId, slots, leaseSeconds],
    );
    return rows.map((r) => ({
      run_id: r.id,
      task_id: r.task_id,
      epoch: Number(r.epoch),
      lease_until: iso(r.lease_until),
      executor: r.executor as RunAssignment["executor"],
      contract: r.body,
    }));
  });
}

// ---------- heartbeats, leases, orphans ----------

export async function heartbeat(sql: Sql, machineId: string, runningRuns: string[], payload: unknown, leaseSeconds = 120) {
  await sql.query(
    `UPDATE machine SET status = 'online', last_seen_at = now(), last_heartbeat = $2 WHERE id = $1`,
    [machineId, JSON.stringify(payload)],
  );
  await sql.query(
    `UPDATE run SET lease_until = now() + make_interval(secs => $3)
      WHERE machine_id = $1 AND id = ANY($2::text[]) AND state <> ALL($4::text[])`,
    [machineId, runningRuns, leaseSeconds, TERMINAL_STATES],
  );
}

/** Marks runs whose lease expired as ORPHANED and silent machines offline. Run every ~15s. */
export async function sweep(sql: Sql, offlineAfterSeconds = 60): Promise<string[]> {
  await sql.query(
    `UPDATE machine SET status = 'offline'
      WHERE status = 'online' AND last_seen_at < now() - make_interval(secs => $1)`, [offlineAfterSeconds]);
  const rows = await sql.query<{ id: string }>(
    `UPDATE run SET state = 'ORPHANED', state_reason = 'lease expired'
      WHERE lease_until < now() AND state NOT IN ('PENDING','ORPHANED','SUCCEEDED','FAILED','ABORTED')
      RETURNING id`);
  return rows.map((r) => r.id);
}

// ---------- events ----------

/**
 * Persists events idempotently. Only the run's current holder may write (machine fencing),
 * and a state_change only applies if it is newer than anything seen (seq ordering).
 */
export async function ingestEvents(sql: Sql, machineId: string, runId: string, events: Event[]): Promise<number> {
  return sql.tx(async (tx) => {
    const [run] = await tx.query<{ machine_id: string | null; last_seq: string | number }>(
      `SELECT machine_id, last_seq FROM run WHERE id = $1 FOR UPDATE`, [runId]);
    if (!run) throw new HttpError(404, "run not found");
    if (run.machine_id !== machineId) throw new HttpError(409, "run is not held by this machine");
    let lastSeq = Number(run.last_seq);
    let inserted = 0;
    for (const e of [...events].sort((a, b) => a.seq - b.seq)) {
      const r = await tx.query(
        `INSERT INTO run_event (run_id, seq, ts, type, data) VALUES ($1, $2, $3, $4, $5)
         ON CONFLICT (run_id, seq) DO NOTHING RETURNING seq`,
        [runId, e.seq, e.ts, e.type, JSON.stringify(e.data)],
      );
      if (r.length === 0) continue;
      inserted++;
      if (e.type === "state_change" && e.seq > lastSeq) {
        const to = e.data.to as RunState;
        await tx.query(
          `UPDATE run SET state = $2, state_reason = $3,
                  ended_at = CASE WHEN $2 = ANY($4::text[]) THEN now() ELSE ended_at END
            WHERE id = $1`,
          [runId, to, String(e.data.reason ?? ""), TERMINAL_STATES],
        );
      }
      if (e.type === "evidence") {
        await tx.query(
          `INSERT INTO evidence (run_id, uri, kind, bytes) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
          [runId, e.data.uri, e.data.kind, e.data.bytes ?? 0]);
      }
      lastSeq = Math.max(lastSeq, e.seq);
    }
    await tx.query(`UPDATE run SET last_seq = $2 WHERE id = $1`, [runId, lastSeq]);
    return inserted;
  });
}

// ---------- directives ----------

export async function issueDirective(
  sql: Sql,
  d: { runId: string; kind: Directive["kind"]; args?: Record<string, unknown>; issuedBy: "brain" | "human" | "jev" },
): Promise<string> {
  const [run] = await sql.query<{ machine_id: string | null; contract_version: number }>(
    `SELECT machine_id, contract_version FROM run WHERE id = $1`, [d.runId]);
  if (!run?.machine_id) throw new HttpError(409, "run is not on a machine yet");
  const id = newId("dir");
  await sql.query(
    `INSERT INTO directive (id, run_id, machine_id, kind, contract_version, args, issued_by)
     VALUES ($1, $2, $3, $4, $5, $6, $7)`,
    [id, d.runId, run.machine_id, d.kind, run.contract_version, JSON.stringify(d.args ?? {}), d.issuedBy],
  );
  return id;
}

export async function pendingDirectives(sql: Sql, machineId: string, after: string): Promise<Directive[]> {
  const rows = await sql.query<{
    id: string; run_id: string; kind: Directive["kind"]; contract_version: number;
    args: Record<string, unknown>; created_at: Date | string;
  }>(
    `SELECT id, run_id, kind, contract_version, args, created_at FROM directive
      WHERE machine_id = $1 AND status = 'queued' AND id > $2 ORDER BY id LIMIT 100`,
    [machineId, after],
  );
  return rows.map((r) => ({ ...r, created_at: iso(r.created_at) }));
}

export async function ackDirective(sql: Sql, machineId: string, id: string, status: string, detail?: string) {
  const rows = await sql.query(
    `UPDATE directive SET status = $3, ack_detail = $4, acked_at = now()
      WHERE id = $1 AND machine_id = $2 AND status = 'queued' RETURNING id`,
    [id, machineId, status, detail ?? null],
  );
  return rows.length > 0;
}

export class HttpError extends Error {
  constructor(public status: 400 | 401 | 404 | 409, message: string) {
    super(message);
  }
}
