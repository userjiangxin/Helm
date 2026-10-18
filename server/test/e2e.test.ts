// End to end: real helmd binary <-> real server (on PGlite). Skipped when Go is unavailable.
import { execFile, execFileSync, spawn, type ChildProcess } from "node:child_process";
import { promisify } from "node:util";
import { mkdtempSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, beforeAll, expect, it } from "vitest";
import { startServer } from "../src/server.js";
import * as S from "../src/store.js";
import { testDb } from "./pg.js";

const hasGo = (() => { try { execFileSync("go", ["version"]); return true; } catch { return false; } })();
const tmp = mkdtempSync(join(tmpdir(), "helm-e2e-"));
const bin = join(tmp, "helmd");
let srv: Awaited<ReturnType<typeof startServer>>;
let sql: S.Sql;
let helmd: ChildProcess | undefined;
let logs = "";

const waitFor = async <T>(fn: () => Promise<T | undefined | false>, ms = 15_000): Promise<T> => {
  const end = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() > end) throw new Error("timeout; helmd logs:\n" + logs);
    await new Promise((r) => setTimeout(r, 100));
  }
};

beforeAll(async () => {
  if (!hasGo) return;
  execFileSync("go", ["build", "-o", bin, "./cmd/helmd"], { cwd: join(import.meta.dirname, "../../daemon") });
  const t = await testDb();
  sql = t.sql;
  srv = await startServer({ sql, listen: (cb) => t.db.listen("helm", cb), port: 0, sweepMs: 500 });
}, 120_000);

afterAll(() => {
  helmd?.kill("SIGTERM");
  srv?.close();
});

it.skipIf(!hasGo)("helmd registers, heartbeats, wakes on dispatch, claims and reports state", async () => {
  // Real registration flow: one-time code -> `helmd register` -> credentials file -> `helmd run`.
  const env = { ...process.env, HELM_HOME: join(tmp, "home"), HELM_TOKEN: "" };
  const code = await S.createRegistrationCode(sql);
  // async: the server under test lives in this same Node process
  await promisify(execFile)(bin, ["register", "--server", `http://127.0.0.1:${srv.port}`, "--code", code, "--name", "e2e-box"], { env });
  const [{ id: machineId }] = await sql.query<{ id: string }>(`SELECT id FROM machine WHERE name = 'e2e-box'`) as [{ id: string }];
  helmd = spawn(bin, ["run", "--max-runs", "2", "--labels", "e2e"], { env });
  helmd.stderr?.on("data", (d) => (logs += d));

  // 1. capabilities + heartbeat reach the database
  await waitFor(async () => (await sql.query(`SELECT 1 FROM machine WHERE id=$1 AND last_heartbeat IS NOT NULL AND 'e2e' = ANY(labels)`, [machineId]))[0]);

  // Pretend the box has codex so the claim filter matches (the codex executor itself is still a stub in M0).
  await sql.query(`UPDATE machine SET capabilities = jsonb_set(capabilities, '{agents}', '[{"name":"codex","path":"/x","version":"1"}]') WHERE id=$1`, [machineId]);

  // 2. dispatch -> NOTIFY -> WebSocket wake -> HTTP claim -> state events back
  const gid = S.newId("gol"), tid = S.newId("tsk");
  await sql.query(`INSERT INTO goal (id, title) VALUES ($1,'g')`, [gid]);
  await sql.query(`INSERT INTO task (id, goal_id, title, executor, current_contract_version) VALUES ($1,$2,'t','codex',1)`, [tid, gid]);
  await sql.query(`INSERT INTO contract (task_id, version, body, author) VALUES ($1,1,$2,'human')`,
    [tid, JSON.stringify({ version: 1, intent: "i", prompt: "p", scope: { write_paths: [] }, budgets: {} })]);
  const t0 = Date.now();
  const runId = await S.dispatchRun(sql, tid);

  const run = await waitFor(async () => (await sql.query<{ state: string; machine_id: string; state_reason: string }>(
    `SELECT state, machine_id, state_reason FROM run WHERE id=$1 AND state='FAILED'`, [runId]))[0]);
  expect(run.machine_id).toBe(machineId);
  // No codex binary on the test box: the executor fails to start and the failure reaches the board.
  expect(run.state_reason).toMatch(/codex/);
  expect(Date.now() - t0).toBeLessThan(5_000); // woken by push, not by the 30s poll

  const events = await sql.query<{ type: string; data: { to: string } }>(`SELECT type, data FROM run_event WHERE run_id=$1 ORDER BY seq`, [runId]);
  expect(events.map((e) => e.data.to)).toEqual(["PREPARING", "FAILED"]);

  // 3. events_ack: once persisted, the daemon's on-disk spool is emptied.
  await waitFor(async () => readdirSync(join(tmp, "home", "spool")).length === 0);
}, 60_000);
