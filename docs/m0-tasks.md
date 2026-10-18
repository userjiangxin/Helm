# M0 任务清单：骨架

**M0 验收标准：在看板上同时盯住两台机器上的 agent 执行，并能暂停、引导、终止。** 这一阶段由你充当“脑”：在看板上手动创建任务和 Contract、手动派发、手动下指令。

每个任务都写明了改哪些文件、怎样算完成，以及建议用哪个模型。“建议模型”的规则：边界清楚、已有测试兜底的任务用 Sonnet；需要摸索第三方协议或改动并发与状态机的任务用 Opus。

## 已完成（本仓库初始提交）

- [x] 线协议说明与共享样例（`protocol/`），Go 与 TS 两端的契约测试
- [x] 策略引擎：路径 scope、只读目录、高危命令、暂停（`daemon/internal/policy`，14 个用例）
- [x] Claude Code PreToolUse hook 桥：unix socket，daemon 不在线时拒绝（`daemon/internal/hook`）
- [x] 连接层：WebSocket 唤醒、HTTP 认领、30 秒轮询兜底、心跳、断线缓存（`daemon/internal/conn`）
- [x] Supervisor 骨架：认领、状态事件、指令应用与过期拒绝、hold → GATED（`daemon/internal/supervisor`）
- [x] 能力探测：agent CLI、GPU（`helmd caps`）
- [x] M0 表结构与 NOTIFY 触发器（`db/migrations/0001_init.sql`）
- [x] 控制面数据操作：注册、原子认领、幂等事件写入、指令拉取与确认、租约与 ORPHANED 清扫（`server/src/store.ts`）
- [x] 控制面路由：`/daemon/*`、`/api/*`、SSE（`server/src/app.ts`）
- [x] 端到端测试：真实 `helmd` 进程 ↔ 控制面，派发后 5 秒内被唤醒并认领（`server/test/e2e.test.ts`）

## 待完成

### Hand（helmd）

| ID | 任务 | 完成标准 | 建议模型 |
| --- | --- | --- | --- |
| ~~D1~~ ✅ | `helmd register --server --code`：换取机器 token 存入 `~/.helm/credentials`（0600）；`helmd run` 默认读取；附 systemd unit、launchd plist 和一行安装脚本 | 新机器从零到在线只需两条命令；token 不出现在进程参数里 | Sonnet |
| D2 | Run 工作区：每个 repo 一个共享 bare clone（`~/.helm/repos`），每个 Run 一个 git worktree；启动前打快照提交供 ROLLBACK 使用；按 `contract.skills` 链接 skill 目录；完成后按 TTL 清理 | 并发两个 Run 互不干扰；测试覆盖 worktree 创建、快照、清理 | Sonnet |
| D3 | 文件持久化的 spool（每个 Run 一个追加写的 JSONL）；服务端回传每个 Run 已持久化的最大 seq，daemon 据此删除 | kill -9 重启 daemon 后事件不丢、不重复 | Sonnet |
| D4 | Codex 执行器：`codex app-server`（stdio JSON-RPC）。先用 `generate-json-schema` 导出协议类型；实现 initialize、thread/start、turn/start、通知归一化、审批请求接入策略引擎、turn/interrupt、thread/resume | 一个真实的 Codex Run 能跑完；越界写入被拦截并让 Run 进入 GATED；STEER 能在下一轮生效 | **Opus** |
| ~~D5~~ ✅ | Claude 执行器：`claude -p` 的 stream-json 输入输出模式，按 Run 生成 settings 文件注册 `helmd hook pre-tool-use`；实现消息注入、中断、`--resume`；动手前先核对已安装版本的 `claude --help` | 一个真实的 Claude Code Run 能跑完；hook 拦截生效；PAUSE 后下一次工具调用被拒绝 | **Opus** |
| D6 | 心跳负载采样：CPU（读 /proc 或 gopsutil）、`nvidia-smi` 的利用率与显存 | 看板“机器”页能看到每张卡的占用 | Sonnet |
| D7 | 事件归一化工具：报错签名（去掉数字、路径、地址后哈希，按签名计数）、L0 证据落盘（`~/.helm/evidence/<run>/`）并上报 `evidence` 事件 | 同一报错重复出现只产生一个签名且 count 递增 | Sonnet |

