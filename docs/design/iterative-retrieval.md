# 迭代式检索 / Agentic RAG 适配 go-tiny-claw：Review 与设计结论

> 基线：`~/code/go-tiny-claw` HEAD=`c6fc993`。本文只做设计，不改代码。
> 输入：用户给出的"迭代式检索"要点，以及 mymaccodex 的交接（证据账本、缺口驱动 Query、去重/冲突/新颖度排序、效果/资源双停止、接入 trace/replay 与 Evaluator）。

## 0. 结论先行

1. **不改 Main Loop**：检索做成只读工具 + 独立的 `internal/retrieval` 包，和第 05/06 讲"能力都是 Registry 里的工具"的原则一致。多轮"检索—评估—补充检索"放进**子智能体**（第 17 讲）里跑，海量召回结果不会污染主上下文。
2. **分工：Query 由模型提，记账与停止由 Harness 管**。下一轮查什么由模型决定（Agentic）；去重、新颖度、冲突、覆盖率和停止条件由 Harness 确定性地计算，并以工具结果的形式反馈给模型，预算类条件由 Harness 硬性熔断。这样模型不会"自己觉得还能再查"，也不需要把搜索逻辑硬编码进 Loop。
3. **证据账本必须外部化**（第 13 讲 PLAN/TODO 的同一哲学）：落盘到 Harness 状态目录，按（会话，检索任务）隔离（§3.4）。账本是事实来源，工具输出只是它的视图。这是与 Compactor 兼容的前提，详见 §6.1。
4. **P0 只做确定性能力**：
   - BM25 关键词检索 + rg 两路召回，用 RRF 融合；
   - 精确去重与近似去重，新颖度衰减；
   - 查询去重、双类停止条件、Trace 埋点；
   - 离线指标，基于 qrels 计算，不依赖大模型。

   向量召回放 P1：本机 Ollama 已经有 `bge-m3`，`/api/embed` 实测可用。冲突裁决、replay 也放 P1，图检索放 P2。

## 1. 现有能力 → 落点对照

| 要点 | 现有能力（文件） | 缺口 | 落点 |
|---|---|---|---|
| 检索入口 | 只读工具 + 并发执行（`tools/registry.go` 的 `ReadOnlyTool`，`engine/loop.go: executeToolsWith`）；read_file/bash(grep) 已能"检索" | 没有排序、去重、账本，每轮结果只是互不相关的字符串 | 新工具 `search`、`evidence`（§3） |
| 多轮循环、上下文隔离 | 子智能体 `RunSub`（最多 10 轮，只读注册表 + 白名单 bash） | 子智能体结束时只返回一段文本，证据结构丢失 | 子智能体注册表挂 `search`/`evidence`；报告附带账本摘要和账本路径 |
| 不重复 Query | `engine/reminder.go: Fingerprint` 的参数规范化（键排序、空白归一、路径 Clean） | 只识别"失败的重复"；"成功但没有新信息"的重复识别不了 | 复用 `normalize` 生成 Query 指纹，在账本层拒绝或标注重复 |
| 记录每轮状态 | Trace 的 Span 树（`internal/trace`），Session JSONL | Trace 只保存 200 字预览，不能回放 | `Retrieval.Round` Span + 账本（完整）+ P1 cassette（回放） |
| 停止条件 | `MaxTurns`、熔断、防早退、子智能体 10 轮上限 | 没有"证据足够""连续无新增""重复率高"这类效果条件 | `retrieval.StopEvaluator`（§2.3） |
| 评测 | `internal/eval`：判卷脚本 + 轨迹指标 + 基线回归 | 没有 qrels、没有排序指标 | `retrieval/metrics.go` + `bench/retrieval/*.json`（§4） |
| 规则外部化 | Skills（第 10 讲） | 没有检索 SOP | `.claw/skills/iterative-retrieval/SKILL.md` 写入五种追问策略（§5） |

## 2. 数据结构与状态机

### 2.1 核心类型（`internal/retrieval/types.go`）

