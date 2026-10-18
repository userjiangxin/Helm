# Helm

个人任务中枢：一个控制面 + 每台机器上的 `helmd`。你在看板上看到每只“手”在做什么，并随时暂停、引导或终止。

- 架构与决策：`docs/architecture.md`
- 当前进度与下一步：`docs/m0-tasks.md`
- 给 coding agent 的说明：`AGENTS.md`（`CLAUDE.md` 引用它）
- 协议：`protocol/README.md`

```bash
make test        # Go + TS 全部测试，含真实 helmd 的端到端测试
make dev-db      # 本地 Postgres 17 + pgvector
cd server && pnpm dev
make helmd && ./bin/helmd caps
```