### 控制面（server）

| ID | 任务 | 完成标准 | 建议模型 |
| --- | --- | --- | --- |
| ~~S1~~ ✅ | 迁移执行器：`pnpm migrate` 按文件名顺序执行 `db/migrations`，用 `schema_migrations` 表记录 | 重复执行无副作用 | Sonnet |
| S2 | hold → 审批：收到 `decision=hold` 的 tool_call 事件时创建 approval（附工具调用上下文）；批准接口生成 CONTINUE（或带新 scope 的 AMEND + 新 contract 版本），拒绝接口生成 STEER 或 ABORT | 测试覆盖“越界 → 审批 → 批准 → 新 contract 版本 → AMEND 下发”全链路 | **Opus** |
| S3 | 看板 API 补全：任务拖拽（status、position）、按任务列出 Run 历史、人工创建新 contract 版本 | 接口都有 PGlite 测试 | Sonnet |
| S4 | 部署：`docker-compose.yml`（Postgres 17 + pgvector、server、web），以及 `tailscale serve` 的配置说明 | 新机器按文档 15 分钟内跑起来 | Sonnet |
| S5 | 看板鉴权：信任 `tailscale serve` 注入的身份头，只允许你的 Tailscale 账号；直连时退回单用户口令 | 未经 Tailscale 的请求拿不到 `/api/*` | Sonnet |

### 看板（web）

开始前先加载 `frontend-design` skill。技术栈：Vite + React + TypeScript + TanStack Query + dnd-kit，样式是 `web/src/styles.css` 里的一套 CSS 变量（不用 Tailwind）；实时更新只用 `/api/stream` 这一个 SSE 流。设计约定：只有“等你拍板”使用琥珀色强调，其余状态保持低饱和。

| ID | 任务 | 完成标准 | 建议模型 |
| --- | --- | --- | --- |
| ~~W1~~ ✅ | 脚手架、路由、API 客户端（类型从 `server/src/protocol.ts` 引入）、SSE hook | `pnpm dev` 可用；SSE 断开后自动重连 | Sonnet |
| ~~W2~~ ✅ | 看板页：按状态分列，卡片显示机器、执行器、Run 状态、最新一条事件；可拖拽 | 数据库里改状态，卡片 1 秒内移动 | Sonnet |
| ~~W3~~ ✅ | 任务详情页：左侧任务树、中间 Run 时间线（按事件类型渲染，hold 高亮）、右侧 Contract 与指令按钮（继续、引导、暂停、终止） | 在页面上点“暂停”，agent 的下一次工具调用被拒绝 | Sonnet |
| ~~W4~~ ✅ | 机器页：在线状态、agent CLI 版本、GPU 占用、正在跑的 Run；生成注册码 | 新机器注册后 15 秒内出现在列表 | Sonnet |
| W5 | 审批收件箱：待审列表，附上下文，批准或拒绝 | 与 S2 联调通过 | Sonnet |

### 收尾

| ID | 任务 | 完成标准 |
| --- | --- | --- |
| E1 | M0 验收演示：本机跑 Claude Code、云主机或 GPU 机跑 Codex，各派一个任务；在看板上对其中一个 PAUSE → STEER → CONTINUE，对另一个 ABORT | 录屏留档；把遇到的问题记入 M1 |

## 推荐顺序

D1 → S1 → S4（先让两台真实机器连上）→ D5 → W1 → W2 → W3（M0 最小闭环：能看、能停）→ D4 → S2 → W5 → D2 → D3 → D6 → D7 → W4 → S3 → S5 → E1。
