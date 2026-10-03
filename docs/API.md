# go-tiny-claw 接口规范

本文档描述当前 `master` 已实现的外部接口。需要模型或人类审批的接口都应经过权限中间件；除非特别说明，路径和文件参数均限制在工作区内。

## HTTP API

默认本地地址为 `http://127.0.0.1:48080`。`CLAW_HTTP_TOKEN` 只保护 `/v1/*` 路由；`/healthz`、`/metrics` 和 webhook 入口不经过该认证中间件。

| 方法 | 路径 | 请求 | 成功响应 | 失败 |
|---|---|---|---|---|
| POST | `/v1/chat` | `{"session":"s1","prompt":"..."}` | 会话、最终回复、事件、耗时、usage | 400 参数错误；401 Token 错误；500 引擎错误 |
| GET | `/v1/approvals?session=s1` | 无 | 当前会话待审批列表 | 400 缺少 session |
| POST | `/v1/approve` | `{"session":"s1","task_id":"ab12cd","approve":true,"reason":"..."}` | 审批结果 | 400 参数错误；409 任务不存在/已处理/跨会话 |
| GET | `/v1/sessions/{id}` | 无 | 会话历史与 usage | 401 Token 错误 |
| POST | `/v1/sessions/{id}/reset` | 无 | `{"id":"...","reset":true}` | 409 会话正在运行 |

`GET /healthz` 返回 `ok`，`GET /metrics` 返回运行统计（默认不鉴权）。配置 webhook 模式时，`POST /webhook/event` 也不经过 HTTP Token，而由飞书校验参数保护。

`/v1/chat` 同一 session 串行执行；审批 scope 带 `http:` 前缀，不能被飞书或其他 session 冒充。返回事件类型包括 `thinking`、`tool_call`、`tool_result`、`message`、`approval`。

## 工具接口

### 基础工具

| 工具 | 关键入参 | 行为 |
|---|---|---|
| `read_file` | `path` | 工作区内读取，输出按字符边界截断 |
| `write_file` | `path`, `content` | 工作区内原子写入 |
| `edit_file` | `path`, `old_text`, `new_text` | 精确、换行归一、Trim、逐行去缩进四级匹配；非唯一拒绝 |
| `bash` | `command` | 工作区 cwd、超时、输出截断；权限规则决定 allow/ask/deny |
| `spawn_subagent` | 子任务 prompt | 隔离 Session，最多 10 Turn，只读工具/只读 Bash 守卫 |
| `record_knowledge` | `kind`, `subject`, `content`, `reason`, `scope`, `source`, `rule` | 写入 Harness 状态目录 Inbox；Run 结束后统一归位 |

### 检索工具

`search`：`{"query":"...","gap_id":"g1","strategy":"new_entity|constraint|new_term|verify|counter"}`。实际可用入口是子智能体：每次 `spawn_subagent` 注入新的 retrieval task，并继承父 Session；主 Agent 虽挂有注册项，但没有 task 时会拒绝。只索引 child chunk，按父块聚合，工具输出最多 6000 字节（账本不受此上限）；最多返回 8 个候选。重复 query、未知 gap、非法 strategy、预算耗尽会拒绝。

`evidence`：`{"action":"add_gap|mark_fact|mark_covered|mark_unanswerable|finish", ...}`。

- `add_gap`：需要非空且唯一 `id`，问题放在 `text`。
- `mark_fact`：把 `text` 写入事实账本。
- `mark_covered`：需要 `id` 和同一 gap 下真实存在的 `evidence_ids`。
- `mark_unanswerable`：关闭无法通过检索回答的 gap。
- `finish`：仍有 open gap 时拒绝，否则将任务标为 done。

账本按 `(session_id, retrieval_task)` 隔离，不进入工作区。默认保存于 `$CLAW_HOME/go-tiny-claw/retrieval`，未设 `CLAW_HOME` 时使用 `os.UserConfigDir()/go-tiny-claw/retrieval`；显式 `Options.StateDir` 时使用 `<StateDir>/retrieval`。读改写使用单锁事务和临时文件 + rename。

## CLI

```text
claw env lock  [-dir DIR] [-mode warn|strict] [-probe] [-tools bash,git,...]
claw env check [-dir DIR] [-probe]
claw env show  [-dir DIR]
```

`env lock` 写 `.claw/env.lock.json`；`check` 无锁或发现 error 级漂移退出码为 2/1，warn 级漂移只报告；损坏锁和 strict 漂移拒绝启动。`-probe` 请求 Provider 获取服务端实际模型版本；已观测版本变化或期望版本时响应缺少 model 会被视为无法核验。环境锁包含 OS/arch/Go、工具路径与版本、BuildInfo 依赖、Skill hash、模型 requested/observed。

普通运行入口为 `claw -workdir DIR -dir DIR -prompt TEXT [-session ID] [-thinking] [-plan] [-yes] [-max-turns N]`；没有 `-session` 时使用内存中的 `cli` 会话。常驻入口为 `claw serve -workdir DIR [-addr ADDR] [-thinking] [-plan] [-max-turns N]`。`serve` 通过 HTTP 提供上表接口，并在配置 `FEISHU_APP_ID`、`FEISHU_APP_SECRET` 时启用飞书长连接。飞书支持 `FEISHU_CONNECTION_MODE=webhook`、`/webhook/event`、`VERIFY_TOKEN`/`ENCRYPT_KEY`；群聊需 @ 机器人，非文本消息拒绝，过滤所有 `sender_type != user`，重复 event_id 去重。支持 `/reset`（`/new` 别名）以及 `approve/同意/批准`、`reject/拒绝/驳回` + 6 位 task_id。

## 状态与持久化

- Session：`<CLAW_HOME>/sessions/*.jsonl`，首次访问时懒加载，会话 ID 文件名带 hash；遇到第一条坏行时将原文件整体隔离为 `.corrupt-<unix秒>`，再用合法前缀重写。reset 会删除磁盘记录，即使服务刚重启且会话尚未加载。
- Dynamic knowledge：Inbox 在 `<CLAW_HOME>/knowledge/inbox/`；成功归位为 ADR、rules、preferences、discoveries 与 index；失败保留，journal 用于重放。
- Environment lock：工作区 `.claw/env.lock.json`。
- Trace：工作区 `.claw/traces/`，JSON 决策树与 Perfetto/Chrome trace。
