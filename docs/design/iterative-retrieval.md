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
