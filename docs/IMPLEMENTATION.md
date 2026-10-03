# go-tiny-claw 实现总结（《从 0 开始构建 Agent Harness》第 02–22 讲）

> 课程：极客时间《从 0 开始构建 Agent Harness》（Tony Bai），源 PDF 位于 `~/Downloads/从 0 开始构建 Agent Harness/`。
> 本仓库按讲次逐段实现、逐段验证（git 历史即实现顺序），并在真实模型（火山方舟 `doubao-seed-evolving`，OpenAI 与 Anthropic 双协议）上复现了每一讲的实战场景。
> 课程代码中的缺陷，这里都修掉了，并在代码注释里写明原因，第 5 节有汇总。

## 1. 总体架构（四层 + 横切）

```
cmd/claw            入口：CLI 单次运行 / serve 常驻服务（HTTP API + 飞书）
internal/engine     核心引擎层：Main Loop、两阶段思考、并发执行、Reminder、Subagent 子循环、Reporter
internal/provider   模型适配层：OpenAI 兼容（openai-go/v3）、Claude（anthropic-sdk-go）
internal/tools      工具与执行层：Registry + Middleware、read/write/edit/bash、spawn_subagent、只读守卫
internal/context    上下文工程：PromptComposer、Skills、Session、Compactor、RecoveryManager（包名 context，引用别名 ctxpkg）
internal/permission 安全：allow/ask/deny 策略（热加载）、ApprovalManager（channel 挂起/唤醒）
internal/observability  CostTracker（Provider 装饰器）、TimedRegistry（工具耗时）
internal/feishu     飞书：事件分发、FeishuReporter、审批口令
internal/httpapi    本地部署入口：/v1/chat、/v1/approvals、/v1/approve
internal/runctx     通过 ctx 透传当前 Session / Reporter，供中间件、计费等横切组件使用
internal/schema     统一"血液"：Message / ToolCall / ToolResult / ToolDefinition / Usage
internal/retrieval  动态 Chunk、账本、Evidence/Gap/Goal、RRF、停止评估与检索指标
internal/knowledge  Inbox、知识归位、ADR/rules/preferences/discoveries 与恢复 journal
internal/envlock    运行时/依赖/Skill/模型快照、漂移分级与 ModelGuard
```

一次 Turn 的数据流：

```
Session.GetWorkingMemory(20) ─┐
PromptComposer.Build() ───────┼─> Compactor.Compact ─> [Phase1 Thinking: tools=nil]
                              │                        [Phase2 Action: tools=Registry]
                              │                                   │ (CostTracker 记账)
                              │          ToolCalls ─> executeTools（只读并发/涉写串行）
                              │                        └ Registry: Middleware(权限/审批) → Tool.Execute → TimedRegistry
                              │                        └ 失败 → RecoveryManager 注入锦囊
Session.Append(assistant, observations, reminder?) <──────────┘
```

## 2. 逐讲实现要点

