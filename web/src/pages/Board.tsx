import { DndContext, PointerSensor, useDraggable, useDroppable, useSensor, useSensors, type DragEndEvent } from "@dnd-kit/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, COLUMNS, RUN_STATE_LABEL, type BoardTask, type Executor, type TaskStatus } from "../api";
import { reasonText } from "../components/reason";
import { State } from "../components/State";

export function Board() {
  const { goalId } = useParams();
  const navigate = useNavigate();
  const goals = useQuery({ queryKey: ["goals"], queryFn: api.goals });
  const current = goalId ?? goals.data?.[0]?.id;
  const [newGoalOpen, setNewGoalOpen] = useState(false);
  const [newTaskOpen, setNewTaskOpen] = useState(false);

  if (goals.isLoading) return null;
  if (goals.error) return <p className="board error">读取目标失败：{String(goals.error.message)}</p>;

  if (!goals.data?.length) {
    return (
      <div className="empty">
        <h1>还没有目标</h1>
        <p>目标是你想推进的一件事，比如“用 9 月数据重训 ranker”。先建一个目标，再在里面拆任务、派给机器。</p>
        <div><button className="btn primary" onClick={() => setNewGoalOpen(true)}>新建目标</button></div>
        <NewGoalDialog open={newGoalOpen} onClose={() => setNewGoalOpen(false)} onCreated={(id) => navigate(`/goals/${id}`)} />
      </div>
    );
  }

  return (
    <div className="board">
      <div className="board-head">
        <div className="goal-tabs" role="tablist" aria-label="目标">
          {goals.data.map((g) => (
            <button key={g.id} role="tab" className="goal-tab" aria-selected={g.id === current}
              onClick={() => navigate(`/goals/${g.id}`)}>{g.title}</button>
          ))}
        </div>
        <button className="btn" onClick={() => setNewGoalOpen(true)}>新建目标</button>
        <div style={{ flex: 1 }} />
        {current && <button className="btn primary" onClick={() => setNewTaskOpen(true)}>新建任务</button>}
      </div>
      <Attention />
      {current && <Columns goalId={current} />}
      <NewGoalDialog open={newGoalOpen} onClose={() => setNewGoalOpen(false)} onCreated={(id) => navigate(`/goals/${id}`)} />
      {current && <NewTaskDialog goalId={current} open={newTaskOpen} onClose={() => setNewTaskOpen(false)} />}
    </div>
  );
}

function Attention() {
  const q = useQuery({ queryKey: ["attention"], queryFn: api.attention });
  const items = q.data ?? [];
  if (items.length === 0) return null;
  return (
    <section className="attention" aria-live="polite">
      <h2>{items.length} 件事在等你</h2>
      <ul>
        {items.map((a) => (
          <li key={a.run_id}>
            <Link to={`/tasks/${a.task_id}?run=${a.run_id}`}>
              <span className="att-state">{RUN_STATE_LABEL[a.state]}</span>
              <span className="att-reason"><span className="att-title">{a.title}</span>{a.state_reason ? `：${reasonText(a.state_reason)}` : ""}</span>
              <span className="hint">{a.machine_name ?? ""}</span>
            </Link>
          </li>
        ))}
      </ul>
    </section>
  );
}

function Columns({ goalId }: { goalId: string }) {
  const qc = useQueryClient();
  const tasks = useQuery({ queryKey: ["tasks", goalId], queryFn: () => api.tasks(goalId) });
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 6 } }));
  const move = useMutation({
    mutationFn: ({ id, status, position }: { id: string; status: TaskStatus; position: number }) => api.patchTask(id, { status, position }),
    onMutate: async ({ id, status, position }) => {
      await qc.cancelQueries({ queryKey: ["tasks", goalId] });
      const prev = qc.getQueryData<BoardTask[]>(["tasks", goalId]);
      qc.setQueryData<BoardTask[]>(["tasks", goalId], (old) => old?.map((t) => (t.id === id ? { ...t, status, position } : t)));
      return { prev };
    },
    onError: (_e, _v, ctx) => ctx?.prev && qc.setQueryData(["tasks", goalId], ctx.prev),
    onSettled: () => qc.invalidateQueries({ queryKey: ["tasks", goalId] }),
  });

  const byStatus = (s: TaskStatus) => (tasks.data ?? []).filter((t) => t.status === s).sort((a, b) => a.position - b.position);

  const onDragEnd = (e: DragEndEvent) => {
    const to = e.over?.id as TaskStatus | undefined;
    const task = tasks.data?.find((t) => t.id === e.active.id);
    if (!to || !task || task.status === to) return;
    const last = byStatus(to).at(-1);
    move.mutate({ id: task.id, status: to, position: (last?.position ?? 0) + 1 });
  };

  return (
    <DndContext sensors={sensors} onDragEnd={onDragEnd}>
      <div className="columns">
        {COLUMNS.map((c) => <Column key={c.status} status={c.status} label={c.label} tasks={byStatus(c.status)} />)}
      </div>
    </DndContext>
  );
}

