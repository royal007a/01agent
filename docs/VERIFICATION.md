# 验证记录（2026-09-23）

环境：macOS arm64，Go 1.26.4；模型为火山方舟 Coding Plan `doubao-seed-evolving`，分别走 OpenAI 兼容协议（`/api/coding/v3`，openai-go/v3）和 Anthropic 兼容协议（`/api/coding`，anthropic-sdk-go）。

## 1. 离线测试（`make test`）

9 个包全部通过：`context engine feishu httpapi observability permission provider textutil tools`。
其中值得一提的集成测试：

| 测试 | 验证点 |
|---|---|
| `provider.TestOpenAITranslation / TestClaudeTranslation` | 用 httptest 抓取真实请求体：assistant 带 tool_calls 时 content 显式为 `""`（1214 修复）；Thinking 阶段不挂 tools 且工具历史降级为文本；Claude 并行 tool_result 合并；Usage 透传 |
| `tools.TestBashTool` | `sleep 30 & echo started` 在 5 秒内返回（第 13 讲 `./server &` 卡死问题）；超时强杀；`command not found` 以 IsError 回传 |
| `tools.TestFuzzyReplaceLevels` | L1–L4 各级命中；L4 丢缩进时自动对齐到原基础缩进；多处命中拒绝替换 |
| `engine.TestExecuteToolsParallelAndOrdered` | 3 个只读工具并发（<700ms），含写工具的批次串行（≥850ms），结果按原顺序 |
| `engine.TestDoomLoopBreaker` | 同一失败动作：3 次真实执行 → 1 条 Reminder → 第 4 次被熔断 → 模型收手 |
| `engine.TestSubagentIsolationAndReadOnly` | 两个子智能体并行，主会话只有 5 条消息；子智能体 `echo > file` 被只读守卫拦截 |
| `feishu.TestWebhookFlow` | 经官方 SDK dispatcher：URL challenge、`@_user_1` 剥离、重复事件去重、按 chat 路由 |
| `feishu.TestFeishuApprovalEndToEnd` | 引擎 + 权限中间件 + 飞书 bot：拒绝时文件保留、批准后真正执行、其他群 approve 被拒 |
| `httpapi.TestChatWithHTTPApproval` | HTTP 审批全链路 + Token 校验 + 跨会话审批被拒 + 用量统计 |

## 2. 真实模型逐讲复现（`make live`，`internal/e2e`）

| 讲 | 场景（与课程一致） | OpenAI 协议 | Anthropic 协议 | 备注 |
|---|---|---|---|---|
| 03/04 | 查北京天气，慢思考开/关 | PASS | PASS | 修复前 doubao 在 Phase 1 输出 `<seed:tool_call>` 伪调用、Phase 2 不再调工具；加临时 nudge 后两种协议 4 个组合全部正常 |
| 05 | read_file 读 hello.txt 并说出暗号 | PASS | PASS | |
| 06 | go version → 写 helloworld.go → 编译运行 | PASS | PASS | 模型把 bash+write_file 放在同一批，引擎按"涉写串行"顺序执行 |
| 07 | 替换带缩进的 if 块 | PASS | PASS | |
| 08 | 一次性并行读 a/b/c.txt（开慢思考） | PASS | PASS | 日志出现"本批 3 个工具均为只读，Fork 并发执行" |
| 10 | AGENTS.md + git-workflow Skill：写 ping.go 并提交 | PASS | PASS | 提交信息 `🎉 feat: 新增 HTTP ping 接口` / `✨ feat: 新增 HTTP ping 接口`；先执行了 git status |
| 11 | 两会话并发 + Working Memory=6 | PASS | PASS | A 读到 token_12345，B 看不到；刷 12 条闲聊后 A 说不记得 |
| 12 | 读 2000 行日志触发 Compactor（水位 3000） | PASS | PASS | 压缩后任务继续执行到 date；Session 全量数据没有被改写 |
| 13 | Plan Mode：跑 6 次工具后"杀进程"，新会话同样指令续传 | PASS | PASS | 第二次运行读取 TODO.md 从断点继续，没有覆盖 PLAN.md，最终打勾并 go build 通过 |
| 14 | 诱导错误 old_text，看模型如何自愈 | PASS | PASS | 第一次 edit 失败 → 带救援指南的 Observation → read_file → 再 edit 成功 |
| 15 | 诱导原样重试 5 次读不存在文件 | PASS | PASS | 首次运行发现：Reminder 已注入，但模型仍按用户要求重试到第 5 次。加熔断后：真实执行 3 次，第 4 次被熔断，模型停止并汇报 |
| 16 | `rm -rf logs` 高危命令：人工拒绝 / 批准 | PASS | PASS | 拒绝时 logs 保留，批准后删除 |
| 17 | 派子智能体找核心密码，主 Agent 写 answer.txt | PASS | PASS | 子智能体轨迹只出现在 Reporter（`[Subagent] bash`），没有进入主 Session |
| 18 | CostTracker 账单 | PASS | PASS | 例：input 1546 tk / output 111 tk / ¥0.002125，LLM 2 次 5.9s，工具 1 次 15ms |

