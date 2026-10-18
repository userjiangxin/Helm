import { expect, it } from "vitest";
import * as S from "../src/store.js";
import { testDb } from "./pg.js";

async function gatedRun() {
  const { sql } = await testDb();
  const { machineId } = await S.registerMachine(sql, await S.createRegistrationCode(sql), "box");
  await S.putCapabilities(sql, machineId, { machine_name: "box", daemon_version: "x", os: "linux", arch: "amd64", labels: [],
    agents: [{ name: "claude", path: "/c", version: "1" }], gpus: [], max_concurrent_runs: 1 });
  const gid = S.newId("gol"), tid = S.newId("tsk");
  await sql.query(`INSERT INTO goal (id, title) VALUES ($1,'g')`, [gid]);
  await sql.query(`INSERT INTO task (id, goal_id, title, executor, current_contract_version) VALUES ($1,$2,'t','claude',1)`, [tid, gid]);
  await sql.query(`INSERT INTO contract (task_id, version, body, author) VALUES ($1,1,$2,'human')`,
    [tid, JSON.stringify({ version: 1, intent: "i", prompt: "p", scope: { write_paths: ["/data/a/**"] }, budgets: {} })]);
  const runId = await S.dispatchRun(sql, tid);
  await S.claimRuns(sql, machineId, 1);
  const ts = new Date().toISOString();
  await S.ingestEvents(sql, machineId, runId, [
    { seq: 1, ts, type: "tool_call", data: { tool: "Write", input_digest: "/shared/x/clean.py", decision: "hold", reason: "write outside contract scope: /shared/x/clean.py" } },
    { seq: 2, ts, type: "state_change", data: { from: "RUNNING", to: "GATED", reason: "write outside contract scope: /shared/x/clean.py" } },
  ]);
  const [a] = await sql.query<{ id: string; options: { id: string; path?: string }[] }>(`SELECT id, options FROM approval WHERE run_id = $1`, [runId]);
  return { sql, machineId, runId, tid, approval: a! };
}

it("a held write creates an approval offering the parent directory", async () => {
  const { approval } = await gatedRun();
  expect(approval.options).toEqual([
    { id: "allow_dir", label: "批准，并允许写入这个目录", path: "/shared/x/**" },
    { id: "reject", label: "不批准" },
  ]);
});

it("allow_dir → contract v2 with the directory, run bumped to v2, AMEND carries the full scope", async () => {
  const { sql, machineId, runId, tid, approval } = await gatedRun();
  await S.decideApproval(sql, approval.id, { option: "allow_dir", note: "团队要用" });
  const [c] = await sql.query<{ body: { version: number; scope: { write_paths: string[] } } }>(
    `SELECT body FROM contract WHERE task_id = $1 AND version = 2`, [tid]);
  expect(c!.body.scope.write_paths).toEqual(["/data/a/**", "/shared/x/**"]);
  const [d] = await S.pendingDirectives(sql, machineId, "");
  expect(d).toMatchObject({ run_id: runId, kind: "AMEND", contract_version: 2,
    args: { scope: { write_paths: ["/data/a/**", "/shared/x/**"] } } });
  expect(String(d!.args!.message)).toContain("团队要用");
  await expect(S.decideApproval(sql, approval.id, { option: "reject" })).rejects.toThrow(/already decided/);
});

it("reject → CONTINUE telling the agent not to retry; contract unchanged", async () => {
  const { sql, machineId, tid, approval } = await gatedRun();
  await S.decideApproval(sql, approval.id, { option: "reject" });
  const [d] = await S.pendingDirectives(sql, machineId, "");
  expect(d).toMatchObject({ kind: "CONTINUE", contract_version: 1 });
  expect(String(d!.args!.message)).toMatch(/NOT approved/);
  expect(await sql.query(`SELECT version FROM contract WHERE task_id = $1`, [tid])).toHaveLength(1);
});

it("revising the contract of a running task bumps the version and sends AMEND with what changed", async () => {
  const { sql, machineId, runId, tid } = await gatedRun();
  const r = await S.reviseContract(sql, tid, { prompt: "use label_v2", scope: { write_paths: ["/data/a/**", "/data/b/**"] } }, "字段改名");
  expect(r.version).toBe(2);
  const [d] = await S.pendingDirectives(sql, machineId, "");
  expect(d).toMatchObject({ run_id: runId, kind: "AMEND", contract_version: 2, args: { scope: { write_paths: ["/data/a/**", "/data/b/**"] } } });
  expect(String(d!.args!.message)).toContain("use label_v2");
  const [t] = await sql.query<{ current_contract_version: number }>(`SELECT current_contract_version FROM task WHERE id = $1`, [tid]);
  expect(t!.current_contract_version).toBe(2);
});