```go
type Chunk struct {
    ID      string    // 稳定 ID：source + path + 起止行 + 内容哈希
    Source  string    // "workspace" | "grep" | "vector:bge-m3" | 外部源名
    Path    string
    Lines   [2]int
    Text    string
    Version string    // 文件 mtime / git blob / 文档版本号，用于冲突裁决
    Trust   float64   // 数据源可信度 0..1（配置给出）
    Sig     uint64    // SimHash，用于近似去重
}

type Hit struct {
    ChunkID   string
    Retriever string  // keyword | grep | vector
    RawScore  float64
    Rank      int     // 在该路召回里的名次（RRF 用）
}

type Evidence struct {
    ID          string
    ChunkID     string
    Snippet     string   // 截断后的证据正文（账本里保存完整片段，发给模型时截断）
    Round       int      // 首次采纳的轮次
    GapIDs      []string // 它补上了哪些缺口
    Novelty     float64  // 采纳时的新颖度 = 1 - 与已采纳证据的最大相似度
    Score       float64  // 融合后得分
    Superseded  string   // 被哪个更新版本或更高可信度的证据取代（冲突裁决）
}

type Gap struct {
    ID        string
    Question  string      // "Average 的空切片行为是什么？"
    Status    GapStatus   // open | covered | unanswerable | needs_user
    CoveredBy []string    // Evidence IDs
}

type Fact struct { Text string; EvidenceIDs []string; Round int }

type Round struct {
    N           int
    Query       string
    QueryFP     string    // 规范化指纹，用于拒绝重复 Query
    Strategy    Strategy  // new_entity | constraint | new_term | verify | counter
    TargetGap   string
    Sources     []string
    Retrievers  []string
    Filters     map[string]string // path_glob / since / version 等结构化条件
    HitIDs      []string          // 本轮召回（去重前）
    NewEvidence []string          // 本轮新增有效证据
    DupCount    int               // 精确 + 近似重复条数
    Conflicts   []string
    Decision    Decision          // continue | stop
    StopReason  StopReason
    Cost        Cost              // 耗时、工具调用次数、Token（来自 CostTracker）
}

type RetrievalState struct {
    Question string
    Gaps     []Gap
    Evidence map[string]*Evidence
    Facts    []Fact
    Rounds   []Round
    Budget   Budget   // MaxRounds=3, MaxToolCalls, MaxDuration, MaxTokens
    Status   string   // running | done
}
```

用户要求"每轮至少记录"的七项，全部对应到 `Round` 与 `RetrievalState` 的字段：Query、数据源与检索方式、召回结果、新增证据、已确认事实、未覆盖缺口、继续或停止的原因。

### 2.2 状态机

```
        ┌──────────────── continue ────────────────┐
        ▼                                          │
Init ─► PlanQuery ─► Retrieve ─► Merge ─► Assess ─► Decide ─► stop ─► Report
 │        │ 拒绝重复Query      多路召回   去重/融合   缺口覆盖    StopEvaluator
 │        │ 必须声明策略+目标缺口          冲突/新颖度  新增证据
 └ 由问题拆出初始 Gaps（模型通过 evidence.add_gap 声明，Harness 至少要求 1 个）
```

- **PlanQuery**（模型负责，Harness 校验）：`search` 必须带 `strategy` 和 `gap_id`。规范化后的 `QueryFP`（Query + 过滤条件 + 数据源）如果已经出现过，直接返回 `{"rejected":"duplicate_query","hint":"换一个关键词、加结构化条件或查反证"}`，不再实际检索。
- **Merge**（Harness 负责）：
  1. 硬条件过滤：path_glob、since、version；
  2. 精确去重：按 `Chunk.ID` 与内容哈希；
  3. 近似去重：SimHash 汉明距离 ≤ 3，或 5-gram Jaccard ≥ 0.8，判为同一内容，只保留可信度与版本更高的一条；
  4. 融合打分：
     `score = RRF(k=60, 各路名次) × Trust × Novelty × VersionFactor × GapWeight`
     - `Novelty = 1 − max sim(chunk, 已采纳证据)`：已被覆盖的同类信息价值自动下降；
     - `GapWeight = 1/(1 + 目标缺口已有证据数)`：缺口覆盖越多，同类证据越不值钱。
- **Assess**：新颖度 ≥ τ（默认 0.3）的 Top-K 才采纳为 Evidence。同一路径的不同版本、或内容互相矛盾时：新版本优先，同版本比较可信度；被取代的证据在 `Superseded` 里记录原因，**不静默丢弃**。
- **Decide**：由 `StopEvaluator` 给出结论，写进 `Round.StopReason`，并在工具结果里告诉模型。

### 2.3 StopReason 与判定顺序（先资源、后效果）

| StopReason | 判定 | 类别 |
|---|---|---|
| `budget_rounds` | 已达 `MaxRounds`（默认 3） | 资源 |
| `budget_tool_calls` / `budget_time` / `budget_tokens` | 超出预算（Token 取自 CostTracker 当前会话） | 资源 |
| `sources_exhausted` | 所有数据源、所有检索方式都已查过 | 资源 |
| `gaps_covered` | 所有 Gap 都是 covered/unanswerable | 效果 |
| `sufficient` | 模型调用 `evidence.finish` 声明已足够，且 Harness 校验每个 Gap 至少有 1 条证据 | 效果 |
| `no_new_info` | 连续 2 轮新增证据 < 2（阈值可配） | 效果 |
| `high_duplication` | 本轮重复率 ≥ 0.8 | 效果 |
| `needs_clarification` | 存在 `needs_user` 类缺口：检索拿不到，只能问用户 | 效果 |

资源类条件由 Harness 硬执行：后续 `search` 直接返回 `stopped`。效果类条件以 `stop_recommended` 的形式返回给模型：可以停，但仍允许模型用新策略再试一轮，因为这可能正是查反证的那一轮。同一个效果条件连续触发两次，就转为硬停，防止无限续查。最终汇报遵循"最小回答原则"：只回答问题本身，并列出可以继续追问的方向。

## 3. 工具与接口（不进 Main Loop）

### 3.1 `retrieval.Retriever`（Provider 式接口）