第 09 讲（飞书）：本机没有专用的飞书应用凭据，因此没有接真实飞书。事件分发、Reporter 输出、审批口令用官方 SDK 的 dispatcher 离线测试覆盖。长连接模式下只需在 `~/.go-tiny-claw/serve.env` 填入凭据即可启用。

## 3. 本地部署验证

`make deploy` 后，launchd 服务 `com.weberzhao.go-tiny-claw` 常驻在 `127.0.0.1:48080`：

1. `POST /v1/chat`：查看 go 版本 → 写 `hello/main.go` → `go run` 输出 `Hello deployed claw`（14s，¥0.0065）。
2. `rm -rf hello`：出现在 `GET /v1/approvals`；用其他 session 批准返回 409；用本 session 拒绝后，模型停止，目录保留。
3. `launchctl kickstart -k` 重启服务后，日志显示"从磁盘恢复会话 http_deploy-smoke：12 条消息"。同一 session 追问，模型能准确复述此前的 `go run` 输出和被拒绝的删除操作。
4. `/healthz`、`/metrics` 均正常。

## 4. 独立 review 修复（第一轮）

独立 review 共发现 7 个缺陷，均已修复，并补了回归测试：

1. 子智能体注册表缺少权限中间件，只读守卫可被 `kubectl`、`base64|bash`、`sed -Ei`、`python -c` 等绕过。修复：子智能体也挂 `permission.Middleware`，审批转发给父 Reporter，同时扩充守卫黑名单（`TestReadOnlyGuardRegressions`）。
2. 模型可能用 bash 调用本机的 `/v1/approve` 给自己审批。修复：
   - bash 子进程的环境变量去掉密钥和 Token；
   - 审批 scope 加渠道前缀（`http:` / `feishu:`），无法跨渠道冒充；
   - `/v1/approvals` 必须指定 session；
   - 默认 deny 规则禁止 bash 访问 `/v1/approve`。
3. 长选项绕过：`rm --recursive --force /` 以前是 allow。现改为 deny，同时新增 `find -delete`、`tee x.go`、管道到 sh 等规则（`TestReviewBypassRegressions`）。
4. 工具执行中途进程重启后，历史里留下"有调用、没结果"的记录，之后每次请求都会被 API 拒绝。修复：每轮发送前执行 `RepairToolPairs`，给缺结果的调用补占位结果、丢弃孤儿结果；HTTP 新增 `POST /v1/sessions/{id}/reset`。
5. 会话 `a/b` 与 `a_b` 共用同一个 JSONL 文件。修复：文件名追加 ID 的哈希。
6. 文件工具可以经符号链接写到工作区外。修复：解析最深的已存在祖先目录的真实路径后，再做一次边界校验（`TestSymlinkEscape`）。
7. 审批超时与人工批准竞争时，结果可能不一致。修复：Resolve 持锁发送；Wait 在超时或取消时，先在锁内注销，再做一次非阻塞接收，以人类的决定为准。

另外：飞书 `/reset` 改为只能在会话空闲时执行；只有"approve/reject + 6 位任务 ID"才会被当作审批口令。

## 5. 已知限制

- 权限规则基于正则，不理解 shell 引号：`grep -r 'rm -rf' docs` 也会触发审批，属于误报，没有漏放。
- 审批只能防模型"犯错"，防不住同机上蓄意对抗的模型：YOLO 模式下的 bash 本来就能直接调用系统命令。真正的隔离需要沙箱或容器（课程第 16 讲的观点）。
- 只读守卫同样靠正则识别写操作，只用来缩小子智能体的爆炸半径，不能当作安全沙箱。本地 YOLO 场景应配合容器或沙箱使用（课程第 16 讲的观点）。
- Session 的累计成本只保存在内存里，服务重启后清零；历史消息会持久化。
- HTTP `/v1/chat` 是同步接口：客户端断开会取消这次任务。
- 真机测试中，模型有时会自己用 `nohup` 拉起服务来自测（例如 8080 端口的 ping 服务），e2e 用例在结束时会清理这些进程。