| 讲 | 主题 | 核心实现 | 主要文件 |
|---|---|---|---|
| 02 | Main Loop | ReAct 无限循环：模型不再请求工具即结束；Observation 必须带 ToolCallID 回灌 | `engine/loop.go` |
| 03 | 慢思考 | Two-Stage ReAct：Phase 1 以 `tools=nil` 请求强制规划，Phase 2 恢复工具行动；`EnableThinking` 开关 | `engine/loop.go` |
| 04 | Provider | `LLMProvider.Generate(ctx, msgs, tools)`，OpenAI/Claude 双向翻译，Usage 透传；`CLAW_*` 环境变量配置 | `provider/*.go` |
| 05 | Tool Registry | `BaseTool` 接口 + 注册/暴露 Schema/路由分发；未知工具返回 IsError 让模型自纠；read_file 8000 字节截断 | `tools/registry.go`, `read_file.go` |
| 06 | 极简工具 + YOLO | write_file / bash；bash 四条底线：工作区绑定、超时、错误回传、输出截断 | `tools/write_file.go`, `bash.go` |
| 07 | 稳健 Edit | L1 精确 → L2 换行归一 → L3 Trim → L4 逐行去缩进；每级都做唯一性校验；L4 自动补齐基础缩进（思考题） | `tools/edit_file.go` |
| 08 | 并发 | Fork-Join + 预分配切片保序；采用课程思维实验推荐的"只读并发、涉写串行"，并加信号量限流（思考题） | `engine/loop.go: executeTools` |
| 09 | 飞书 | Reporter 接口解耦 I/O；飞书事件分发、每条消息一个 goroutine、同会话排队（思考题）、事件去重 | `engine/reporter.go`, `feishu/*` |
| 10 | 提示词组装 | 极简内核 + `AGENTS.md` + `.claw/skills/*/SKILL.md`（YAML frontmatter） | `context/composer.go`, `skill.go` |
| 11 | Session | SessionManager 物理隔离；Working Memory 截取最近 N 条并丢弃开头的孤儿 ToolResult；可选 JSONL 持久化 | `context/session.go` |
| 12 | Compaction | 超水位时：远期工具输出掩码（ToolCalls 保留）、远期推理折叠、保护区超长输出掐头去尾；只作用于请求，不改 Session | `context/compactor.go` |
| 13 | 状态外部化 | Plan Mode 注入 PLAN.md/TODO.md 强制规范（嗅探→新建/续传→逐步打勾）；思考与行动合并为单条 assistant | `context/composer.go` |
| 14 | 错误自愈 | 失败 Observation 按工具+特征匹配注入"[系统救援指南]" | `context/recovery.go` |
| 15 | System Reminders | 规范化指纹 + 按会话滑动窗口统计，同一动作失败 ≥3 次注入 User 角色的强力提醒；提醒后仍原样重试则熔断该动作 | `engine/reminder.go` |
| 16 | Middleware 与审批 | Registry.Use 中间件链；allow/ask/deny 策略（deny>ask>allow，`.claw/permissions.json` 热加载）；ask 时通过 ctx 找到当前 Reporter 走飞书/终端/HTTP 审批，无通道则 fail-closed | `tools/registry.go`, `permission/*`, `feishu/approval.go` |
| 17 | Subagent | `spawn_subagent` 就是一个普通工具：`RunSub` 拉起上下文隔离的一次性子循环（≤10 轮），只拿只读注册表 + 只读 bash 守卫；多个子智能体可并行 | `tools/subagent.go`, `engine/subagent.go`, `tools/readonly_guard.go` |
| 18 | 成本追踪 | `CostTracker` 装饰 Provider（按 ctx 中的会话记账）；`TimedRegistry` 装饰 Registry 记录工具本地耗时（思考题）；`/metrics` | `observability/tracker.go` |
| 19 | Tracing | `internal/trace`：借助 ctx 自动挂接父子 Span（Agent.Run → Turn-N → LLM.Thinking/LLM.Action/Tool.Execute → Subagent.Run）；导出嵌套 JSON 决策树 + Perfetto/Chrome Trace 甘特图（思考题）；Turn 逐轮结束（修正课程 defer 在循环里导致耗时全错） | `trace/trace.go`, `engine/loop.go` |
| 20 | Benchmark | `internal/eval` + `cmd/bench`：Setup → Agent → 验证脚本判卷（Fail-to-Pass），隔离工作区并发跑；轨迹指标（轮数、工具失败、自愈提示、Reminder、一发入魂，思考题）；Markdown/JSON 报告与基线回归检测（exit 2，可接 CI）；`bench/cases.json` 7 道题，判卷脚本均做过正反向校验 | `eval/benchmark.go`, `cmd/bench` |
| 21 | CLI 串讲 | `internal/app.Build` 统一装配（CLI/服务端/Benchmark 共用）；CLI 支持 `-dir`、横幅、财务报表与 Trace 路径；并发竞态修复作为 Benchmark 用例 t07（Plan Mode） | `app/app.go`, `cmd/claw` |
| 22 | AgentOps | `deploy/agentops`：运维 AGENTS.md + ops_troubleshoot Skill + 权限规则（write/edit/nginx -s/systemctl/kill 需审批，sed -i 禁止）+ 故障现场脚本 + 假 nginx；飞书群聊意图过滤（未 @ 机器人不唤醒，思考题）；离线全链路测试 `TestAgentOpsScenario` | `deploy/agentops/*`, `feishu/bot.go` |

### 扩展：动态知识更新（跨会话知识丢失的解法）

