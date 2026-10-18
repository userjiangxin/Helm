// Helm wire protocol v1 — TypeScript side. Mirrors daemon/internal/wire/wire.go.
// Source of truth: protocol/README.md + protocol/fixtures/*.json (see test/protocol.test.ts).
import { z } from "zod";

export const RunState = z.enum([
  "PENDING", "PREPARING", "RUNNING", "GATED", "SUSPENDED", "VERIFYING",
  "SUCCEEDED", "FAILED", "ABORTED", "ORPHANED",
]);
export type RunState = z.infer<typeof RunState>;
export const TERMINAL_STATES: RunState[] = ["SUCCEEDED", "FAILED", "ABORTED"];

const ts = z.iso.datetime({ offset: true });

export const Scope = z.strictObject({
  write_paths: z.array(z.string()),
  deny_commands: z.array(z.string()).optional(),
  readonly_paths: z.array(z.string()).optional(),
});

export const Contract = z.strictObject({
  version: z.number().int().positive(),
  intent: z.string(),
  prompt: z.string(),
  repo: z.string().optional(),
  base_ref: z.string().optional(),
  workdir: z.string().optional(),
  scope: Scope,
  checkpoints: z.array(z.string()).optional(),
  acceptance: z.array(z.string()).optional(),
  budgets: z.strictObject({
    tokens: z.number().int().optional(),
    wall_seconds: z.number().int().optional(),
    gpu_hours: z.number().int().optional(),
  }),
  skills: z.array(z.string()).optional(),
});
export type Contract = z.infer<typeof Contract>;

export const Executor = z.enum(["codex", "claude", "shell", "job"]);

export const RunAssignment = z.strictObject({
  run_id: z.string(),
  task_id: z.string(),
  epoch: z.number().int(),
  lease_until: ts,
  executor: Executor,
  contract: Contract,
});
export type RunAssignment = z.infer<typeof RunAssignment>;

export const EventType = z.enum([
  "state_change", "agent_message", "tool_call", "cmd_exit", "fs_change", "error", "evidence", "claim",
]);

export const Event = z.strictObject({
  seq: z.number().int().positive(),
  ts,
  type: EventType,
  data: z.record(z.string(), z.unknown()),
});
export type Event = z.infer<typeof Event>;

export const RunEvents = z.strictObject({ run_id: z.string(), events: z.array(Event).min(1) });

export const Wake = z.strictObject({ reason: z.enum(["run_queued", "directive_queued", "skills_updated"]) });

export const Heartbeat = z.strictObject({
  running_runs: z.array(z.string()),
  load: z.strictObject({
    cpu_pct: z.number().optional(),
    gpus: z.array(z.strictObject({
      index: z.number().int(), util_pct: z.number(), mem_used_mb: z.number(), mem_total_mb: z.number(),
    })).optional(),
  }),
});

export const EventsAck = z.strictObject({ run_id: z.string(), last_seq: z.number().int().nonnegative() });

export const MsgType = z.enum(["wake", "heartbeat", "run_events", "events_ack"]);
export const Envelope = z.strictObject({
  v: z.literal(1),
  type: MsgType,
  id: z.string(),
  ts,
  payload: z.unknown(),
});
export type Envelope = z.infer<typeof Envelope>;

export const DirectiveKind = z.enum(["CONTINUE", "STEER", "AMEND", "PAUSE", "ROLLBACK", "ABORT", "COMPLETE"]);
export const Directive = z.strictObject({
  id: z.string(),
  run_id: z.string(),
  kind: DirectiveKind,
  contract_version: z.number().int(),
  args: z.record(z.string(), z.unknown()).optional(),
  created_at: ts,
});
export type Directive = z.infer<typeof Directive>;

export const AckStatus = z.enum(["applied", "rejected_stale", "rejected_invalid", "failed"]);
export const DirectiveAck = z.strictObject({
  directive_id: z.string(),
  status: AckStatus,
  detail: z.string().optional(),
  acked_at: ts,
});

export const Capabilities = z.strictObject({
  machine_name: z.string(),
  daemon_version: z.string(),
  os: z.string(),
  arch: z.string(),
  labels: z.array(z.string()).nullable().transform((v) => v ?? []),
  agents: z.array(z.strictObject({ name: z.string(), path: z.string(), version: z.string() })),
  gpus: z.array(z.strictObject({ index: z.number().int(), name: z.string(), mem_total_mb: z.number() })).nullable().transform((v) => v ?? []),
  max_concurrent_runs: z.number().int().positive(),
});
export type Capabilities = z.infer<typeof Capabilities>;

export function envelope(type: z.infer<typeof MsgType>, payload: unknown, id: string): Envelope {
  return { v: 1, type, id, ts: new Date().toISOString(), payload };
}
