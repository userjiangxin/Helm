import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { api, type DirectiveKind, type Run, type RunEvent, type RunState } from "../api";
import { reasonText } from "../components/reason";
import { ago, clock, State } from "../components/State";

const ACTIVE: RunState[] = ["PENDING", "PREPARING", "RUNNING", "GATED", "SUSPENDED", "VERIFYING"];

export function TaskPage() {
  const { taskId = "" } = useParams();
  const [search, setSearch] = useSearchParams();
  const q = useQuery({ queryKey: ["task", taskId], queryFn: () => api.task(taskId) });
  if (q.isLoading) return null;
  if (q.error || !q.data) return <p className="board error">读取任务失败：{q.error?.message}</p>;

  const { task, contracts, runs, children } = q.data;
  const runId = search.get("run") ?? runs[0]?.id;
  const run = runs.find((r) => r.id === runId);
  const contract = contracts[0];

  return (
    <div className="detail">
      <section className="left" aria-label="任务与执行记录">
        <div>
          <Link className="crumb" to={`/goals/${task.goal_id}`}>← 返回看板</Link>
          <h1>{task.title}</h1>
        </div>
        {children.length > 0 && (
          <div>
            <h2>子任务</h2>
            <ul className="runs">{children.map((c) => <li key={c.id}><Link to={`/tasks/${c.id}`}>{c.title}</Link></li>)}</ul>
          </div>
        )}
        <div>
          <h2>执行记录</h2>
          {runs.length === 0 && <p className="hint">还没有派发过。</p>}
          <ul className="runs">
            {runs.map((r, i) => (
              <li key={r.id}>
                <button aria-pressed={r.id === runId} onClick={() => setSearch({ run: r.id })}>
                  <span><State s={r.state} /> 第 {runs.length - i} 次</span>
                  <small>{r.machine_name ?? "等待机器认领"} · {ago(r.created_at)}</small>
                </button>
              </li>
            ))}
          </ul>
        </div>
      </section>

      <section aria-label="执行过程">
        {run ? <Timeline run={run} /> : <p className="hint">派发后，这里会实时显示 agent 的每一步。</p>}
      </section>

      <section className="right" aria-label="控制">
        {run?.state === "GATED" && <ApprovalCard runId={run.id} />}
        <Controls taskId={task.id} run={run} canDispatch={!runs.some((r) => ACTIVE.includes(r.state)) && !!contract} />
        {contract && <ContractView v={contract.version} c={contract.body} />}
      </section>
    </div>
  );
}

function Timeline({ run }: { run: Run }) {
  const ev = useQuery({ queryKey: ["events", run.id], queryFn: () => api.events(run.id) });
  return (
    <>
      <h2>
        <State s={run.state} /> {run.machine_name ? `在 ${run.machine_name} 上` : ""}{run.state_reason ? `：${reasonText(run.state_reason)}` : ""}
      </h2>
      <ol className="timeline">
        {dedupe(ev.data ?? []).map((e) => <EventRow key={e.seq} e={e} />)}
      </ol>
    </>
  );
}

// Claude's final message of a turn is also its claim; show it once, as the claim.
function dedupe(events: RunEvent[]): RunEvent[] {
  return events.filter((e, i) => {
    const next = events[i + 1];
    return !(e.type === "agent_message" && next?.type === "claim" && next.data.text === e.data.text);
  });
}

const STATE_LABEL_SHORT: Record<string, string> = {
  PENDING: "排队中", PREPARING: "准备中", RUNNING: "执行中", GATED: "等你拍板", SUSPENDED: "已暂停",
  VERIFYING: "待验收", SUCCEEDED: "已完成", FAILED: "失败", ABORTED: "已终止", ORPHANED: "失联",
};

function EventRow({ e }: { e: RunEvent }) {
  const d = e.data as Record<string, string | number | boolean>;
  const t = <time dateTime={e.ts}>{clock(e.ts)}</time>;
  switch (e.type) {
    case "state_change":
      return <li className="ev state-change">{t}<div className="ev-body">进入 <b>{STATE_LABEL_SHORT[String(d.to)] ?? String(d.to)}</b>{d.reason ? `（${reasonText(String(d.reason))}）` : ""}</div></li>;
    case "agent_message":
      return <li className="ev message">{t}<div className="ev-body">{String(d.text)}</div></li>;
    case "tool_call": {
      const dec = String(d.decision);
      const label = dec === "allow" ? "放行" : dec === "hold" ? "拦下待批" : "拒绝";
      return (
        <li className={`ev tool ${dec}`}>{t}
          <div className="ev-body"><span className="decision">{label}</span>{String(d.tool)} <code>{String(d.input_digest)}</code>
            {d.reason ? <div className="hint">{reasonText(String(d.reason))}</div> : null}
          </div>
        </li>
      );
    }
    case "error":
      return <li className="ev error">{t}<div className="ev-body">{String(d.message)}{Number(d.count) > 1 ? `（第 ${d.count} 次）` : ""}</div></li>;
    case "claim":
      return <li className="ev claim">{t}<div className="ev-body"><div className="hint">agent 自述{d.kind === "done" ? "已完成，需要验收" : "本轮异常结束"}</div>{String(d.text)}</div></li>;
    default:
      return <li className="ev">{t}<div className="ev-body hint">{e.type}</div></li>;
  }
}

