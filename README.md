# Helm

个人任务中枢：一个控制面 + 每台机器上的 `helmd`。你在看板上看到每只“手”在做什么，并随时暂停、引导或终止。

- 部署（单机试用 / Tailscale + Docker）：`docs/deploy.md`
- 架构与决策：`docs/architecture.md`
- 当前进度与下一步：`docs/m0-tasks.md`
- 给 coding agent 的说明：`AGENTS.md`（`CLAUDE.md` 引用它）
- 协议：`protocol/README.md`

最快的试用方式（单机、不装 Docker）：

```bash
make demo                        # 构建看板和 helmd，以内嵌 Postgres 启动控制面，打开 http://127.0.0.1:8080
# 看板 → 机器 → 添加机器，按提示在本机执行 deploy/install.sh
```

开发：

```bash
make test        # Go + TS 全部测试，含真实 helmd 的端到端测试
make dev-db      # 本地 Postgres 17 + pgvector
cd server && pnpm dev
make helmd && ./bin/helmd caps
```