```go
type Query struct { Text string; Filters map[string]string; K int }
type Retriever interface {
    Name() string                                   // keyword | grep | vector
    Search(ctx context.Context, q Query) ([]Hit, []Chunk, error)
}
```

- **P0 `KeywordRetriever`**：纯 Go 实现，工作区文本文件按段落/函数切块，中文用 bigram、英文用单词做 BM25，倒排索引按文件 mtime 增量更新，缓存在 `<CLAW_HOME>/retrieval/index/<工作区哈希>`（与账本同样放在工作区之外）。
- **P0 `GrepRetriever`**：直接用 argv 执行 `rg -n --json <pattern>`，不经过 shell，从根上避开 bash 注入和只读守卫的复杂度。适合精确匹配 ID、错误码、配置键（BM25 在这类 token 上不稳定）。
- **P1 `VectorRetriever`**：调用 Ollama `/api/embed`（bge-m3），向量缓存在本地；语义近义表达由它兜底。
- **P2 `GraphRetriever`**：符号与引用图（Go 的 import/调用关系），用于"跨文件找调用链"类问题。

多源召回与多路召回走同一条流水线：统一成 Chunk → 硬过滤 → 去重 → RRF 融合 → 业务规则调整（Trust/Version）。

### 3.2 工具定义

| 工具 | 参数 | 返回 | 读写 |
|---|---|---|---|
| `search` | `query, strategy, gap_id, sources?, mode(keyword\|grep\|vector\|hybrid), filters?, k?` | 本轮**新增**证据（ID、路径、行号、截断片段、得分、新颖度）；重复、被取代的条数；覆盖状态（已确认事实数、未覆盖缺口）；`stop_recommended/stopped` 及原因 | 不碰用户文件，但会写账本 → **`ReadOnly()=false`**（见 §3.4） |
| `evidence` | `action: add_gap \| mark_fact \| mark_unanswerable \| status \| finish` | 账本摘要 | 同上，`ReadOnly()=false` |

关键点：`search` **只返回增量**，已经看过的内容不会再回灌给模型。这是"合并而不是覆盖、排序看剩余信息量"在上下文层面的落实，也天然减轻了 Compactor 的压力。

### 3.3 接入方式

- `app.Build`：主注册表与子智能体只读注册表都挂上这两个工具。工具构造时**只注入 `LedgerStore`，不绑定任何会话**，账本在运行时按 §3.4 解析。
- 子智能体提示词加一行："涉及资料检索时使用 search/evidence，按 iterative-retrieval Skill 执行"。
- Trace：每次 `search` 在 `Tool.Execute` 下产生一个 `Retrieval.Round` 子 Span。属性包括 query、strategy、gap、sources、hits、new_evidence、dup_rate、novelty_gain、stop_reason，但只记录 ID 和计数，不记录正文，控制 Trace 体积。

### 3.4 账本的会话绑定与并发语义（codex review 补充，实现前必须满足）

**会话绑定**：`app.Build` 按工作区组装 Registry，而 Session 要到运行时才进入 ctx。如果在构造工具时捕获单一会话，同一工作区的多个会话（多个飞书群、多个 HTTP session）就会串账。规则如下：
- `search`/`evidence` 每次执行都从 `runctx.Session(ctx)` 取会话 ID。ctx 里没有会话时直接报错拒绝，不回退到任何全局账本。
- 账本 key 为（会话 ID, 检索任务 ID）。检索任务 ID 由 `spawn_subagent` 在 `RunSub` 入口生成，写入 ctx（新增 `runctx.WithRetrievalTask`）；主 Agent 直接调用时，任务 ID 固定为 `main`。并行派出的多个子智能体因此各有独立的 `RetrievalState`，互不串轮次。
- 账本路径：`<CLAW_HOME>/retrieval/<fileNameFor(sessionID)>/<task>.json`。`fileNameFor` 复用 Session 持久化里的"可读前缀 + 哈希"，避免 `a/b` 与 `a_b` 这类 ID 冲突。账本放在 **Harness 状态目录，而不是工作区**：模型用 write_file 或 bash 改不了账本，只读子智能体也不会因此在工作区产生写操作。
- `LedgerStore` 内部是 `map[key]*ledger`，打开和创建受全局锁保护；每个 ledger 各自持有 `sync.Mutex`，读、改、落盘都在锁内完成，落盘采用"临时文件 + rename"的原子写。

**硬约束（codex 复核补充）**：
- ctx 里没有 Session 或检索任务 ID 时，**硬拒绝**：返回工具错误，不做任何回退。
- `LedgerStore` 读写或落盘失败时，**必须返回工具错误**（IsError=true）：不能出现"模型看到新增证据、账本里却没有"的情况，否则回放、评测与模型视图会不一致。先落盘成功，再组装返回内容。

