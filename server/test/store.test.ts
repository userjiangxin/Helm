import { beforeEach, describe, expect, it } from "vitest";
import * as S from "../src/store.js";
import type { Capabilities, Contract } from "../src/protocol.js";
import { testDb } from "./pg.js";

const caps = (agents: string[], labels: string[] = []): Capabilities => ({
  machine_name: "m", daemon_version: "0.1.0", os: "linux", arch: "amd64", labels,
  agents: agents.map((name) => ({ name, path: `/usr/bin/${name}`, version: "1" })), gpus: [], max_concurrent_runs: 4,
});

const contract = (v = 1): Contract => ({
  version: v, intent: "prep data", prompt: "do it", scope: { write_paths: ["/data/x/**"] }, budgets: {},
});

let sql: S.Sql;

async function machine(name: string, agents: string[], labels: string[] = []) {
  const code = await S.createRegistrationCode(sql);
  const m = await S.registerMachine(sql, code, name);
  await S.putCapabilities(sql, m.machineId, caps(agents, labels));
  return m;
}

async function task(opts: { executor?: string; labels?: string[] } = {}) {
  const gid = S.newId("gol");
  const tid = S.newId("tsk");
  await sql.query(`INSERT INTO goal (id, title) VALUES ($1, 'g')`, [gid]);
  await sql.query(
    `INSERT INTO task (id, goal_id, title, executor, target_labels, current_contract_version) VALUES ($1,$2,'t',$3,$4,1)`,
    [tid, gid, opts.executor ?? "codex", opts.labels ?? []]);
  await sql.query(`INSERT INTO contract (task_id, version, body, author) VALUES ($1, 1, $2, 'human')`,
    [tid, JSON.stringify(contract())]);
  return tid;
}

beforeEach(async () => {
  ({ sql } = await testDb());
});

describe("registration", () => {
  it("codes are single use and tokens authenticate", async () => {
    const code = await S.createRegistrationCode(sql);
    const { machineId, token } = await S.registerMachine(sql, code, "gpu-01");
    expect(await S.authMachine(sql, token)).toBe(machineId);
    expect(await S.authMachine(sql, "hm_wrong")).toBeNull();
    await expect(S.registerMachine(sql, code, "gpu-02")).rejects.toThrow(/invalid or expired/);
  });
});

describe("claim", () => {
  it("assigns each run once and matches executor + labels", async () => {
    const laptop = await machine("laptop", ["claude"]);
    const gpu = await machine("gpu-01", ["codex"], ["gpu"]);
    const r1 = await S.dispatchRun(sql, await task({ executor: "codex", labels: ["gpu"] }));
    const r2 = await S.dispatchRun(sql, await task({ executor: "claude" }));

    expect(await S.claimRuns(sql, laptop.machineId, 4)).toMatchObject([{ run_id: r2, executor: "claude", epoch: 1 }]);
    const got = await S.claimRuns(sql, gpu.machineId, 4);
    expect(got).toHaveLength(1);
    expect(got[0]).toMatchObject({ run_id: r1, contract: { version: 1, intent: "prep data" } });
    expect(await S.claimRuns(sql, gpu.machineId, 4)).toEqual([]); // nothing left
  });

  it("respects slots", async () => {
    const m = await machine("m1", ["codex"]);
    for (let i = 0; i < 3; i++) await S.dispatchRun(sql, await task());
    expect(await S.claimRuns(sql, m.machineId, 2)).toHaveLength(2);
    expect(await S.claimRuns(sql, m.machineId, 2)).toHaveLength(1);
  });
});

describe("events", () => {
  it("are idempotent, fenced to the holder, and drive run state by seq", async () => {
    const m = await machine("m1", ["codex"]);
    const other = await machine("m2", ["codex"]);
    const runId = await S.dispatchRun(sql, await task());
    await S.claimRuns(sql, m.machineId, 1);
    const ev = (seq: number, to: string) => ({ seq, ts: new Date().toISOString(), type: "state_change" as const, data: { from: "X", to } });

    expect(await S.ingestEvents(sql, m.machineId, runId, [ev(1, "RUNNING"), ev(2, "GATED")])).toBe(2);
    expect(await S.ingestEvents(sql, m.machineId, runId, [ev(2, "GATED")])).toBe(0); // resend is harmless
    await expect(S.ingestEvents(sql, other.machineId, runId, [ev(3, "ABORTED")])).rejects.toThrow(/not held/);

    const [run] = await sql.query<{ state: string; last_seq: number }>(`SELECT state, last_seq FROM run WHERE id=$1`, [runId]);
    expect(run).toMatchObject({ state: "GATED", last_seq: 2 });
  });
});

describe("directives", () => {
  it("are pulled in order after a cursor and acked once", async () => {
    const m = await machine("m1", ["codex"]);
    const runId = await S.dispatchRun(sql, await task());
    await S.claimRuns(sql, m.machineId, 1);
    const d1 = await S.issueDirective(sql, { runId, kind: "PAUSE", issuedBy: "human" });
    const d2 = await S.issueDirective(sql, { runId, kind: "STEER", args: { message: "use label_v2" }, issuedBy: "brain" });

    const all = await S.pendingDirectives(sql, m.machineId, "");
    expect(all.map((d) => d.id)).toEqual([d1, d2]);
    expect(all[1]).toMatchObject({ kind: "STEER", contract_version: 1, args: { message: "use label_v2" } });
    expect((await S.pendingDirectives(sql, m.machineId, d1)).map((d) => d.id)).toEqual([d2]);

    expect(await S.ackDirective(sql, m.machineId, d1, "applied")).toBe(true);
    expect(await S.ackDirective(sql, m.machineId, d1, "applied")).toBe(false);
    expect((await S.pendingDirectives(sql, m.machineId, "")).map((d) => d.id)).toEqual([d2]);
  });
});

describe("leases", () => {
  it("expired leases orphan the run; heartbeats keep it alive", async () => {
    const m = await machine("m1", ["codex"]);
    const keep = await S.dispatchRun(sql, await task());
    const lose = await S.dispatchRun(sql, await task());
    await S.claimRuns(sql, m.machineId, 2, 0); // lease already expired
    await S.heartbeat(sql, m.machineId, [keep], { running_runs: [keep], load: {} });
    expect(await S.sweep(sql)).toEqual([lose]);
  });
});