内层循环在交付前按"用户统筹 / 发现 / 决策"三类调用 `record_knowledge` 写入会话暂存区；外层（Harness）在 Run 结束后按工作区知识布局统一归位到 ADR、规则、偏好、发现文件，并由 PromptComposer 注入后续会话。详见 `docs/design/dynamic-knowledge.md`。

### 扩展：环境锁定（环境漂移的解法）

锁定可控项、隔离不可控项：`internal/envlock` 把运行时（OS/arch/Go、Agent 调用的 bash/git/rg/go/grep/sed 版本）、依赖（BuildInfo 的 version+sum）、Skill 内容哈希、模型（请求名 + 服务端回报名）写进 `<workdir>/.claw/env.lock.json`；`app.Build` 启动前比对（strict 拒绝启动），`ModelGuard` 在每次响应上核对实际模型版本，Benchmark 报告带环境指纹。`init.sh`（toolchain 固定 + `go mod verify` + 工作区锁门禁）接入 `make deploy`，`Dockerfile` 按 digest 锁基础镜像。详见 `docs/design/environment-lock.md`。

```bash
make env-check                 # 当前环境与部署工作区的锁比对
make env-lock MODE=strict PROBE=1   # 有意升级后重新锁定（探测服务端实际模型版本）
make docker-prebuilt GOPROXY_BUILD=https://goproxy.cn,direct   # 构建镜像（colima 4G 内存不足以在容器内编译 SDK）
```

## 3. 使用

```bash
# 真实模型配置（本机从 ~/.hermes/.env 读取方舟 key，不复制密钥）
source scripts/env.sh openai      # 或 claude（走 Anthropic 协议）

go run ./cmd/claw -workdir /tmp/ws -prompt "读取 hello.txt 并总结"      # 单次运行
go run ./cmd/claw -workdir /tmp/ws -thinking -plan -session demo -prompt "搭一个 Go Web Server"   # 慢思考 + Plan Mode + 可续聊
go run ./cmd/claw -yes ...                                               # 沙箱中自动批准高危操作

make bench         # 第 20 讲自动化跑分（bench/cases.json，报告在 bench/reports/）
make agentops-demo # 第 22 讲运维演示工作区
make test          # 单元/集成测试（离线，含飞书 webhook、审批、子智能体、HTTP API）
make live RUN=L12  # 用真实模型复现某一讲的实战场景（internal/e2e，build tag live）
```

其他 Provider：`CLAW_PROVIDER=openai|claude`、`CLAW_API_KEY`、`CLAW_BASE_URL`、`CLAW_MODEL`；不设置时按课程默认走智谱 `glm-4.5-air`（`ZHIPU_API_KEY`）。

## 4. 本地部署

```bash
make deploy        # 安装 ~/.local/bin/claw，注册 launchd 服务 com.weberzhao.go-tiny-claw，监听 127.0.0.1:48080
make status        # launchd 状态 + /metrics
make logs          # ~/.go-tiny-claw/logs/claw.log
make undeploy
```

- 工作区 `~/.go-tiny-claw/workspace`，会话 JSONL 在 `~/.go-tiny-claw/sessions/`（重启自动恢复）。
- HTTP API：
  ```bash
  curl -s localhost:48080/v1/chat -d '{"session":"s1","prompt":"用 bash 看一下 go 版本"}'
  curl -s 'localhost:48080/v1/approvals?session=s1'                  # 查看挂起的高危操作
  curl -s localhost:48080/v1/approve -d '{"session":"s1","task_id":"ab12cd","approve":true}'
  ```
- 接飞书：在 `~/.go-tiny-claw/serve.env` 写入 `FEISHU_APP_ID` / `FEISHU_APP_SECRET`（默认长连接模式，无需公网 IP），然后 `make deploy`。群里发 `approve <id>` / `reject <id>` 审批，`/reset` 清空会话。
  注意：不要复用其他常驻机器人（如 botmux / hermes）的应用凭据，飞书长连接会把事件随机分给多个连接。

## 5. 相对课程代码的修复与增强

