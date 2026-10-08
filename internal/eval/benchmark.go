// Package eval 是第 20 讲的自动化 Benchmark：借鉴 SWE-bench 的 Test-Driven Evaluation，
// 每个用例在隔离工作区里 Setup → 让 Agent 干活 → 用验证脚本客观判卷，并结合成本/耗时/轨迹打分。
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	clawapp "github.com/weberzhao/go-tiny-claw/internal/app"
	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/envlock"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
	"github.com/weberzhao/go-tiny-claw/internal/trace"
)

// TestCase 一个需要 Agent 完成并被客观验证的独立任务。
type TestCase struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Tags     []string `json:"tags,omitempty"`
	Setup    string   `json:"setup,omitempty"`     // Agent 运行前在工作区执行的 bash（初始化靶机）
	Prompt   string   `json:"prompt"`              // 交给 Agent 的任务
	Validate string   `json:"validate"`            // 判卷脚本：exit 0 即通过
	MaxTurns int      `json:"max_turns,omitempty"` // 超过即失败
	Thinking bool     `json:"thinking,omitempty"`
	PlanMode bool     `json:"plan_mode,omitempty"`
	Timeout  int      `json:"timeout_sec,omitempty"`
	// Approval 模拟值班人员对 ask 类高危操作的审批："approve"（默认，全部同意）/ "reject"（全部拒绝）。
	// 没有审批通道时 ask 会 fail-closed 变成拒绝，跑分就测不到"需要审批的正常操作"了。
	Approval string `json:"approval,omitempty"`
	// Knowledge 为 true 时开启动态知识更新（交付前清点 + 外层归位）；一次性跑分默认关闭，省一轮交互
	Knowledge bool `json:"knowledge,omitempty"`
	// NoPrecheck 关闭 Fail-to-Pass 前置校验：判卷脚本是"不许破坏"型的守卫（如审批被拒后目录必须还在），
	// 在 Agent 动手前本来就会通过。默认要求判卷脚本在 Agent 动手前失败（第 20 讲 SWE-bench 范式）。
	NoPrecheck bool `json:"no_precheck,omitempty"`
}

// autoApprover 按用例策略自动审批，并记录审批次数。
type autoApprover struct {
	engine.NopReporter
	approve bool
	mu      sync.Mutex
	count   int
}

func (a *autoApprover) RequestApproval(_ context.Context, req permission.ApprovalRequest) (bool, string) {
	a.mu.Lock()
	a.count++
	a.mu.Unlock()
	if a.approve {
		return true, ""
	}
	return false, "Benchmark 审批策略：拒绝所有高危操作。"
}

