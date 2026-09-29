# 动态知识更新：消除"跨会话知识丢失"坏味道

## 问题
进度有 progress（本项目中是 PLAN.md/TODO.md），成果有交付物。但执行中核对出的新事实、用户在对话里给出的判断与决策，既不属于进度也不属于交付物，没有同步渠道，会话一结束就随上下文消失。
表现为：用户裁决过的事，换一个任务场景又被违背；同类返工反复发生。

## 三类知识
| 类型 | 含义 | 必填 | 性质 |
|---|---|---|---|
| 用户统筹 user_triage | 用户表达的优先级与偏好 | 内容、适用场景；来源固定为 user | 倾向，不是定论 |
| 发现 discovery | 模型执行中自己核对出的事实 | 内容、来源（文件/命令/工具）；时间自动记录 | 可能随代码变化过期 |
| 决策 decision | 模型自主做出，或用户裁决的结论 | 结论、理由、适用范围、来源（user/model）；可标 rule=全局约束 | 最需要同步 |

## 重构手法：内层记录，外层归位
- **内层循环（每次任务执行）**：模型给出最终答复前，Harness 注入一次"交付前知识清点"提醒，模型按三类调用 `record_knowledge`；本轮没有新知识也要调用一次 `kind=none`。内层只写会话暂存区（Harness 状态目录，工作区之外），**不需要知道知识最终住在哪里**。
- **外层循环（Harness 编排层）**：Run 结束后，`knowledge.Placer` 按工作区知识布局统一归位：
  - 决策 → ADR 文件（已有 `docs/adr` 时写到那里，否则写到 `.claw/knowledge/decisions/`）；rule=true 的决策同时进入 `rules.md`；
  - 用户统筹 → `preferences.md`；发现 → `discoveries.md`；
  - 结构化索引 `index.json` 作为唯一事实来源，markdown 文件都由它渲染；
  - 完全相同的条目去重；同主题的新条目取代旧条目，旧 ADR 标注"已被 ADR-000N 取代"，不删除；
  - 布局可以用 `.claw/knowledge.json` 覆盖；所有写入经 `os.Root`，不会写出工作区；索引损坏时拒绝覆盖，暂存条目保留，下次重试。
- **闭环**：`PromptComposer` 把沉淀的知识注入后续会话的 System Prompt，规则置顶，然后是已做决策、用户偏好（明确标注为倾向）、近期发现，总量不超过 4KB。

## 何时触发
- 事前判据：只要内层循环存在用户交互就触发。go-tiny-claw 的每次 `Chat` 都来自用户输入，因此默认对每次唤醒都做清点，每次唤醒最多提醒一次。
- 事后信号：跨任务出现不一致（用户说过不要用某库，换个任务又引入）。离线回归 `TestDynamicKnowledgeAcrossSessions` 与真机测试 `TestKnowledge_CrossSession` 就是按这个信号设计的。
- 一次性跑分（Benchmark）默认关闭（`TestCase.knowledge=false`），省一轮交互。

## 验证
- 离线：`internal/knowledge`（按类型校验、暂存区失败重试、布局与越界、去重与取代、索引损坏拒写、决策高于偏好）；`engine.TestDynamicKnowledgeAcrossSessions`（会话 1 裁决 → 外层归位 → 会话 2 的 System Prompt 带上规则；清点每次唤醒恰好一次）；`go test -race ./...` 全绿。
- 真实模型（本地 Ollama qwen3:8b，云端额度耗尽期间的替代）：`TestKnowledge_CrossSession` 通过。会话 1 中，模型在清点提醒后记录了"日志库选型"决策（source=user）和一条发现；外层归位生成 ADR-0001，并写入规则文件。会话 2 是新会话，写 server.go 时沿用了 slog，没有引入 logrus/zap。
- 真机暴露的问题及修复：模型把同一件事同时记成了决策和偏好 → 规则改为"决策高于偏好"：同主题有生效决策时，偏好不再单独入库；新决策会取代同主题偏好。

## 代码位置
- `internal/knowledge/{types,inbox,placer,manager}.go`
- `internal/tools/knowledge.go`（`record_knowledge`）
- `internal/engine/loop.go`：交付前清点提醒 + `settleKnowledge`
- `internal/context/composer.go`：注入知识
- `internal/app/app.go`：装配（`Options.NoKnowledge` 可关闭）

## 已知限制
- 主题键由模型给出。主题写得不一致（例如"日志库"和"日志框架选型"）时不会触发取代，只会并存；P1 可以加入主题归一化或近似匹配。
- 模型仍然可以用 write_file 直接改 `.claw/knowledge/` 下的文件。markdown 由索引重新渲染，会被覆盖，但索引本身没有做防篡改；需要时可以把索引移到 Harness 状态目录。
- "偏好与规则冲突时以规则为准"目前只写在提示词里，没有做程序化的冲突检测。