function Controls({ taskId, run, canDispatch }: { taskId: string; run?: Run; canDispatch: boolean }) {
  const qc = useQueryClient();
  const [note, setNote] = useState("");
  const send = useMutation({
    mutationFn: ({ kind, message }: { kind: DirectiveKind; message?: string }) =>
      api.directive(run!.id, kind, message ? { message } : undefined),
    onSuccess: () => { setNote(""); void qc.invalidateQueries(); },
  });
  const dispatch = useMutation({ mutationFn: () => api.dispatch(taskId), onSuccess: () => qc.invalidateQueries() });

  const s = run?.state;
  const onMachine = !!run?.machine_name && s && ACTIVE.includes(s) && s !== "PENDING";
  const go = (kind: DirectiveKind, needsNote = false) => () => {
    if (needsNote && !note.trim()) return;
    send.mutate({ kind, message: note.trim() || undefined });
  };

  return (
    <div className="controls">
      <h2>操作</h2>
      {canDispatch && <button className="btn primary" onClick={() => dispatch.mutate()} disabled={dispatch.isPending}>派发执行</button>}
      {onMachine && (
        <>
          <label className="field">
            <span>{s === "VERIFYING" ? "打回时说明还差什么" : "给 agent 的话（引导或放行时附带）"}</span>
            <textarea className="textarea" value={note} onChange={(e) => setNote(e.target.value)}
              placeholder={s === "GATED" ? "例如：这个目录不能写，改写到工作目录下的 out/" : "例如：样本标签字段叫 label_v2"} />
          </label>
          <div className="btns">
            {s === "GATED" && <button className="btn signal" onClick={go("CONTINUE")}>继续执行</button>}
            {s === "SUSPENDED" && <button className="btn primary" onClick={go("CONTINUE")}>恢复执行</button>}
            {s === "VERIFYING" && <button className="btn primary" onClick={go("COMPLETE")}>验收通过</button>}
            {s === "VERIFYING" && <button className="btn" onClick={go("STEER", true)} disabled={!note.trim()}>打回修改</button>}
            {(s === "RUNNING" || s === "GATED") && <button className="btn" onClick={go("STEER", true)} disabled={!note.trim()}>发送引导</button>}
            {(s === "RUNNING" || s === "PREPARING") && <button className="btn" onClick={go("PAUSE")}>暂停</button>}
            <button className="btn danger" onClick={() => confirm("终止后 agent 会立即停止，确定吗？") && send.mutate({ kind: "ABORT" })}>终止</button>
          </div>
        </>
      )}
      {run && s === "PENDING" && <p className="hint">等待有能力的机器认领。确认目标机器在线，并装有对应的执行器。</p>}
      {send.error && <p className="error">{send.error.message}</p>}
      {dispatch.error && <p className="error">{dispatch.error.message}</p>}
    </div>
  );
}

function ContractView({ v, c }: { v: number; c: import("../api").Contract }) {
  return (
    <div className="contract">
      <h2>Contract 第 {v} 版</h2>
      <dl>
        <div><dt>要求</dt><dd className="prompt">{c.prompt}</dd></div>
        {c.workdir && <div><dt>工作目录</dt><dd>{c.workdir}</dd></div>}
        {c.scope.write_paths.length > 0 && <div><dt>额外可写</dt><dd><ul>{c.scope.write_paths.map((p) => <li key={p}>{p}</li>)}</ul></dd></div>}
        {!!c.scope.readonly_paths?.length && <div><dt>只读</dt><dd><ul>{c.scope.readonly_paths.map((p) => <li key={p}>{p}</li>)}</ul></dd></div>}
        {!!c.acceptance?.length && <div><dt>验收标准</dt><dd><ul>{c.acceptance.map((a) => <li key={a}>{a}</li>)}</ul></dd></div>}
      </dl>
    </div>
  );
}

function ApprovalCard({ runId }: { runId: string }) {
  const qc = useQueryClient();
  const [note, setNote] = useState("");
  const q = useQuery({ queryKey: ["approvals", runId], queryFn: () => api.approvals(runId) });
  const decide = useMutation({
    mutationFn: ({ id, option }: { id: string; option: "allow_dir" | "reject" }) => api.decide(id, option, note.trim() || undefined),
    onSuccess: () => { setNote(""); void qc.invalidateQueries(); },
  });
  const a = q.data?.[0];
  if (!a) return null;
  return (
    <div className="approval" role="region" aria-label="待你拍板">
      <h2>待你拍板</h2>
      <p>agent 想用 <b>{a.context.tool}</b> {a.context.reason ? reasonText(a.context.reason) : a.question}</p>
      <label className="field"><span>附言（可选，会转给 agent）</span>
        <input className="input" value={note} onChange={(e) => setNote(e.target.value)} />
      </label>
      <div className="btns">
        {a.options.map((o) => (
          <button key={o.id} className={`btn ${o.id === "reject" ? "" : "signal"}`} disabled={decide.isPending}
            onClick={() => decide.mutate({ id: a.id, option: o.id })}>
            {o.id === "allow_dir" ? `批准，允许写入 ${o.path}` : o.label}
          </button>
        ))}
      </div>
      {a.options.length === 1 && <p className="hint">高风险命令不会被自动放行。确实需要的话，请自己在机器上执行。</p>}
      {decide.error && <p className="error">{decide.error.message}</p>}
    </div>
  );
}