**并发与顺序**：
- P0 两个工具都声明 `ReadOnly()=false`。按现有引擎规则，只要一批调用里含有它们，这一批就退化为按模型给出的顺序串行执行，账本变更顺序就是确定的。这里用并行吞吐换取可回放性。
- 轮次编号在 ledger 锁内分配，单调递增；`Round` 同时记录 `ToolCallID`，Trace 与账本可以一一对应。
- P1 如果要做并行 search，改为两段式：召回、打分这些纯计算阶段并行、不加锁；提交阶段在 ledger 锁内按 `ToolCallID` 在本批中的下标顺序串行合并。这需要引擎把"批内下标"放进 ctx，因此列为 P1。

**权限与审批语义**：
- 账本写入属于 Harness 内部状态，不经过 permission Middleware 审批，因为它不产生外部副作用。但它受 Harness 自身校验约束：空 Query 或无效 Query、未知 gap_id、超预算、Query 重复，都在执行前拒绝，并返回可操作的错误，让 RecoveryManager 能匹配。
- `evidence.finish` 不能跳过校验：如果还有未覆盖的 Gap，就拒绝，并列出缺口；只有显式标注 `mark_unanswerable` 或 `needs_user` 的缺口才能跳过。
- `GrepRetriever` 执行 argv 时同样受工作区边界约束：`rg` 的 cwd 固定为工作区，路径参数经 `os.Root`/`workspace()` 校验，不允许 `../` 越界检索。

## 4. 评测指标（`internal/retrieval/metrics.go`，纯函数）

数据集格式放在 `bench/retrieval/*.json`：

```json
{"id":"r01","corpus":"testdata/corpus_a","question":"...","gaps":["..."],
 "qrels":{"chunkA":2,"chunkB":1},"gold_facts":["..."],"max_rounds":3}
```

| 指标 | 定义 |
|---|---|
| Recall@K | 前 K 条中相关结果数 ÷ 相关结果总数 |
| Precision@K | 前 K 条中相关结果数 ÷ K |
| MRR | 第一个相关结果名次的倒数，对所有问题取平均 |
| NDCG@K | 按 qrels 的相关度分级（0/1/2）计算 DCG，再除以理想排序的 DCG |
| 证据覆盖率 | 被覆盖的 Gap 数 ÷ Gap 总数；也可以用"命中 gold_facts 的比例" |
| 重复率 | 被判为精确或近似重复的召回条数 ÷ 总召回条数（按轮统计，也可汇总） |
| 增量信息量 | 每轮新增有效证据数；另记"每单位成本的新增证据"（每次工具调用、每千 Token） |
| 检索成本 | 轮数、工具调用次数、耗时、Token（来自 Trace 与 CostTracker） |
| 编辑修改成本 | 把返回的排序列表改成理想列表的最小代价：漏召回（插入）c=1.0，错误召回（删除）c=0.5，重复（删除）c=0.3，排序错位（相邻交换，按 Kendall 距离计）c=0.2；除以理想列表长度做归一化 |

分两层评测：
- **离线层**：对固定语料和固定 Query 序列计算排序指标，结果确定，可以做回归门禁。
- **端到端层**：扩展现有 `eval.Result`，增加 `Retrieval *RetrievalMetrics` 字段，从账本读取；报告增加几列。基线回归沿用 exit 2 机制，并新增"覆盖率下降"或"重复率上升超过阈值"两类回归条件。

## 5. P0/P1 与验收

**P0**（约 1–2 天，全部可以离线验证）：

1. `internal/retrieval`：
   - 类型与账本（落盘、加锁、按会话隔离）；
   - Chunker、BM25、rg 两路召回，RRF 融合；
   - 精确去重与近似去重，Novelty 和 GapWeight 两个衰减因子；
   - Query 指纹去重、StopEvaluator、metrics。
2. `tools/search.go`、`tools/evidence.go`，由 `app.Build` 挂载，子智能体可用。
3. Trace 增加 `Retrieval.Round` Span。
4. Skill：`deploy/skills/iterative-retrieval/SKILL.md`，写明五种追问策略（新实体、结构化条件、新术语、验证假设、查反证），以及"最小回答 + 引导追问"。

**P1**：
- 向量检索（bge-m3）；
- 版本与可信度冲突裁决的完整规则，外部数据源 Schema；
- **Cassette 回放**：按（检索方式、规范化 Query、过滤条件、语料版本哈希）录制和回放召回结果，让端到端评测可重复；
- `eval` 集成检索指标；Compactor 感知（§6.1）。

**P2**：
- 图检索；
- 模型驱动的数据源路由；
- 用模型判断"证据是否足够"（作为 `sufficient` 的补充信号，不替代 Gap 校验）。

**回归与验收用例**（P0 全部必须）：

