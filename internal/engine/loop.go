// Package engine 是核心引擎层：维护上下文时间线，驱动 ReAct Main Loop。
package engine

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
	"github.com/weberzhao/go-tiny-claw/internal/trace"
)

// 慢思考阶段的两条"临时提示"：只拼进当次请求，不写入上下文时间线。
//
// 实测（方舟 doubao / Anthropic 协议）发现课程原版的纯架构剥离有两个坑：
//  1. 没有工具 Schema 时，模型会在文本里"脑补"伪工具调用（课程第 04 讲也观察到了 <invoke> 现象）；
//  2. Phase 2 的请求以 assistant(思考) 结尾，Anthropic 协议会把它当作 prefill 续写，
//     模型只会接着写文字而不再发起工具调用。
//
// 因此 Phase 1 追加一条"只规划、不调用"的提示，Phase 2 追加一条"按计划行动"的提示。
const (
	thinkingNudge = "[系统: 规划阶段] 现在请只做分析与规划：梳理已知信息、下一步要做什么以及原因。此阶段没有任何工具可用，不要输出或模拟任何工具调用语法。"
	actionNudge   = "[系统: 行动阶段] 请按照你上面的规划立即行动：需要工具就直接发起工具调用；如果任务已经完成，则直接给出最终答复。"
)

// AgentEngine 是微型 OS 的核心驱动。
// 从第 11 讲开始，引擎本身无状态：记忆由 Session 承载，工作区跟随 Session。
type AgentEngine struct {
	provider provider.LLMProvider
	registry tools.Registry
	// EnableThinking 慢思考模式开关（第 03 讲 Two-Stage ReAct）
	EnableThinking bool
	// PlanMode 计划模式开关（第 13 讲状态外部化）
	PlanMode bool
	// WorkingMemoryLimit 每轮从 Session 截取的最近消息条数（第 11 讲）
	WorkingMemoryLimit int
	// MaxTurns 硬性预算熔断：单次唤醒最多跑多少个 Turn（0 表示不限制）。
	// 课程主张不在 Loop 里做生硬截断，依赖 Compaction 与 Reminders；这里只作为最终兜底。
	MaxTurns int
	// TraceDir 链路追踪导出目录（第 19 讲）；为空时写入 <工作区>/.claw/traces，为 "-" 时不导出。
	TraceDir string
	// OnRunFinished 每次 Run 结束时回调根 Span（Benchmark 用它统计轮数、工具失败等轨迹指标）。
	OnRunFinished func(root *trace.Span)
	compactor     *ctxpkg.Compactor
	recovery      *ctxpkg.RecoveryManager
	injector      *ReminderInjector
}

func NewAgentEngine(p provider.LLMProvider, r tools.Registry, enableThinking bool, planMode bool) *AgentEngine {
	return &AgentEngine{
		provider:           p,
		registry:           r,
		EnableThinking:     enableThinking,
		PlanMode:           planMode,
		WorkingMemoryLimit: 20,
		MaxTurns:           60,
		compactor:          ctxpkg.NewCompactor(20000, 6),
		recovery:           ctxpkg.NewRecoveryManager(),
		injector:           NewReminderInjector(),
	}
}

// SetCompactor 替换默认的上下文压缩器。
func (e *AgentEngine) SetCompactor(c *ctxpkg.Compactor) { e.compactor = c }

// Chat 把一条用户指令压入 Session 并唤醒引擎。
func (e *AgentEngine) Chat(ctx context.Context, session *ctxpkg.Session, prompt string, reporter Reporter) error {
	session.Append(schema.Message{Role: schema.RoleUser, Content: prompt})
	return e.Run(ctx, session, reporter)
}