function Column({ status, label, tasks }: { status: TaskStatus; label: string; tasks: BoardTask[] }) {
  const { setNodeRef, isOver } = useDroppable({ id: status });
  return (
    <div ref={setNodeRef} className={`column ${isOver ? "over" : ""}`} aria-label={label}>
      <h3><b>{label}</b><span>{tasks.length || ""}</span></h3>
      {tasks.map((t) => <Card key={t.id} t={t} />)}
    </div>
  );
}

const EXECUTOR_LABEL: Record<Executor, string> = { claude: "Claude Code", codex: "Codex", shell: "Shell", job: "长任务" };

function Card({ t }: { t: BoardTask }) {
  const { attributes, listeners, setNodeRef, transform, isDragging } = useDraggable({ id: t.id });
  const style = transform ? { transform: `translate(${transform.x}px, ${transform.y}px)` } : undefined;
  return (
    <Link ref={setNodeRef} to={`/tasks/${t.id}`} style={style}
      className={`card ${t.run_state === "GATED" ? "gated" : ""} ${isDragging ? "dragging" : ""}`}
      {...listeners} {...attributes}>
      <span className="card-title">{t.title}</span>
      <span className="card-meta">
        {t.run_state && <State s={t.run_state} />}
        <span>{EXECUTOR_LABEL[t.executor]}</span>
        {t.target_labels.length > 0 && <span>· {t.target_labels.join(", ")}</span>}
      </span>
      {t.state_reason && t.run_state && !["SUCCEEDED", "RUNNING"].includes(t.run_state) && <span className="card-reason">{reasonText(t.state_reason)}</span>}
    </Link>
  );
}

function useDialog(open: boolean) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    if (!open && d.open) d.close();
  }, [open]);
  return ref;
}

function NewGoalDialog({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: (id: string) => void }) {
  const ref = useDialog(open);
  const qc = useQueryClient();
  const [title, setTitle] = useState("");
  const [intent, setIntent] = useState("");
  const create = useMutation({
    mutationFn: () => api.createGoal(title.trim(), intent.trim()),
    onSuccess: ({ id }) => { void qc.invalidateQueries({ queryKey: ["goals"] }); setTitle(""); setIntent(""); onClose(); onCreated(id); },
  });
  return (
    <dialog ref={ref} onClose={onClose}>
      <form method="dialog" onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
        <div className="dialog-body">
          <h2>新建目标</h2>
          <label className="field"><span>目标</span>
            <input className="input" required value={title} onChange={(e) => setTitle(e.target.value)} placeholder="用 9 月新增数据重训 ranker-v7" autoFocus />
          </label>
          <label className="field"><span>背景（可选）</span>
            <textarea className="textarea" value={intent} onChange={(e) => setIntent(e.target.value)} placeholder="为什么做、做到什么程度算完" />
          </label>
          {create.error && <p className="error">{create.error.message}</p>}
        </div>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={onClose}>取消</button>
          <button className="btn primary" disabled={!title.trim() || create.isPending}>创建目标</button>
        </div>
      </form>
    </dialog>
  );
}

const lines = (s: string) => s.split("\n").map((l) => l.trim()).filter(Boolean);