| 用例 | 验证点 |
|---|---|
| `TestQueryRepeatRejected` | 同 Query 仅空白、大小写、键序不同 → 拒绝；过滤条件不同 → 放行 |
| `TestDedupExactAndNear` | 同一块内容重复召回计为 dup；改动一个字的近似块被合并，保留高可信度版本 |
| `TestNoveltyDecay` | 同一缺口第 3 条同类证据的得分，低于另一缺口的第 1 条证据 |
| `TestConflictVersionTrust` | 新旧版本冲突时采纳新版本，旧版本记录 `Superseded`，不丢弃 |
| `TestStopEvaluator` | 表驱动覆盖 §2.3 全部 StopReason，并验证"资源先于效果"的判定顺序、"效果条件连续两次转为硬停" |
| `TestLedgerSurvivesCompaction` | 让早期的 search 结果被 Compactor 掩码，`evidence.status` 仍能给出完整事实与缺口 |
| `TestMetricsGolden` | 固定排序列表与 qrels → Recall/Precision/MRR/NDCG/重复率/编辑成本与手算值一致 |
| `TestSearchParallelSafe` | 两个并发 search 写同一账本，`-race` 不报竞争，轮次编号唯一 |
| `TestLedgerSessionIsolation` | 同一工作区的两个会话（及同会话的两个并行子智能体任务）各自记账，互不串轮次；ctx 中没有会话时直接报错 |
| `TestBatchSerializedWithLedgerTools` | 一批调用中同时含 search 与 evidence 时按模型顺序串行执行，账本轮次与 ToolCallID 顺序一致 |
| `TestLedgerWriteFailureIsToolError` | 账本目录不可写时，search/evidence 返回 IsError，且不返回任何"新增证据" |
| `TestFinishRequiresCoverage` | 存在未覆盖 Gap 时 `evidence.finish` 被拒绝，并列出缺口；标注 unanswerable 后才放行 |
| `TestRetrievalTraceAttrs` | Span 属性齐全，且不含正文 |
| Bench `r01_multi_hop` | 第 1 轮必然出现一个新术语，第 2 轮必须用它才能找到答案；判卷检查答案正确、轮数 ≤ 3、覆盖率 = 1 |
| Bench `r02_counter_evidence` | 语料里有一个过期结论和一个新版勘误；判卷要求采纳新版，并说明冲突 |

## 6. 兼容性检查与设计取舍

### 6.1 Context Compaction（第 12 讲）：主要冲突

证据如果只存在于工具结果里，离开 Working Memory 后会被掩码成"早期工具输出已被清理"，模型也就丢了已确认的事实。解决办法：
- 账本才是事实来源；
- `search` 只返回增量；
- 每轮请求前追加一条**临时的"检索状态"消息**，内容是已确认事实、未覆盖缺口、剩余预算，控制在 1.5KB 以内。它的注入方式和第 03 讲的 thinking nudge 一样：不写入 Session，所以不会被压缩，也不会膨胀。

取舍：每轮会多出一段固定开销，但省下了反复重查的成本。

### 6.2 Trace 与回放

Trace 现在只有预览，不能回放。回放用单独的 cassette 实现，不把完整召回塞进 Trace，以免 Trace 膨胀（第 19 讲思考题的担忧）。Trace 与 cassette 通过 `Round.QueryFP` 关联。

### 6.3 Evaluator（第 20 讲）

现有判卷机制不变，检索指标是附加信息。离线排序指标不需要模型，可以放进 `make test`；端到端检索题照常跑 `make bench`。本机方舟额度的问题会影响端到端题，但不影响 P0 验收。

### 6.4 Reminder 与熔断（第 15 讲）

"成功但没有新信息"的重复不是 IsError，现有 Reminder 管不到，应由账本层的 Query 去重和 `no_new_info` 停止条件负责，两者不重叠。熔断指纹已包含全部参数，不同过滤条件的查询不会被误熔断。

### 6.5 权限与只读守卫

`GrepRetriever` 直接执行 argv，不经过 bash，不受白名单守卫影响，也不引入新的注入面。账本改放 Harness 状态目录（§3.4），不产生工作区副作用，因此可以挂进只读子智能体。但它会修改状态，所以 **`ReadOnly()=false`**，同批调用串行执行，避免竞态（原稿写的是 `true`，已按 codex review 修正）。

### 6.6 AGENTS.md / spec

仓库没有 AGENTS.md，不需要修改。检索 SOP 用 Skill 外置（第 10 讲），不写进极简内核。需要更新的文档是 `docs/IMPLEMENTATION.md`（新增"扩展：迭代式检索"一节）和 `docs/VERIFICATION.md`（验收记录）。

### 6.7 取舍记录

| 分歧 | 选择 | 理由 |
|---|---|---|
| 模型驱动还是 Harness 驱动 | 混合：模型提 Query，Harness 记账并决定停止 | 课程主张 Loop 极简、由模型决策；"查得够不够"必须可度量、可熔断，只能由 Harness 负责 |
| 检索写进 Main Loop 还是做成工具 | 工具 + 子智能体 | 不增加 Loop 分支；多轮噪声留在子上下文 |
| P0 是否上向量检索 | 否（P1） | P0 追求确定性、可回归；bge-m3 已就绪，P1 只是接一个 Retriever 实现 |
| 近似去重用 SimHash 还是 embedding | P0 用 SimHash/Jaccard，P1 可换成 embedding 相似度 | 不依赖模型，测试稳定 |

---

# 第二轮：Agentic RAG 边界、最小充分检索、端到端性能、预算动态检索与缓存

