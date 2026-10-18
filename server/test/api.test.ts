import { describe, expect, it } from "vitest";
import { buildApp, Hub } from "../src/app.js";
import * as S from "../src/store.js";
import { testDb } from "./pg.js";

const noWs = (() => () => new Response(null)) as never;

describe("board api", () => {
  it("create goal → task → dispatch → run events move the card; attention lists gated runs", async () => {
    const { sql } = await testDb();
    const app = buildApp(sql, new Hub(), noWs);
    const j = (method: string, path: string, body?: unknown) =>
      app.request(path, { method, headers: { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });

    const { id: goalId } = await (await j("POST", "/api/goals", { title: "ranker" })).json() as { id: string };
    const res = await j("POST", `/api/goals/${goalId}/tasks`, {
      title: "prep data", executor: "claude",
      contract: { intent: "prep", prompt: "do it", workdir: "/w", scope: { write_paths: [] }, budgets: {} },
    });
    expect(res.status).toBe(201);
    const { id: taskId } = await res.json() as { id: string };

    expect((await j("PATCH", `/api/tasks/${taskId}`, { status: "inbox", position: 3 })).status).toBe(204);
    const { run_id } = await (await j("POST", `/api/tasks/${taskId}/dispatch`)).json() as { run_id: string };

    const { machineId } = await S.registerMachine(sql, await S.createRegistrationCode(sql), "box");
    await S.putCapabilities(sql, machineId, { machine_name: "box", daemon_version: "x", os: "linux", arch: "amd64", labels: [],
      agents: [{ name: "claude", path: "/c", version: "1" }], gpus: [], max_concurrent_runs: 1 });
    await S.claimRuns(sql, machineId, 1);
    const ev = (seq: number, to: string, reason = "") => ({ seq, ts: new Date().toISOString(), type: "state_change" as const, data: { from: "", to, reason } });
    await S.ingestEvents(sql, machineId, run_id, [ev(1, "RUNNING"), ev(2, "GATED", "write outside scope")]);

    const attention = await (await j("GET", "/api/attention")).json() as { run_id: string; state: string }[];
    expect(attention).toMatchObject([{ run_id, state: "GATED" }]);

    await S.ingestEvents(sql, machineId, run_id, [ev(3, "VERIFYING")]);
    const detail = await (await j("GET", `/api/tasks/${taskId}`)).json() as { task: { status: string }; runs: { machine_name: string }[]; contracts: unknown[] };
    expect(detail.task.status).toBe("verifying");
    expect(detail.runs[0]?.machine_name).toBe("box");
    expect(detail.contracts).toHaveLength(1);

    const d = await j("POST", `/api/runs/${run_id}/directives`, { kind: "COMPLETE" });
    expect(d.status).toBe(201);
    expect((await j("POST", `/api/runs/${run_id}/directives`, { kind: "NOPE" })).status).toBe(400);
  });
});
