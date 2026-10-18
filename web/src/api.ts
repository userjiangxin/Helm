// Board-side API client. Shapes mirror server/src/app.ts (/api/*).
export type RunState =
  | "PENDING" | "PREPARING" | "RUNNING" | "GATED" | "SUSPENDED" | "VERIFYING"
  | "SUCCEEDED" | "FAILED" | "ABORTED" | "ORPHANED";
export type TaskStatus = "inbox" | "planning" | "awaiting_approval" | "running" | "verifying" | "done" | "blocked" | "cancelled";
export type Executor = "codex" | "claude" | "shell" | "job";
export type DirectiveKind = "CONTINUE" | "STEER" | "AMEND" | "PAUSE" | "ROLLBACK" | "ABORT" | "COMPLETE";

export interface Goal { id: string; title: string; intent: string; status: string; created_at: string }
export interface BoardTask {
  id: string; goal_id: string; parent_id: string | null; title: string; status: TaskStatus; position: number;
  executor: Executor; target_labels: string[]; current_contract_version: number | null;
  run_id: string | null; run_state: RunState | null; machine_id: string | null; state_reason: string | null;
}
export interface Contract {
  version: number; intent: string; prompt: string; workdir?: string; repo?: string;
  scope: { write_paths: string[]; readonly_paths?: string[]; deny_commands?: string[] };
  acceptance?: string[]; budgets: Record<string, number>; skills?: string[];
}
export interface Run {
  id: string; state: RunState; state_reason: string; machine_name: string | null; executor: Executor;
  contract_version: number; created_at: string; started_at: string | null; ended_at: string | null;
}
export interface TaskDetail {
  task: BoardTask;
  contracts: { version: number; body: Contract; author: string; reason: string; created_at: string }[];
  runs: Run[];
  children: { id: string; title: string; status: TaskStatus }[];
}
export interface RunEvent { seq: number; ts: string; type: string; data: Record<string, unknown> }
export interface Attention { run_id: string; state: RunState; state_reason: string; task_id: string; title: string; goal_id: string; machine_name: string | null }
export interface Machine {
  id: string; name: string; labels: string[]; status: "online" | "offline" | "draining"; last_seen_at: string | null;
  capabilities: { agents?: { name: string; version: string }[]; gpus?: { index: number; name: string; mem_total_mb: number }[]; os?: string; arch?: string };
  last_heartbeat: { running_runs: string[]; load: { cpu_pct?: number; gpus?: { index: number; util_pct: number; mem_used_mb: number; mem_total_mb: number }[] } } | null;
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api${path}`, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    const msg = await res.json().then((j: { error?: string }) => j.error).catch(() => res.statusText);
    throw new Error(msg || `${res.status}`);
  }
  return res.status === 204 ? (undefined as T) : res.json();
}

export const api = {
  goals: () => call<Goal[]>("GET", "/goals"),
  createGoal: (title: string, intent = "") => call<{ id: string }>("POST", "/goals", { title, intent }),
  tasks: (goalId: string) => call<BoardTask[]>("GET", `/goals/${goalId}/tasks`),
  createTask: (goalId: string, body: {
    title: string; executor: Executor; target_labels: string[]; parent_id?: string;
    contract: Omit<Contract, "version">;
  }) => call<{ id: string }>("POST", `/goals/${goalId}/tasks`, body),
  task: (id: string) => call<TaskDetail>("GET", `/tasks/${id}`),
  patchTask: (id: string, body: Partial<Pick<BoardTask, "status" | "position" | "title">>) => call<void>("PATCH", `/tasks/${id}`, body),
  dispatch: (taskId: string) => call<{ run_id: string }>("POST", `/tasks/${taskId}/dispatch`),
  events: (runId: string) => call<RunEvent[]>("GET", `/runs/${runId}/events`),
  directive: (runId: string, kind: DirectiveKind, args?: Record<string, unknown>) =>
    call<{ id: string }>("POST", `/runs/${runId}/directives`, { kind, args }),
  attention: () => call<Attention[]>("GET", "/attention"),
  machines: () => call<Machine[]>("GET", "/machines"),
  registrationCode: () => call<{ code: string }>("POST", "/machines/registration-codes"),
};

export const RUN_STATE_LABEL: Record<RunState, string> = {
  PENDING: "排队中", PREPARING: "准备中", RUNNING: "执行中", GATED: "等你拍板", SUSPENDED: "已暂停",
  VERIFYING: "待验收", SUCCEEDED: "已完成", FAILED: "失败", ABORTED: "已终止", ORPHANED: "失联",
};

export const COLUMNS: { status: TaskStatus; label: string }[] = [
  { status: "inbox", label: "收件箱" },
  { status: "awaiting_approval", label: "待派发" },
  { status: "running", label: "执行中" },
  { status: "verifying", label: "待验收" },
  { status: "blocked", label: "受阻" },
  { status: "done", label: "已完成" },
];