// Run 以 Session 为上下文载体驱动 ReAct 循环，直到模型不再请求工具。
func (e *AgentEngine) Run(ctx context.Context, session *ctxpkg.Session, reporter Reporter) (err error) {
	if reporter == nil {
		reporter = NopReporter{}
	}
	// 第 19 讲：根 Span 覆盖整个 Run；defer 保证成功/失败都会结束并导出"数字病历"
	ctx, root := trace.Start(ctx, "Agent.Run")
	root.Set("session_id", session.ID)
	root.Set("work_dir", session.WorkDir)
	root.Set("thinking", e.EnableThinking)
	root.Set("plan_mode", e.PlanMode)
	defer func() {
		if err != nil {
			root.Set("error", err.Error())
		}
		root.End()
		e.exportTrace(root, session)
		if e.OnRunFinished != nil {
			e.OnRunFinished(root)
		}
	}()
	// 把当前会话与 Reporter 放进 ctx，供 Middleware（审批）/ Tracker（计费）等横切组件使用
	ctx = runctx.WithReporter(runctx.WithSession(ctx, session), reporter)
	log.Printf("[Engine] 唤醒会话 [%s]，锁定工作区: %s (Thinking: %v, PlanMode: %v)", session.ID, session.WorkDir, e.EnableThinking, e.PlanMode)

	e.injector.Forget(session.ID) // 新的人类指令，重置死循环监控与熔断状态

	// 根据当前 Session 的工作区动态组装 System Prompt（AGENTS.md + Skills + Plan Mode）
	systemMsg := ctxpkg.NewPromptComposer(session.WorkDir, e.PlanMode).Build()

	for turn := 1; ; turn++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.MaxTurns > 0 && turn > e.MaxTurns {
			msg := fmt.Sprintf("⛔ 已达到单次任务的最大轮数 (%d)，为防止失控已强制停止。请检查进度后再下达更具体的指令。", e.MaxTurns)
			reporter.OnMessage(ctx, msg)
			return fmt.Errorf("超过最大轮数 %d", e.MaxTurns)
		}
		log.Printf("========== [Turn %d] 开始 ==========", turn)
		turnCtx, turnSpan := trace.Start(ctx, fmt.Sprintf("Turn-%d", turn))
		root.Set("turns", turn)
		done, err := e.runTurn(turnCtx, turnSpan, session, systemMsg, reporter)
		turnSpan.End() // 课程在 for 循环里 defer，所有 Turn 都要等 Run 结束才关闭，耗时全错；这里逐轮结束
		if err != nil || done {
			return err
		}
	}
}

