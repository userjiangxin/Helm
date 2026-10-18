// helmd writes state reasons in English (they are also log lines); the board shows them in Chinese.
const RULES: [RegExp, (m: RegExpMatchArray) => string][] = [
  [/^claimed$/, () => "已被机器认领"],
  [/^executor started$/, () => "agent 已启动"],
  [/^agent reports the task is done; awaiting verification$/, () => "agent 自述已完成，等你验收"],
  [/^agent turn ended with an error$/, () => "agent 本轮异常结束"],
  [/^write outside contract scope: (.+)$/, (m) => `想写入范围外的路径：${m[1]}`],
  [/^path is read-only in this contract: (.+)$/, (m) => `想改动只读路径：${m[1]}`],
  [/^high-risk command needs approval: (.+)$/, (m) => `高风险命令待批：${m[1]}`],
  [/^command denied by contract, needs approval: (.+)$/, (m) => `Contract 禁止的命令：${m[1]}`],
  [/^accepted by reviewer$/, () => "已验收"],
  [/^lease expired$/, () => "机器失联（租约过期）"],
  [/^agent process exited: (.+)$/, (m) => `agent 进程退出：${m[1]}`],
  [/^agent process exited unexpectedly$/, () => "agent 进程意外退出"],
  [/^executor not available on this machine: (.+)$/, (m) => `这台机器没有执行器 ${m[1]}`],
  [/^(CONTINUE|STEER|PAUSE|ABORT|AMEND)$/, (m) => ({ CONTINUE: "你放行了", STEER: "你发送了引导", PAUSE: "你暂停了", ABORT: "你终止了", AMEND: "你修改了范围" })[m[1]!]!],
];

export function reasonText(r: string | null | undefined): string {
  if (!r) return "";
  for (const [re, f] of RULES) {
    const m = r.match(re);
    if (m) return f(m);
  }
  return r;
}