> 输入：用户第二段材料（Agentic RAG 与迭代式检索的关系、端到端检索性能、最小充分检索、基于预算的动态检索、缓存）以及流程图，外加 mymaccodex 的交接。
> 流程图：生成 query → 多源多路召回 → 过滤/融合/排序 → 信息是否充分？是 → 退出；否 → 资源是否不足且已达最小充分？是 → 退出；否 → 检索缺失信息，回到生成 query。
> 说明：第一轮设计稿已提交（`db7e16c`），本节在其基础上增量修订；与第一轮冲突的地方在 §12 列出。

## 7. 边界：迭代式检索（机制） vs Agentic RAG（系统编排）

| 能力 | 归属 | 在 go-tiny-claw 中的位置 | 本期 |
|---|---|---|---|
| 根据上一轮结果补充检索、证据账本、停止判定 | 迭代式检索 | `internal/retrieval`（账本、StopEvaluator），`search`/`evidence` 工具 | P0 |
| 决定查什么（Query 改写、问题拆解） | Agentic RAG | 模型负责；Harness 用 strategy/gap 校验、拒绝重复 Query | P0（校验）|
| 决定去哪查（数据源、检索方式的选择） | Agentic RAG | 数据源目录 `SourceCatalog`（名称、描述、成本档位、可信度）作为工具描述暴露给模型；模型选择，Harness 按预算截断（§10） | P0 两个源 / P1 扩展 |
| 调用什么工具、怎么验证证据 | Agentic RAG | 已有：Main Loop + Registry + 子智能体；验证用 `verify`/`counter` 策略加冲突裁决 | 已有 + P0 |
| 什么时候停 | 两者共有 | 最小充分 gate（§8）+ 预算阶梯（§10）+ StopEvaluator | P0 |
| 多 Agent 协作规划、外部 API 数据源、LLM 充分性裁判 | Agentic RAG | 不进入当前 Harness | P2 |

结论：go-tiny-claw 本身就是 Agent Harness，Agentic 的"自主决策"由现有 Main Loop 和子智能体承担。本期只补两块：一是迭代式检索机制；二是让自主决策**受约束**的数据源目录和预算阶梯。不另建编排框架，保持第 06 讲的极简原则。

## 8. 最小充分检索：Goal → Gap → Finish gate

目标：不要一上来把用户需求里的所有子任务都展开去查。

- **新增 `Goal` 层**：`Goal{ID, Statement, IndependentValue bool, Status: active|deferred|done, GapIDs}`，`Gap` 归属于某个 Goal。
- **拆解规则**：模型调用 `evidence.plan_goals`，一次声明全部子目标，并标注每个子目标是否"单独完成就能给用户有意义的结果"（独立价值判断）。Harness 规定：
  1. 每个检索任务**同时只能有 1 个 active Goal**：取第一个具有独立价值的子目标，其余一律标为 `deferred`；
  2. 每个 Goal 最多 5 个 Gap，超出时拒绝并提示"先收敛到最小目标"；
  3. `search` 的 `gap_id` 只要属于 deferred Goal，就拒绝，返回 `{"rejected":"goal_deferred","hint":"先完成当前最小目标"}`。
- **Finish gate**：active Goal 的全部 Gap 都满足 covered 或 unanswerable，才允许 `evidence.finish`。finish 的返回包括：
  - 最小回答需要的证据清单；
  - 用 deferred Goals 生成的"引导追问"（例如："要继续帮你优化这个项目吗？"）。

  用户确认后，下一次唤醒把下一个 Goal 提为 active，账本里已有的证据可以直接复用（§11）。
- **示例**："分析三个项目哪个最适合放简历，优化它，再生成面试题"：
  - G1 选项目：active；
  - G2 优化：deferred；
  - G3 面试题：deferred。

  本次只为 G1 检索，回答完后引导用户进入 G2。
- **流程图对应**："资源不足 & 最小充分 → 退出"＝预算触发且 active Goal 已满足 finish gate，则直接回答；预算触发但 finish gate 未满足，则按 §10 进入降级阶梯的最后一档（带不确定性说明回答，或向用户澄清），不再继续扩大检索。

## 9. 端到端 SLO 与观测字段

用户感知的是"多久拿到一个靠谱答案、花了多少成本"，而不是某一次向量查询的耗时。所以 SLO 按端到端链路拆分：

| SLO 维度 | 指标 | 来源 |
|---|---|---|
| 首个可用答案耗时 | `ttfa_ms`：从 Chat 开始到最终答复（p50/p95，跑分取整套用例） | Agent.Run Span |
| 检索延迟 | `retrieval_ms`，并拆到阶段：rewrite / embed / recall（按路） / fuse / rerank / assess | Retrieval.Round 子 Span |
| 轮次 | LLM 调用次数、工具调用次数、检索轮数、最高到达的阶梯档位 `tier_max` | Trace 计数 |
| Token/成本 | prompt/completion tokens、cost_cny，另记"检索相关 Token"（search 结果注入量） | CostTracker + Round |
| 证据覆盖率 | active Goal 的 Gap 覆盖率、重复率、每轮新增证据 | 账本 |
| 最终答案质量 | 判卷通过率（validate）、gold_facts 命中率；P2 可加 LLM 裁判分 | eval |
| 复用效率 | cache_hits / cache_misses（按层），以及复用节省的检索耗时 | 缓存层 |