// runTurn 执行一个 Turn；done=true 表示模型不再请求工具。
func (e *AgentEngine) runTurn(ctx context.Context, turnSpan *trace.Span, session *ctxpkg.Session, systemMsg schema.Message, reporter Reporter) (done bool, err error) {
	availableTools := e.registry.GetAvailableTools()

	// 1. 上下文组装：System Prompt + Working Memory，再过一遍内存压缩器
	// RepairToolPairs：会话若在工具执行中途被重启，历史里会残留"有调用没结果"，补齐后才能合法发送
	contextHistory := append([]schema.Message{systemMsg}, ctxpkg.RepairToolPairs(session.GetWorkingMemory(e.WorkingMemoryLimit))...)
	compacted := e.compactor.Compact(contextHistory)
	turnSpan.Set("context_message_count", len(compacted))
	turnSpan.Set("context_chars", ctxpkg.EstimateLength(compacted))
	if len(compacted) > 0 && ctxpkg.EstimateLength(compacted) < ctxpkg.EstimateLength(contextHistory) {
		turnSpan.Set("compacted", true)
	}

	// 2. Phase 1: 慢思考 —— 剥夺工具，强制规划
	var thinking string
	if e.EnableThinking {
		log.Println("[Engine][Phase 1] 剥夺工具访问权，强制进入慢思考与规划阶段...")
		reporter.OnThinking(ctx)
		thinkReq := append(append([]schema.Message{}, compacted...), schema.Message{Role: schema.RoleUser, Content: thinkingNudge})
		thinkCtx, thinkSpan := trace.Start(ctx, "LLM.Thinking")
		thinkResp, err := e.provider.Generate(thinkCtx, thinkReq, nil)
		endLLMSpan(thinkSpan, thinkResp, err)
		if err != nil {
			return false, fmt.Errorf("Thinking 阶段生成失败: %w", err)
		}
		if thinkResp.Content != "" {
			thinking = thinkResp.Content
			log.Printf("[Engine] 🧠 内部思考 Trace: %s", textutil.Truncate(thinking, 300, "..."))
			compacted = append(compacted,
				schema.Message{Role: schema.RoleAssistant, Content: thinking},
				schema.Message{Role: schema.RoleUser, Content: actionNudge})
		}
	}

	// 3. Phase 2: 行动 —— 恢复工具，顺着规划执行
	log.Println("[Engine][Phase 2] 恢复工具挂载，等待模型采取行动...")
	actCtx, actSpan := trace.Start(ctx, "LLM.Action")
	actionResp, err := e.provider.Generate(actCtx, compacted, availableTools)
	endLLMSpan(actSpan, actionResp, err)
	if err != nil {
		return false, fmt.Errorf("Action 阶段生成失败: %w", err)
	}
	// 第 13/14 讲的 1214 修复：把思考与行动合并为一条合法的 Assistant 消息写入 Session，
	// 避免出现连续两条 assistant、或 assistant(思考) 之后紧跟 tool 消息的非法序列。
	final := *actionResp
	if thinking != "" {
		final.Content = strings.TrimSpace(thinking + "\n" + actionResp.Content)
	}
	session.Append(final)
	if actionResp.Content != "" {
		reporter.OnMessage(ctx, actionResp.Content)
	}

	// 4. 退出条件：模型没有请求任何工具调用，挂起等待人类的下一条指令
	if len(actionResp.ToolCalls) == 0 {
		log.Println("[Engine] 模型未请求调用工具，任务宣告完成。")
		return true, nil
	}

	// 5. 执行行动并把 Observation 写入 Session
	log.Printf("[Engine] 模型请求调用 %d 个工具...", len(actionResp.ToolCalls))
	observations := e.executeTools(ctx, actionResp.ToolCalls, reporter)
	session.Append(observations...)

	// 6. 死循环探测（第 15 讲）：必要时把严厉提醒作为最新的 User 消息追加到 Session 末尾
	if reminder := e.injector.CheckAndInject(session.ID, actionResp.ToolCalls, observations); reminder != nil {
		session.Append(*reminder)
		turnSpan.Set("reminder_injected", true)
		reporter.OnMessage(ctx, "⚠️ 检测到重复失败，已注入 System Reminder 干预。")
	}
	return false, nil
}

// endLLMSpan 记录一次模型调用的结果概要。
func endLLMSpan(sp *trace.Span, resp *schema.Message, err error) {
	if err != nil {
		sp.Set("error", err.Error())
	} else if resp != nil {
		sp.Set("tool_calls", len(resp.ToolCalls))
		sp.Set("content_preview", textutil.Truncate(resp.Content, 200, "..."))
		if resp.Usage != nil {
			sp.Set("prompt_tokens", resp.Usage.PromptTokens)
			sp.Set("completion_tokens", resp.Usage.CompletionTokens)
		}
	}
	sp.End()
}

// exportTrace 只导出顶层 Run（子智能体的 Run 作为子树嵌在父 Trace 里）。
func (e *AgentEngine) exportTrace(root *trace.Span, session *ctxpkg.Session) {
	if e.TraceDir == "-" {
		return
	}
	dir := e.TraceDir
	if dir == "" {
		dir = filepath.Join(session.WorkDir, ".claw", "traces")
	}
	path, err := trace.Export(root, dir, session.ID)
	if path != "" {
		root.Set("trace_path", path) // 仅供进程内回调读取（文件已写完，不会进入导出内容）
	}
	if err != nil {
		log.Printf("[Tracing] 导出失败: %v", err)
	} else {
		log.Printf("📊 [Tracing] 本次任务的执行回放链路已保存: %s（*.perfetto.json 可拖进 ui.perfetto.dev 看甘特图）", path)
	}
}

// MaxParallelTools 单轮最多同时运行的工具数（第 08 讲思考题：信号量限流）。
const MaxParallelTools = 5