// Result 单个用例的结果。除 pass/fail 外还记录轨迹指标（第 20 讲思考题）：
// ToolErrors/FirstTry 衡量"试错成本"，Turns/ToolCalls 衡量"驾驭顺滑度"。
type Result struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Passed           bool    `json:"passed"`
	Error            string  `json:"error,omitempty"`
	DurationMs       int64   `json:"duration_ms"`
	CostCNY          float64 `json:"cost_cny"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Turns            int     `json:"turns"`
	LLMCalls         int     `json:"llm_calls"`
	ToolCalls        int     `json:"tool_calls"`
	ToolErrors       int     `json:"tool_errors"`
	RecoveryHints    int     `json:"recovery_hints"`
	Reminders        int     `json:"reminders"`
	FirstTry         bool    `json:"first_try"` // 通过且全程无工具报错：一发入魂
	Approvals        int     `json:"approvals"`
	WorkDir          string  `json:"work_dir,omitempty"`
	TracePath        string  `json:"trace_path,omitempty"`
	Score            float64 `json:"score"`
	// ObservedModel 服务端回报的实际模型版本（环境锁定：同名模型换代时基线对比才有据可查）
	ObservedModel string `json:"observed_model,omitempty"`
}

// Report 一次跑分的完整报告。
type Report struct {
	StartedAt  time.Time `json:"started_at"`
	Model      string    `json:"model"`
	Protocol   string    `json:"protocol"`
	Total      int       `json:"total"`
	Passed     int       `json:"passed"`
	PassRate   float64   `json:"pass_rate"`
	FirstTry   int       `json:"first_try"`
	TotalCost  float64   `json:"total_cost_cny"`
	DurationMs int64     `json:"duration_ms"`
	AvgTurns   float64   `json:"avg_turns"`
	// AvgScore 平均综合得分（第 20 讲第 4 步：结合成本、耗时、轮数打分；失败计 0）
	AvgScore float64  `json:"avg_score,omitempty"`
	Results  []Result `json:"results"`
	// Env 跑分时的环境快照；与基线不一致时，回归先归因到环境而不是代码
	Env *envlock.Lock `json:"env,omitempty"`
}

// Runner 执行评测集。
type Runner struct {
	Provider    provider.Config
	LLM         provider.LLMProvider // 非空时覆盖 Provider（测试用）
	WorkRoot    string               // 每个用例在其下创建隔离目录
	Parallelism int
	KeepPassed  bool // 通过的用例默认清理工作区；失败的保留以便复盘
}

// LoadCases 从 JSON 文件加载用例，并按正则过滤 ID。
func LoadCases(path, filter string) ([]TestCase, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []TestCase
	if err := json.Unmarshal(b, &cases); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if err := validateCases(cases); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if filter == "" {
		return cases, nil
	}
	re, err := regexp.Compile(filter)
	if err != nil {
		return nil, err
	}
	var out []TestCase
	for _, c := range cases {
		if re.MatchString(c.ID) {
			out = append(out, c)
		}
	}
	return out, nil
}

// validateCases 保证判卷可信：空判卷脚本 `bash -c ""` 会返回 0，等于"自动满分"。
func validateCases(cases []TestCase) error {
	seen := map[string]bool{}
	for i, c := range cases {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("第 %d 个用例缺少 id", i+1)
		}
		if seen[c.ID] {
			return fmt.Errorf("用例 id %q 重复", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Prompt) == "" {
			return fmt.Errorf("用例 %s 缺少 prompt", c.ID)
		}
		if strings.TrimSpace(c.Validate) == "" {
			return fmt.Errorf("用例 %s 缺少 validate 判卷脚本", c.ID)
		}
	}
	return nil
}

// RunSuite 并发执行所有用例（彼此工作区隔离），汇总报告。
func (r *Runner) RunSuite(ctx context.Context, cases []TestCase) Report {
	start := time.Now()
	rep := Report{StartedAt: start, Model: r.Provider.Model, Protocol: r.Provider.Kind, Total: len(cases)}
	if env, err := envlock.Capture(envlock.CaptureOptions{WorkDir: r.WorkRoot, Model: envlock.ModelOf(r.Provider)}); err == nil {
		env.Skills = nil // 用例 Skill 属于各自工作区，不进 Suite 级指纹
		rep.Env = env
	} else {
		log.Printf("⚠️ 采集环境快照失败: %v", err)
	}
	results := make([]Result, len(cases))
	par := r.Parallelism
	if par <= 0 {
		par = 1
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, tc := range cases {
		wg.Add(1)
		go func(i int, tc TestCase) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			log.Printf(">>> ⏳ 正在执行用例 [%s]: %s", tc.ID, tc.Name)
			res := r.runOne(ctx, tc)
			if res.Passed {
				log.Printf(">>> ✅ [%s] 通过 | %dms | %d 轮 | 工具 %d 次(失败 %d) | ¥%.4f", tc.ID, res.DurationMs, res.Turns, res.ToolCalls, res.ToolErrors, res.CostCNY)
			} else {
				log.Printf(">>> ❌ [%s] 失败 | %s", tc.ID, textutil.Truncate(res.Error, 300, "..."))
			}
			results[i] = res
		}(i, tc)
	}
	wg.Wait()
	rep.Results = results
	if rep.Env != nil {
		for _, res := range results {
			if res.ObservedModel != "" {
				rep.Env.Model.Observed = res.ObservedModel
				break
			}
		}
	}
	var turns int
	var score float64
	for i := range results {
		results[i].Score = Score(results[i])
		score += results[i].Score
	}
	for _, res := range results {
		if res.Passed {
			rep.Passed++
		}
		if res.FirstTry {
			rep.FirstTry++
		}
		rep.TotalCost += res.CostCNY
		turns += res.Turns
	}
	if rep.Total > 0 {
		rep.PassRate = float64(rep.Passed) / float64(rep.Total)
		rep.AvgTurns = float64(turns) / float64(rep.Total)
		rep.AvgScore = score / float64(rep.Total)
	}
	rep.DurationMs = time.Since(start).Milliseconds()
	return rep
}

// 综合得分的参考尺度：达到这些量时对应维度的效率分降到一半。
const (
	scoreRefTurns    = 10
	scoreRefCostCNY  = 0.05
	scoreRefDuration = 120 * time.Second
)

// Score 第 20 讲第 4 步的综合得分（0~100）：判卷不通过记 0；通过记 60 分底分，
// 再按轮数（20）、成本（10）、耗时（10）的效率加分，效率 = 1/(1+实际/参考)：越省越接近满分，达到参考值得一半。
// 只是同一套用例下比较 Harness 版本的相对指标，参考尺度随评测集调整。
func Score(r Result) float64 {
	if !r.Passed {
		return 0
	}
	eff := func(x, ref float64) float64 { return 1 / (1 + x/ref) } // 0 → 1，等于参考值 → 0.5
	s := 60 +
		20*eff(float64(r.Turns), scoreRefTurns) +
		10*eff(r.CostCNY, scoreRefCostCNY) +
		10*eff(float64(r.DurationMs), float64(scoreRefDuration.Milliseconds()))
	return math.Round(s*10) / 10
}

func (r *Runner) runOne(ctx context.Context, tc TestCase) (res Result) {
	res = Result{ID: tc.ID, Name: tc.Name}
	start := time.Now()
	if err := validateCases([]TestCase{tc}); err != nil { // 直接调用 RunSuite 的场景同样拒绝空判卷
		res.Error = "用例定义无效: " + err.Error()
		return
	}
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()

	root := r.WorkRoot
	if root == "" {
		root = os.TempDir()
	}
	_ = os.MkdirAll(root, 0o755)
	wd, err := os.MkdirTemp(root, "bench-"+tc.ID+"-")
	if err != nil {
		res.Error = err.Error()
		return
	}
	res.WorkDir = wd
	defer func() {
		if res.Passed && !r.KeepPassed {
			_ = os.RemoveAll(wd)
			res.WorkDir, res.TracePath = "", ""
		}
	}()

	// 1. Setup 靶机
	if tc.Approval != "" && tc.Approval != "approve" && tc.Approval != "reject" {
		res.Error = "用例定义无效: approval 只能是 approve 或 reject"
		return
	}
	if tc.Setup != "" {
		if out, err := runScript(ctx, wd, tc.Setup, 2*time.Minute); err != nil {
			res.Error = fmt.Sprintf("靶机 Setup 失败: %v\n%s", err, out)
			return
		}
	}
	// 1.5 Fail-to-Pass 前置校验（第 20 讲）：判卷脚本必须在 Agent 动手前失败，否则这道题测不出任何东西
	if !tc.NoPrecheck {
		if _, err := runScript(ctx, wd, tc.Validate, 3*time.Minute); err == nil {
			res.Error = "用例无效：判卷脚本在 Agent 动手前就已通过，不满足 Fail-to-Pass（守卫型判卷请设置 no_precheck）"
			return
		}
	}
	// 2. 组装全新引擎（与 CLI/服务端同一套组件）
	a, err := clawapp.Build(clawapp.Options{WorkDir: wd, Thinking: tc.Thinking, PlanMode: tc.PlanMode, Provider: r.Provider, LLM: r.LLM, NoKnowledge: !tc.Knowledge, StateDir: filepath.Join(wd, ".claw", "state")})
	if err != nil {
		res.Error = err.Error()
		return
	}
	if tc.MaxTurns > 0 {
		a.Engine.MaxTurns = tc.MaxTurns
	}
	tracesDir := filepath.Join(wd, ".claw", "traces")
	a.Engine.TraceDir = tracesDir
	var rootSpan *trace.Span
	a.Engine.OnRunFinished = func(s *trace.Span) { rootSpan = s }

	timeout := time.Duration(tc.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess := ctxpkg.NewSession("bench_"+tc.ID, wd)
	approver := &autoApprover{approve: tc.Approval != "reject"}
	runErr := a.Engine.Chat(runCtx, sess, tc.Prompt, approver)
	res.Approvals = approver.count
	res.ObservedModel = a.Model.Observed()

	// 3. 采集成本与轨迹指标
	res.PromptTokens, res.CompletionTokens, res.CostCNY = sess.UsageSnapshot()
	if rootSpan != nil {
		collectTrajectory(rootSpan, &res)
		res.TracePath, _ = rootSpan.Get("trace_path").(string)
	}
	if runErr != nil {
		res.Error = "Agent 运行失败: " + runErr.Error()
		return
	}
	// 4. 客观判卷：Agent 说自己改好了不算数
	if out, err := runScript(ctx, wd, tc.Validate, 3*time.Minute); err != nil {
		res.Error = fmt.Sprintf("验证脚本未通过: %v\n%s", err, textutil.Truncate(out, 1500, "..."))
		return
	}
	res.Passed = true
	res.FirstTry = res.ToolErrors == 0
	return
}

// collectTrajectory 从 Trace 树提取轨迹指标。
func collectTrajectory(root *trace.Span, res *Result) {
	if t, ok := root.Get("turns").(int); ok {
		res.Turns = t
	}
	root.Walk(func(sp *trace.Span, _ int) {
		switch {
		case strings.HasPrefix(sp.Name, "LLM."):
			res.LLMCalls++
		case sp.Name == "Tool.Execute":
			res.ToolCalls++
			if sp.Get("error") == true {
				res.ToolErrors++
			}
			if sp.Get("recovery_hint") == true {
				res.RecoveryHints++
			}
		case strings.HasPrefix(sp.Name, "Turn-"):
			if sp.Get("reminder_injected") == true {
				res.Reminders++
			}
		}
	})
}

func runScript(ctx context.Context, dir, script string, timeout time.Duration) (string, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, "bash", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Markdown 渲染报告；baseline 非空时附带逐用例对比。
func (rep Report) Markdown(baseline *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Harness Benchmark 报告\n\n- 时间：%s\n- 模型：%s（%s 协议）\n", rep.StartedAt.Format("2006-01-02 15:04:05"), rep.Model, rep.Protocol)
	fmt.Fprintf(&b, "- 通过：**%d/%d（%.0f%%）**，一发入魂 %d 个，平均综合得分 **%.1f**\n- 总成本：¥%.4f，总耗时 %.1fs，平均 %.1f 轮\n",
		rep.Passed, rep.Total, rep.PassRate*100, rep.FirstTry, rep.AvgScore, rep.TotalCost, float64(rep.DurationMs)/1000, rep.AvgTurns)
	if baseline != nil {
		fmt.Fprintf(&b, "- 对比基线（%s）：通过率 %.0f%% → %.0f%%，成本 ¥%.4f → ¥%.4f，平均轮数 %.1f → %.1f\n",
			baseline.StartedAt.Format("01-02 15:04"), baseline.PassRate*100, rep.PassRate*100, baseline.TotalCost, rep.TotalCost, baseline.AvgTurns, rep.AvgTurns)
	}
	if models := rep.observedModels(); len(models) > 1 {
		fmt.Fprintf(&b, "- ⚠️ 本次跑分中服务端回报了多个模型版本：%s（跑分途中模型发生了滚动）\n", strings.Join(models, "、"))
	}
	if ds := rep.EnvDrift(baseline); len(ds) > 0 {
		b.WriteString("\n> ⚠️ **环境与基线不一致**：以下差异下的回归/修复不能直接归因到 Harness 代码改动。\n>\n")
		for _, d := range ds {
			fmt.Fprintf(&b, "> - %s\n", d)
		}
	}
	b.WriteString("\n| 用例 | 结果 | 得分 | 轮数 | 工具调用(失败) | 自愈提示 | Reminder | 审批 | Token 入/出 | 成本 | 耗时 |")
	if baseline != nil {
		b.WriteString(" 基线 |")
	}
	b.WriteString("\n|---|---|---|---|---|---|---|---|---|---|---|")
	if baseline != nil {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	base := map[string]Result{}
	if baseline != nil {
		for _, r := range baseline.Results {
			base[r.ID] = r
		}
	}
	rs := append([]Result(nil), rep.Results...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	for _, r := range rs {
		mark := "❌"
		if r.FirstTry {
			mark = "✅ 一发入魂"
		} else if r.Passed {
			mark = "✅"
		}
		fmt.Fprintf(&b, "| %s | %s | %.1f | %d | %d(%d) | %d | %d | %d | %d/%d | ¥%.4f | %.1fs |", r.ID, mark, r.Score, r.Turns, r.ToolCalls, r.ToolErrors,
			r.RecoveryHints, r.Reminders, r.Approvals, r.PromptTokens, r.CompletionTokens, r.CostCNY, float64(r.DurationMs)/1000)
		if baseline != nil {
			if br, ok := base[r.ID]; !ok {
				b.WriteString(" 新增 |")
			} else if br.Passed && !r.Passed {
				b.WriteString(" ⚠️ 回归 |")
			} else if !br.Passed && r.Passed {
				b.WriteString(" 🎉 修复 |")
			} else {
				fmt.Fprintf(&b, " %d 轮 |", br.Turns)
			}
		}
		b.WriteString("\n")
	}
	for _, r := range rs {
		if !r.Passed {
			fmt.Fprintf(&b, "\n### ❌ %s\n\n```\n%s\n```\n工作区：`%s`  Trace：`%s`\n", r.ID, textutil.Truncate(r.Error, 1200, "..."), r.WorkDir, r.TracePath)
		}
	}
	return b.String()
}

// EnvDrift 本次与基线的环境差异；任一方缺少快照（旧报告）视为"未测"，返回空。
func (rep Report) EnvDrift(baseline *Report) []envlock.Drift {
	if baseline == nil || baseline.Env == nil || rep.Env == nil {
		return nil
	}
	return envlock.Diff(baseline.Env, rep.Env)
}

func (rep Report) observedModels() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rep.Results {
		if r.ObservedModel != "" && !seen[r.ObservedModel] {
			seen[r.ObservedModel] = true
			out = append(out, r.ObservedModel)
		}
	}
	sort.Strings(out)
	return out
}

// Regressions 返回在基线中通过、本次失败的用例。
func (rep Report) Regressions(baseline Report) []string {
	passed := map[string]bool{}
	for _, r := range baseline.Results {
		passed[r.ID] = r.Passed
	}
	var out []string
	for _, r := range rep.Results {
		if passed[r.ID] && !r.Passed {
			out = append(out, r.ID)
		}
	}
	return out
}
