import { useMutation, useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { ago } from "../components/State";

export function Machines() {
  const q = useQuery({ queryKey: ["machines"], queryFn: api.machines, refetchInterval: 15_000 });
  const code = useMutation({ mutationFn: api.registrationCode });
  const server = window.location.origin;

  return (
    <div className="machines">
      <div className="board-head">
        <h1 style={{ margin: 0, fontSize: 18 }}>机器</h1>
        <div style={{ flex: 1 }} />
        <button className="btn primary" onClick={() => code.mutate()} disabled={code.isPending}>添加机器</button>
      </div>
      {code.data && (
        <div className="field">
          <span>在要接入的机器上，进入 Helm 仓库目录执行（注册码 30 分钟内有效，只能用一次）：</span>
          <div className="codebox">{`deploy/install.sh --server ${server} --code ${code.data.code} --labels gpu`}</div>
          <span className="hint">没有 GPU 的机器去掉 --labels gpu；装了钉钉 CLI 的机器可以加上 dingtalk。</span>
        </div>
      )}
      {q.data?.length === 0 && <p className="hint">还没有机器接入。点“添加机器”生成注册命令。</p>}
      {!!q.data?.length && (
        <table className="mtable">
          <thead><tr><th>机器</th><th>状态</th><th>执行器</th><th>GPU</th><th>正在执行</th></tr></thead>
          <tbody>
            {q.data.map((m) => {
              const gpus = m.last_heartbeat?.load.gpus ?? [];
              return (
                <tr key={m.id}>
                  <td><b>{m.name}</b><div className="hint">{m.labels.join(", ")}</div></td>
                  <td><span className={`dot ${m.status === "online" ? "on" : ""}`} /> {m.status === "online" ? "在线" : m.status === "draining" ? "排空中" : "离线"}
                    <div className="hint">{ago(m.last_seen_at)}</div></td>
                  <td>{(m.capabilities.agents ?? []).map((a) => <div key={a.name}>{a.name} <span className="hint">{a.version}</span></div>)}
                    {!(m.capabilities.agents ?? []).length && <span className="hint">未检测到</span>}</td>
                  <td>{gpus.length ? gpus.map((g) => (
                    <div key={g.index}><span className="gpu-bar"><i style={{ width: `${Math.round(g.util_pct)}%` }} /></span>
                      #{g.index} {Math.round(g.util_pct)}% · {(g.mem_used_mb / 1024).toFixed(0)}/{(g.mem_total_mb / 1024).toFixed(0)} GB</div>
                  )) : (m.capabilities.gpus ?? []).map((g) => <div key={g.index}>#{g.index} {g.name}</div>)}
                    {!gpus.length && !(m.capabilities.gpus ?? []).length && <span className="hint">无</span>}</td>
                  <td>{m.last_heartbeat?.running_runs.length ?? 0}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}