// executeTools 执行一批工具调用，并按模型原始顺序返回 Observation。
//
// 课程采用"独立性假设"无脑并行；第 08 讲的思维实验同时指出了同一文件读写竞争的风险，
// 并推荐"只读并发、涉写串行"。这里取两者之长：整批都是只读工具时并发（带信号量限流），
// 只要混入任何有副作用的工具（write/edit/bash），就退化为按顺序串行执行。
// 两种路径都写入预分配切片的固定下标，天然保序、无需加锁。
func (e *AgentEngine) executeTools(ctx context.Context, calls []schema.ToolCall, reporter Reporter) []schema.Message {
	return e.executeToolsWith(ctx, e.registry, calls, reporter)
}

// executeToolsWith 使用指定注册表执行（子智能体用受限注册表）。
func (e *AgentEngine) executeToolsWith(ctx context.Context, registry tools.Registry, calls []schema.ToolCall, reporter Reporter) []schema.Message {
	observations := make([]schema.Message, len(calls))
	run := func(idx int, call schema.ToolCall) {
		log.Printf("  -> [Go-%d] 🛠️ 执行工具: %s, 参数: %s", idx, call.Name, textutil.Truncate(string(call.Arguments), 300, "..."))
		reporter.OnToolCall(ctx, call.Name, string(call.Arguments))
		toolCtx, sp := trace.Start(ctx, "Tool.Execute")
		sp.Set("tool_name", call.Name)
		sp.Set("arguments", textutil.Truncate(string(call.Arguments), 500, "..."))
		defer sp.End()
		var result schema.ToolResult
		if sess, _ := runctx.Session(ctx).(*ctxpkg.Session); sess != nil && e.injector.IsTripped(sess.ID, call) {
			sp.Set("circuit_broken", true)
			log.Printf("  -> [Go-%d] ⛔ 动作已被熔断，拒绝执行", idx)
			result = schema.ToolResult{ToolCallID: call.ID, Output: breakerMessage, IsError: true}
		} else {
			result = registry.Execute(toolCtx, call)
		}
		if strings.HasPrefix(result.Output, "执行被系统拦截") {
			sp.Set("intercepted", true)
		}
		// 错误自愈（第 14 讲）：失败时由 RecoveryManager 诊断并注入"锦囊妙计"
		if result.IsError {
			result.Output = e.recovery.AnalyzeAndInject(call.Name, result.Output)
			sp.Set("error", true)
			if strings.Contains(result.Output, "[系统救援指南]") {
				sp.Set("recovery_hint", true)
			}
		}
		sp.Set("output_preview", textutil.Truncate(result.Output, 200, "..."))
		// 人类看到的是缩略版（防止飞书消息过长）；传给模型的 Observation 依然是完整数据
		reporter.OnToolResult(ctx, call.Name, textutil.Truncate(result.Output, 200, "... (已截断)"), result.IsError)
		if result.IsError {
			log.Printf("  -> [Go-%d] ❌ 工具执行报错: %s", idx, textutil.Truncate(result.Output, 300, "..."))
		} else {
			log.Printf("  -> [Go-%d] ✅ 工具执行成功 (返回 %d 字节)", idx, len(result.Output))
		}
		observations[idx] = schema.Message{
			Role:       schema.RoleUser,
			Content:    result.Output,
			ToolCallID: call.ID,
			IsError:    result.IsError,
		}
	}

	parallel := len(calls) > 1
	for _, c := range calls {
		if !registry.IsReadOnly(c.Name) {
			parallel = false
			break
		}
	}
	if !parallel {
		for i, c := range calls {
			run(i, c)
		}
		return observations
	}

	log.Printf("[Engine] 本批 %d 个工具均为只读，Fork 并发执行", len(calls))
	sem := make(chan struct{}, MaxParallelTools)
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(idx int, call schema.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			run(idx, call)
		}(i, c)
	}
	wg.Wait()
	log.Println("[Engine] 所有并发工具执行完毕，开始聚合观察结果 (Observation)...")
	return observations
}