**Trace 字段**：
- `Agent.Run` 新增：`ttfa_ms, retrieval_ms, generation_ms, tier_max, cache_hits, cache_misses, goals_total, goals_done`。
- `Retrieval.Round` 新增：`tier, route_ms.{keyword,grep,vector,rerank}, candidates_in, candidates_out, dup_rate, new_evidence, cache, budget_left.{rounds,ms,tokens}`。

**Evaluator**：`eval.Result` 新增：`TTFAMs, RetrievalMs, TierMax, CacheHits, Coverage, GoalsDone`。用例新增可选的 `slo:{max_ttfa_ms,max_cost_cny,min_coverage,max_tier}` 字段。违反 SLO 记为 `slo_violation`：不改变 pass/fail，但会作为一种回归类型参与基线比较（exit 2）。报告增加 p50/p95 TTFA 两列汇总。

## 10. 基于预算的动态检索：升级与降级阶梯

| 档位 | 内容 | 典型成本 | 进入条件 |
|---|---|---|---|
| T0 复用 | 命中结果缓存或已有账本证据 | ~0 | 总是先试 |
| T1 轻量 | 单路召回：精确 token 走 rg，自然语言走 BM25；k=5 | 毫秒级 | 默认起点 |
| T2 多路 | BM25 + rg 用 RRF 融合；k=10；可加结构化过滤 | 十毫秒级 | T1 评估后新增证据 < 2 或目标 Gap 仍 open |
| T3 语义 | 加 bge-m3 向量召回（P1），再加重排（P2：本地 reranker 或 LLM 重排） | 百毫秒到秒级 + Token | T2 仍不足，且预算余量 ≥ 50% |
| T4 澄清/收敛 | 停止检索：用现有证据给出带不确定性说明的回答，或向用户澄清（`needs_clarification`） | 一次模型调用 | 预算余量 < 20%，或 T3 仍不足，或缺口不可检索 |

- **升级**：由 Harness 依据上一轮的评估结果（新增证据、Gap 状态）决定"允许的最高档位"。模型可以在 `mode` 里请求更高档位，超过允许档位的一律截断到允许档位，并在返回中说明原因。**不允许跳过评估直接升到 T3**。
- **降级**：每轮开始前计算预算余量，取剩余时间、剩余轮次、剩余 Token、剩余可查数据源中最小的那个比例：
  - 余量 < 50%：禁用 T3，k 减半；
  - 余量 < 20%：直接进入 T4。

  这对应"简单问题走轻量路径，证据不足再逐步升级；预算不足则主动降级"。
- 预算来源：
  - 用例的 `budget` / `slo` 字段；
  - 服务端按渠道配置（飞书默认 60s，CLI 默认 180s）；
  - Token 余量从 CostTracker 当前会话实时读取。

## 11. 缓存：分层、键与失效边界

| 层 | 键 | 值 | 失效/版本边界 | 本期 |
|---|---|---|---|---|
| L1 检索结果缓存 | `retriever + QueryFP(规范化 Query+filters+sources) + index_version` | 命中的 Chunk ID 与内容哈希列表 | 命中后**逐个 Chunk 复核**：stat 文件 mtime/size 变化时，重新计算该 Chunk 的哈希，不一致就丢弃该条，而不是整条作废；`index_version` 在切块规则或 BM25 参数变化时递增 | P0 |
| L2 证据复用 | `workspace_id + chunk content hash` | Evidence（片段、来源、版本） | 内容哈希变化即失效。跨会话复用只在同一工作区内，飞书不同群之间**不共享**，避免信息泄漏 | P0（同会话跨轮、跨 Goal）/ P1（跨会话） |
| L3 Embedding 缓存 | `model + chunk content hash` | 向量 | 内容哈希变化或模型变化即失效 | P1 |
| L4 答案缓存 | `规范化问题 + corpus_version + model + prompt_version` | 最终回答与证据 ID | `corpus_version`＝已索引文件 (path, size, mtime) 的哈希，或 git HEAD+dirty 哈希；`prompt_version`＝System Prompt（含 AGENTS.md、Skills）的哈希；另设 TTL。**只对显式标记为"稳定问题"的请求启用**，且该请求不能调用过有副作用的工具 | P1（默认关闭）|

- 存储位置：`<CLAW_HOME>/retrieval/cache/`，和账本一样放在工作区之外，采用原子写。每条记录带 `created_at` 和 `index_version`。
- 可观测：每次命中或未命中都记入 `Retrieval.Round.cache` 和 `Agent.Run.cache_hits`。
- **cache 与 cassette 的区别**：两者共用同一个键函数，但语义相反。cache 是运行时优化，随版本失效；cassette 是回放用的，键严格、不失效、只在 record/replay 模式启用。**replay 模式下关闭运行时 cache**，保证回放只依赖 cassette，结果确定。

## 12. 与第一轮方案的兼容性、冲突与取舍

