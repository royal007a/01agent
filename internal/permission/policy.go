// Package permission 实现 allow / ask / deny 三态的工具权限判定（第 16 讲 + 思考题）。
//
// 规则来源：内置默认规则 + 工作区 .claw/permissions.json（运行时热加载，改完即生效，无需重启）。
// 判定优先级与 Claude Code 一致：deny > ask > allow；都不命中则按 YOLO 默认放行。
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

type Action string

const (
	Allow Action = "allow"
	Ask   Action = "ask"
	Deny  Action = "deny"
)

// Rule 一条权限规则。Tool 为空或 "*" 表示匹配所有工具；Pattern 是作用在参数 JSON 上的正则。
type Rule struct {
	Tool    string `json:"tool"`
	Pattern string `json:"pattern"`
	Action  Action `json:"action"`
	re      *regexp.Regexp
}

// File 是 .claw/permissions.json 的 Schema。
//
//	{"rules": [
//	  {"tool": "bash", "pattern": "kubectl\\s+delete", "action": "ask"},
//	  {"tool": "bash", "pattern": "^\\{\"command\":\"git status", "action": "allow"}
//	]}
type File struct {
	Rules []Rule `json:"rules"`
}

// DefaultRules 内置规则：课程 IsDangerousCommand 的黑名单 + 若干常见高危操作。
// 注意课程的 `rm\s+-r` 抓不到 `rm -fr`，这里改为匹配任意包含 r/f 的选项组合。
var DefaultRules = []Rule{
	{Tool: "bash", Pattern: `\brm\s+(-[a-zA-Z]+\s+)*-[a-zA-Z]*[rR][a-zA-Z]*\s+/(\s|"|$)`, Action: Deny}, // rm -rf /
	{Tool: "bash", Pattern: `:\(\)\s*\{\s*:\|:&\s*\};:`, Action: Deny},                                  // fork bomb
	{Tool: "bash", Pattern: `\brm\s+(-[a-zA-Z]+\s+)*-[a-zA-Z]*[rRf]`, Action: Ask},
	{Tool: "bash", Pattern: `\bsudo\b`, Action: Ask},
	{Tool: "bash", Pattern: `(?i)\bdrop\s+(table|database|schema)\b`, Action: Ask},
	{Tool: "bash", Pattern: `>\s*[^\s|&]*\.go\b`, Action: Ask}, // 恶意覆盖源代码
	{Tool: "bash", Pattern: `\bgit\s+(push|reset\s+--hard|clean\s+-[a-zA-Z]*f)`, Action: Ask},
	{Tool: "bash", Pattern: `\b(kubectl\s+delete|mkfs|shutdown|reboot)\b|\bdd\s+if=`, Action: Ask},
	{Tool: "bash", Pattern: `chmod\s+(-R\s+)?777`, Action: Ask},
}

// Policy 带热加载能力的权限判定引擎。
type Policy struct {
	path string

	mu      sync.RWMutex
	rules   []Rule
	modTime time.Time
}

// NewPolicy 以内置规则初始化；path 非空时额外加载并监视该 JSON 文件。
func NewPolicy(path string) *Policy {
	p := &Policy{path: path}
	p.rules = compile(DefaultRules)
	p.reloadIfChanged()
	return p
}

func compile(rs []Rule) []Rule {
	out := make([]Rule, 0, len(rs))
	for _, r := range rs {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			log.Printf("[Permission] 忽略非法规则 %q: %v", r.Pattern, err)
			continue
		}
		r.re = re
		out = append(out, r)
	}
	return out
}

// reloadIfChanged 用 mtime 判断是否需要重新加载；读取与解析在锁外完成，成功后原子替换。
func (p *Policy) reloadIfChanged() {
	if p.path == "" {
		return
	}
	info, err := os.Stat(p.path)
	if err != nil {
		return
	}
	p.mu.RLock()
	same := info.ModTime().Equal(p.modTime)
	p.mu.RUnlock()
	if same {
		return
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("[Permission] %s 解析失败，继续使用旧规则: %v", p.path, err)
		return
	}
	rules := compile(append(append([]Rule{}, DefaultRules...), f.Rules...))
	p.mu.Lock()
	p.rules, p.modTime = rules, info.ModTime()
	p.mu.Unlock()
	log.Printf("[Permission] 已热加载 %s（自定义规则 %d 条）", p.path, len(f.Rules))
}

// Decide 返回该调用的判定结果以及命中的规则。
func (p *Policy) Decide(call schema.ToolCall) (Action, string) {
	p.reloadIfChanged()
	p.mu.RLock()
	defer p.mu.RUnlock()
	subject := string(call.Arguments)
	if cmd := commandOf(call); cmd != "" {
		subject = cmd
	}
	var hit = map[Action]string{}
	for _, r := range p.rules {
		if r.Tool != "" && r.Tool != "*" && r.Tool != call.Name {
			continue
		}
		if r.re.MatchString(subject) {
			if _, ok := hit[r.Action]; !ok {
				hit[r.Action] = r.Pattern
			}
		}
	}
	for _, a := range []Action{Deny, Ask, Allow} {
		if pat, ok := hit[a]; ok {
			return a, pat
		}
	}
	return Allow, ""
}

// commandOf 对 bash 调用取出 command 字段，让正则作用在真实命令而非转义后的 JSON 上。
func commandOf(call schema.ToolCall) string {
	var a struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(call.Arguments, &a) == nil {
		return a.Command
	}
	return ""
}

// ApprovalRequest 一次人工审批请求。
type ApprovalRequest struct {
	ToolName string
	Args     string
	Rule     string
}

// Approver 由展示层实现（飞书 Reporter、终端 Reporter），负责把请求送到人类面前并阻塞等待结果。
type Approver interface {
	RequestApproval(ctx context.Context, req ApprovalRequest) (allowed bool, reason string)
}

// Middleware 把 Policy 装配成 Registry 中间件。
// ask 时从 ctx 中取出当前运行的 Reporter，若它实现了 Approver 就交给人类审批；
// 找不到审批通道时 fail-closed 拒绝（宁可不做，也不在无人值守时执行高危命令）。
func Middleware(p *Policy) tools.MiddlewareFunc {
	return func(ctx context.Context, call schema.ToolCall) (bool, string) {
		action, rule := p.Decide(call)
		switch action {
		case Deny:
			return false, fmt.Sprintf("命中禁止规则 `%s`，该操作被永久禁止。", rule)
		case Ask:
			approver, ok := runctx.Reporter(ctx).(Approver)
			if !ok {
				return false, fmt.Sprintf("命中高危规则 `%s`，需要人工审批，但当前运行环境没有审批通道，已拒绝。", rule)
			}
			log.Printf("[Permission] 工具 %s 命中高危规则 %s，挂起等待人工审批...", call.Name, rule)
			allowed, reason := approver.RequestApproval(ctx, ApprovalRequest{ToolName: call.Name, Args: string(call.Arguments), Rule: rule})
			if !allowed {
				return false, reason
			}
			return true, ""
		default:
			return true, ""
		}
	}
}