| # | 问题（课程原实现） | 现象 / 影响 | 处理 |
|---|---|---|---|
| 1 | Phase 1 无工具时模型在文本里"脑补"伪工具调用 | 实测 doubao 输出 `<seed:tool_call>`，Phase 2 不再真正调工具 | Phase 1 追加临时"只规划"提示，Phase 2 追加"按计划行动"提示（不入 Session） |
| 2 | Phase 2 请求以 assistant(思考) 结尾 | Anthropic 协议当作 prefill 续写，模型只写字不调工具 | 同上，以 user 提示收尾 |
| 3 | 历史含 tool_use 时以 `tools=nil` 请求 | Anthropic 协议直接 400 | Thinking 阶段把工具历史降级为文本 |
| 4 | bash 失败返回 `(字符串, nil)` | `IsError` 恒为 false，第 14 讲 Recovery、第 15 讲 Reminder 对 bash 全部失效 | 非零退出/超时返回 error，由 Registry 包装为 IsError |
| 5 | `./server &` 卡死整个引擎（第 13 讲结尾） | 后台进程继承输出管道，`CombinedOutput` 永不返回 | 独立进程组 + 超时杀整组 + `WaitDelay` 自动分离 |
| 6 | 截断用 `s[:n]` | 切断 UTF-8 中文字符，产生非法 JSON 字符串 | `textutil.Head/Tail` 按字符边界对齐 |
| 7 | 文件工具无路径校验（课程 TODO） | `../../etc/passwd` 可读写 | `resolveInWorkDir` 限制在工作区内 |
| 8 | Registry 用 map 遍历导出工具 | 每轮工具顺序随机，破坏提示词缓存 | 保持注册顺序 |
| 9 | Claude 适配器用 `/api/paas/v4/` | 那是智谱的 OpenAI 路径 | Anthropic 协议使用 `/api/anthropic` |
| 10 | Claude 消息逐条追加 | 并行 tool_result 分散、首条可能是 assistant | 合并相邻同角色消息；首条补 user |
| 11 | Reminder 状态挂在引擎上、只看第 0 个工具、任何成功即清零 | 多会话互相污染；穿插一次 ls 就能永远逃过检测 | 按会话滑动窗口 + 全部调用 + 规范化指纹（思考题） |
| 12 | 审批用长 ToolCallID、任意群都能批准、无限等待 | 手打困难；越权审批；协程永久挂起 | 6 位短 ID；仅同会话可批；超时/取消自动拒绝 |
| 13 | `rm\s+-r` 规则 | 抓不到 `rm -fr` | 选项组合匹配；新增 deny/ask 三态与热加载 |
| 14 | CostTracker 构造时绑死一个 Session；注释写美元打印人民币 | 服务端所有人账单记到同一会话 | 从 ctx 取当前会话；统一人民币/百万 Token，可用 `CLAW_PRICE_IN/OUT` 覆盖 |
| 15 | 子智能体只"不给 edit_file"，但 bash 能写文件；Reporter 构造时固定 | 爆炸半径没真正限制；飞书多会话进度发错群 | 只读 bash 守卫中间件；Reporter 从 ctx 取；超轮次返回阶段性发现 |
| 16 | 飞书内容用 `TrimPrefix("{\"text\":\"")` 剥壳 | 换行/引号转义错乱，@ 占位符残留 | JSON 解析 + 去 `@_user_N`；忽略机器人消息；事件去重 |
| 17 | 第 16 讲飞书 bot 绑定单个 Session，`b.r` 在多协程间竞争 | 所有群共享一段记忆；审批消息发错群 | 每个 chat_id 一个 Session；Reporter 按运行透传 |
| 18 | 只有 Reminder，没有兜底 | 真机：用户要求"重试 5 次"时模型无视提醒继续重试 | 提醒后同一动作在本次唤醒内熔断（不再真实执行），另有 MaxTurns 硬预算 |

## 6. 验证记录

完整接口清单与测试矩阵见 [`docs/API.md`](API.md) 和 [`docs/TEST_MATRIX.md`](TEST_MATRIX.md)。

- 离线：`make test` 覆盖 9 个包——Provider 请求体翻译（httptest 抓包）、四级模糊匹配与缩进对齐、路径边界、bash 超时/后台进程/截断、并发保序与串行退化、Session 隔离与持久化恢复、Compactor、Recovery、Reminder、权限判定与热加载、审批（跨会话越权/超时/取消）、飞书 webhook（challenge、去重、串行、审批全链路）、子智能体隔离与只读守卫、CostTracker、HTTP API 审批链路。
- 真实模型（`internal/e2e`，`make live`）：复现各讲"奇迹时刻"，结果见 `docs/VERIFICATION.md`。
