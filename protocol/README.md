# Helm 线协议 v1（脑 / 控制面 ↔ 手）

本目录是 helmd（Go）与 helm-server（TypeScript）之间协议的**唯一说明**。
两端各自实现类型：

- Go：`daemon/internal/wire/wire.go`
- TS：`server/src/protocol.ts`

`fixtures/` 里的 JSON 是两端共同的契约测试样例：两端的单元测试都必须能解析全部样例，并且重新序列化后字段不丢失。**改协议时先改样例，再改两端类型。**

## 设计原则

1. **WebSocket 只负责提醒，真相在数据库。** 服务端通过 WS 推 `wake`；helmd 收到后走 HTTP 认领 Run、拉取指令。WS 断了也不丢活，30 秒轮询兜底。
2. **所有写操作幂等。** 认领带 `epoch`；指令带 `contract_version`；事件带单调递增的 `seq`。重复投递只会被去重，不会产生副作用。
3. **版本落后的指令一律拒绝。** helmd 执行指令前比较 `contract_version`，落后则 ack 为 `rejected_stale`。

## 连接与认证

- helmd 通过 `helmd register --server <url> --token <一次性注册码>` 换取长期机器凭证 `machine_token`，存于 `~/.helm/credentials`（0600）。
- 之后所有 HTTP 请求带 `Authorization: Bearer <machine_token>`；WS 连接 `GET /daemon/ws` 同样在握手时带该头。
- 心跳：每 15 秒 WS 发送一次 `heartbeat`；服务端超过 60 秒未收到即将该机器标为 `offline`，其名下 Run 进入 `ORPHANED`。

## HTTP 接口（helmd → server）

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| POST | `/daemon/register` | 用注册码换机器凭证 |
| PUT | `/daemon/capabilities` | 上报能力（启动时与 agent CLI 版本变化时） |
| POST | `/daemon/claim` | 认领待执行的 Run，返回 `RunAssignment[]` |
| GET | `/daemon/directives?after=<id>` | 拉取发给本机的未处理指令 |
| POST | `/daemon/directives/:id/ack` | 回报指令执行结果 |
| POST | `/daemon/events` | WS 不可用时的事件上报兜底 |
| POST | `/daemon/runs/:id/state` | Run 状态变更（同时也以事件形式上报） |

## WebSocket 消息

统一信封：

```json
{ "v": 1, "type": "<类型>", "id": "<ulid>", "ts": "<RFC3339>", "payload": { } }
```

| 方向 | type | payload |
| --- | --- | --- |
| server → helmd | `wake` | `{ "reason": "run_queued" \| "directive_queued" \| "skills_updated" }` |
| helmd → server | `heartbeat` | `{ "running_runs": ["run_id"], "load": {...} }` |
| helmd → server | `run_events` | `{ "run_id", "events": [Event] }`，events 按 seq 递增 |

## Run 状态

`PENDING → PREPARING → RUNNING ⇄ GATED → VERIFYING → SUCCEEDED`，
另有 `SUSPENDED`、`ABORTED`、`ORPHANED`、`FAILED`。状态图见架构文档。

## L1 事件（M0 子集）

| type | 关键字段 | 说明 |
| --- | --- | --- |
| `state_change` | from, to, reason | Run 状态变化 |
| `agent_message` | role, text | agent 的文本输出（已截断，完整内容在 L0） |
| `tool_call` | tool, input_digest, decision | 工具调用及策略判定（allow / deny / hold） |
| `cmd_exit` | cmd_digest, exit_code, duration_ms | 命令结束 |
| `fs_change` | path, added, removed, in_scope | 文件变更 |
| `error` | signature, message, count | 归一化后的报错签名 |
| `evidence` | uri, kind, bytes | 新增 L0 证据的地址（`ev://<machine>/<run>/<name>`） |
| `claim` | kind, text | agent 自述（`done` 一轮结束并声称完成、`error` 一轮异常结束），**只是声明，不是证据**；`done` 让 Run 进入 `VERIFYING` |

## 指令（Directive）

| kind | 生效时机 | helmd 的动作 |
| --- | --- | --- |
| `CONTINUE` | 立即 | 放行当前挂起的闸门，可附带一句说明 |
| `STEER` | 下一轮 | 向 agent 注入一条消息 |
| `AMEND` | 立即 + 下一轮 | 用 `args.scope`（完整的新 scope，而非增量，保证幂等）替换策略引擎的 scope，再注入 `args.message` |
| `PAUSE` | 下一个工具边界 | 拒绝后续工具调用；Job 用 SIGSTOP |
| `ROLLBACK` | 立即 | 恢复 worktree 快照，开新会话并注入回滚说明 |
| `ABORT` | 立即 | 终止进程，进入 `ABORTED` |
| `COMPLETE` | 立即 | 验收通过：结束会话，进入 `SUCCEEDED`（只能在 `VERIFYING` 状态下执行） |

ack 状态：`applied`、`rejected_stale`、`rejected_invalid`、`failed`。