function NewTaskDialog({ goalId, open, onClose }: { goalId: string; open: boolean; onClose: () => void }) {
  const ref = useDialog(open);
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [f, setF] = useState({ title: "", executor: "claude" as Executor, labels: "", workdir: "", repo: "", baseRef: "", prompt: "", write: "", readonly: "", acceptance: "" });
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });

  const create = useMutation({
    mutationFn: async (dispatch: boolean) => {
      const { id } = await api.createTask(goalId, {
        title: f.title.trim(),
        executor: f.executor,
        target_labels: f.labels.split(",").map((s) => s.trim()).filter(Boolean),
        contract: {
          intent: f.title.trim(),
          prompt: f.prompt.trim(),
          workdir: f.workdir.trim() || undefined,
          repo: f.repo.trim() || undefined,
          base_ref: f.repo.trim() ? f.baseRef.trim() || undefined : undefined,
          scope: { write_paths: lines(f.write), readonly_paths: lines(f.readonly) },
          acceptance: lines(f.acceptance),
          budgets: {},
        },
      });
      if (dispatch) await api.dispatch(id);
      return id;
    },
    onSuccess: (id) => { void qc.invalidateQueries(); onClose(); navigate(`/tasks/${id}`); },
  });

  return (
    <dialog ref={ref} onClose={onClose}>
      <form method="dialog" onSubmit={(e) => { e.preventDefault(); create.mutate(true); }}>
        <div className="dialog-body">
          <h2>新建任务</h2>
          <p className="hint">现阶段由你充当“脑”：在这里写清楚要做什么、能动哪些路径、怎样算完成。</p>
          <label className="field"><span>任务</span>
            <input className="input" required value={f.title} onChange={set("title")} placeholder="准备 2026-09 训练数据并做校验" autoFocus />
          </label>
          <div className="row">
            <label className="field"><span>执行器</span>
              <select className="select" value={f.executor} onChange={set("executor")}>
                <option value="claude">Claude Code</option>
                <option value="codex">Codex</option>
              </select>
            </label>
            <label className="field"><span>机器标签（逗号分隔，可空）</span>
              <input className="input" value={f.labels} onChange={set("labels")} placeholder="gpu" />
            </label>
          </div>
          <div className="row">
            <label className="field"><span>工作目录（在目标机器上）</span>
              <input className="input" required={!f.repo.trim()} value={f.workdir} onChange={set("workdir")} placeholder="/home/you/projects/ranker" />
            </label>
            <label className="field"><span>或者：Git 仓库（每次执行用独立副本，可回滚）</span>
              <input className="input" value={f.repo} onChange={set("repo")} placeholder="git@github.com:you/ranker.git" />
            </label>
          </div>
          {f.repo.trim() && (
            <label className="field"><span>分支或提交（默认 HEAD）</span>
              <input className="input" value={f.baseRef} onChange={set("baseRef")} placeholder="main" />
            </label>
          )}
          <label className="field"><span>具体要求</span>
            <textarea className="textarea" required value={f.prompt} onChange={set("prompt")} placeholder="下载 oss://… 下 2026-09 的分区，按 schema v3 校验，不要启动训练。" />
          </label>
          <div className="row">
            <label className="field"><span>额外可写路径（每行一个，工作目录默认可写）</span>
              <textarea className="textarea" value={f.write} onChange={set("write")} placeholder="/data/ranker/2026-09/**" />
            </label>
            <label className="field"><span>只读路径（每行一个）</span>
              <textarea className="textarea" value={f.readonly} onChange={set("readonly")} placeholder="/data/ranker/prod/**" />
            </label>
          </div>
          <label className="field"><span>验收标准（每行一条）</span>
            <textarea className="textarea" value={f.acceptance} onChange={set("acceptance")} placeholder="样本数在 1.8M 到 2.2M 之间" />
          </label>
          {create.error && <p className="error">{create.error.message}</p>}
        </div>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={onClose}>取消</button>
          <button type="button" className="btn" disabled={!f.title.trim() || create.isPending} onClick={() => create.mutate(false)}>只创建</button>
          <button className="btn primary" disabled={!f.title.trim() || !f.prompt.trim() || create.isPending}>创建并派发</button>
        </div>
      </form>
    </dialog>
  );
}
