// Package knowledge 解决"跨会话知识丢失"坏味道（动态知识更新）：
//
//   - 内层循环（每次任务执行）只负责在交付前按三类清点知识，写入会话暂存区（Inbox），
//     它不需要、也不应该知道知识最终住在哪里；
//   - 外层循环（Harness 编排层）掌握工作区的知识布局（Layout），在任务结束后统一
//     把条目归位到规则文件、ADR、偏好文件、发现记录，并注入后续会话的 System Prompt。
package knowledge

import (
	"fmt"
	"strings"
	"time"
)

// Kind 知识的三种来源类型（外加 none：本轮确认没有新知识）。
type Kind string

const (
	// UserTriage 用户统筹：用户表达的优先级与偏好。是倾向，不是定论。
	UserTriage Kind = "user_triage"
	// Discovery 发现：模型在执行中自己核对出的新事实。
	Discovery Kind = "discovery"
	// Decision 决策：模型自主做出或用户裁决的结论，最需要同步，必须带理由与适用范围。
	Decision Kind = "decision"
	// None 本轮清点后确认没有值得跨会话保留的知识。
	None Kind = "none"
)

// Status 条目在知识库中的状态。
type Status string

const (
	Active     Status = "active"
	Superseded Status = "superseded"
)

// Entry 一条知识。
type Entry struct {
	ID      string `json:"id"`
	Kind    Kind   `json:"kind"`
	Subject string `json:"subject"`          // 主题键：同主题的新决策/偏好会取代旧的
	Content string `json:"content"`          // 结论 / 事实 / 偏好内容
	Reason  string `json:"reason,omitempty"` // 决策理由
	Scope   string `json:"scope,omitempty"`  // 适用场景 / 适用范围
	Source  string `json:"source"`           // user | model | 具体来源（文件、命令、工具）
	// Rule 为 true 的决策属于"所有任务都必须遵守的约束"，会进入规则文件并置顶注入。
	Rule       bool      `json:"rule,omitempty"`
	SessionID  string    `json:"session_id"`
	Task       string    `json:"task,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`

	// 以下由外层归位时填写
	Status       Status `json:"status,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	Location     string `json:"location,omitempty"` // 归位到的文件（相对工作区）
}

// Validate 按类型检查必填字段：记录不完整的知识等于没有记录。
func (e *Entry) Validate() error {
	e.Subject = strings.TrimSpace(e.Subject)
	e.Content = strings.TrimSpace(e.Content)
	e.Scope = strings.TrimSpace(e.Scope)
	e.Source = strings.TrimSpace(e.Source)
	switch e.Kind {
	case None:
		return nil
	case UserTriage:
		if e.Content == "" || e.Scope == "" {
			return fmt.Errorf("用户统筹需要 content（偏好内容）与 scope（适用场景）")
		}
		if e.Source == "" {
			e.Source = "user"
		}
		if e.Source != "user" {
			return fmt.Errorf("用户统筹的 source 只能是 user")
		}
	case Discovery:
		if e.Content == "" || e.Source == "" {
			return fmt.Errorf("发现需要 content（事实）与 source（从哪里核对出来的：文件、命令或工具）")
		}
	case Decision:
		if e.Content == "" || strings.TrimSpace(e.Reason) == "" || e.Scope == "" {
			return fmt.Errorf("决策需要 content（结论）、reason（理由）与 scope（适用范围）")
		}
		if e.Source != "user" && e.Source != "model" {
			return fmt.Errorf("决策的 source 必须是 user（用户裁决）或 model（模型自主决策）")
		}
	default:
		return fmt.Errorf("未知的知识类型 %q（可选 user_triage / discovery / decision / none）", e.Kind)
	}
	if e.Subject == "" {
		return fmt.Errorf("需要 subject（主题键，用于去重与取代旧条目，例如 \"日志库选型\"、\"单词 abandon 掌握情况\"）")
	}
	return nil
}

// Norm 规范化文本，用于去重与主题匹配。
func Norm(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