| 对象 | 影响 | 处理 |
|---|---|---|
| 第一轮 §2.2 "search 必须带 gap_id" | Gap 现在归属 Goal | 仍然必须带 gap_id；新增规则：gap 属于 deferred Goal 时拒绝 |
| 第一轮 §2.3 `gaps_covered` / `sufficient` | 如果按全部 Gap 判定，会迫使系统展开所有子任务，**与最小充分检索冲突** | 改为**只看 active Goal 的 Gap**；deferred Goal 不参与覆盖率计算，写进 finish 的引导追问 |
| StopReason | 新增 `minimal_sufficient`（active Goal 满足 finish gate 且预算触发）、`degraded_answer`（T4 带不确定性回答）、`cache_answer`（L4 命中） | StopEvaluator 表驱动扩展 |
| LedgerStore | 结构增加 Goals，每轮增加 tier、cache、budget_left | 向后兼容：旧账本没有 Goals 时，视为单一 active Goal |
| Compaction | 临时"检索状态"消息原本只含事实和缺口 | 增加"当前目标 + 已推迟目标（仅标题）+ 剩余预算档位"，仍限制在 1.5KB 以内 |
| cassette | cache 命中会绕过 retriever | replay 模式关闭 cache；record 模式同时记录 cache 命中与否，保证轨迹一致 |
| bench | 原有判卷不变 | 新增 `slo`、`goals`、`budget` 可选字段；新增 SLO 回归类型；报告增加 p50/p95 TTFA |
| 性能口径 | 材料强调"先消除明显浪费" | P0 先做三件事：已有证据不重查（L1/L2）、Query 去重、search 只返回增量。算法优化（重排、向量）放 P1/P2 |

**更新后的 P0**（在第一轮 P0 基础上增量添加，仍然全部可离线验证）：
1. Goal 层、`evidence.plan_goals`、单一 active Goal 约束、deferred 拒绝、按 active Goal 判定的 Finish gate 与引导追问；
2. 阶梯 T0–T2 与 T4、预算余量计算与档位截断（T3 为 P1 预留接口）；
3. L1 检索结果缓存（带 Chunk 复核）、L2 同会话证据复用；
4. Trace 与 eval 的 SLO 字段、用例 `slo` 字段、SLO 回归判定。

**P1**：
- T3 向量召回（bge-m3）+ L3 Embedding 缓存；
- 跨会话 L2 复用；
- L4 答案缓存（默认关闭）；
- cassette；
- 渠道级预算配置。

**P2**：
- 重排；
- LLM 充分性裁判；
- 外部数据源路由。

**新增回归与验收用例**：

| 用例 | 验证点 |
|---|---|
| `TestSingleActiveGoal` | 声明 3 个子目标时只有第 1 个具备独立价值的目标为 active；search 指向 deferred Goal 的 gap 被拒绝 |
| `TestFinishGateActiveGoalOnly` | active Goal 覆盖完成即可 finish，返回中包含 deferred Goals 的引导追问；deferred Goals 不计入覆盖率 |
| `TestTierEscalation` | T1 新增证据 < 2 时下一轮允许 T2；未经评估请求 T3 被截断，并说明原因 |
| `TestBudgetDegrade` | 余量 < 50% 时禁用 T3、k 减半；余量 < 20% 时直接进入 T4，StopReason 为 `degraded_answer` 或 `minimal_sufficient` |
| `TestResultCacheChunkRevalidation` | 命中 L1 后修改其中一个文件：只丢弃该 Chunk，其余复用；`index_version` 变化时整体失效 |
| `TestEvidenceNotSharedAcrossChats` | 同一工作区的两个飞书会话不复用 L2 证据 |
| `TestReplayDisablesCache` | replay 模式下 cache 命中数为 0，结果与 cassette 完全一致 |
| `TestSLOFieldsAndRegression` | Trace 与 eval 输出 `ttfa_ms/retrieval_ms/tier_max/cache_hits`；超出 `slo.max_ttfa_ms` 记为 SLO 回归 |
| Bench `r03_minimal_goal` | 复合需求（选项目 + 优化 + 面试题）；判卷要求只完成 G1，回答中包含 G2 的引导问题，检索轮数 ≤ 2 |
| Bench `r04_budget_tight` | `budget.max_ms` 很小，要求走 T1 后收敛，给出带不确定性说明的回答，不超预算 |

**涉及文件**（均为新增或小改；不改 Main Loop）：
- 新增 `internal/retrieval/{types,ledger,goal,stop,tier,budget,cache,fuse,dedup,metrics}.go`；
- 新增 `internal/retrieval/{keyword,grep}_retriever.go`；
- 新增 `internal/tools/{search,evidence}.go`；
- 修改 `internal/runctx`：新增 `WithRetrievalTask`、`WithBudget`；
- 修改 `internal/engine/subagent.go`：生成 task ID、注入检索状态临时消息；
- 修改 `internal/app/app.go`：挂载工具；
- 修改 `internal/eval/benchmark.go`：SLO 字段与回归判定；
- 新增 `bench/retrieval/*.json`、`deploy/skills/iterative-retrieval/SKILL.md`（写入五种追问策略、最小回答与引导追问）。
