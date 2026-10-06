package tools

import (
	"context"
	"encoding/json"
	"fmt"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/knowledge"
	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// RecordKnowledgeTool 内层循环的"交付前知识清点"入口：只写会话暂存区，不关心知识最终住在哪里。
type RecordKnowledgeTool struct{ Inbox *knowledge.Inbox }

func (t *RecordKnowledgeTool) Meta() ToolMeta {
	return ToolMeta{SideEffect: "none", MaxOutputBytes: 4000}
}

func NewRecordKnowledgeTool(inbox *knowledge.Inbox) *RecordKnowledgeTool {
	return &RecordKnowledgeTool{Inbox: inbox}
}

func (t *RecordKnowledgeTool) Name() string { return "record_knowledge" }

// ReadOnly=false：会写暂存区，同批调用按顺序执行。
func (t *RecordKnowledgeTool) ReadOnly() bool { return false }

func (t *RecordKnowledgeTool) Definition() schema.ToolDefinition {
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	return schema.ToolDefinition{
		Name: t.Name(),
		Description: "在交付前记录本轮产生、值得跨会话保留的知识（每条调用一次）。三类：" +
			"user_triage=用户统筹（用户表达的偏好/优先级，是倾向不是定论，必须写适用场景 scope）；" +
			"discovery=发现（你在执行中核对出的新事实，必须写 source：从哪个文件/命令/工具得到）；" +
			"decision=决策（结论+理由+适用范围，source=user 表示用户裁决，model 表示你自主决定；所有任务都必须遵守的约束设 rule=true）。" +
			"本轮没有值得保留的知识时，调用一次 kind=none。你不需要关心知识存到哪里，Harness 会统一归位。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"kind":    map[string]interface{}{"type": "string", "enum": []string{"user_triage", "discovery", "decision", "none"}},
				"subject": str("主题键，用于去重和取代旧条目，例如 \"日志库选型\"、\"单词 abandon 掌握情况\""),
				"content": str("偏好内容 / 事实 / 决策结论"),
				"reason":  str("决策理由（decision 必填）"),
				"scope":   str("适用场景或适用范围（user_triage 与 decision 必填）"),
				"source":  str("user_triage 固定为 user；decision 为 user 或 model；discovery 为具体来源"),
				"rule":    map[string]interface{}{"type": "boolean", "description": "decision 是否为所有任务都必须遵守的约束"},
			},
			"required": []string{"kind"},
		},
	}
}

func (t *RecordKnowledgeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var e knowledge.Entry
	if err := json.Unmarshal(args, &e); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	s, _ := runctx.Session(ctx).(*ctxpkg.Session)
	if s == nil {
		return "", fmt.Errorf("记录知识需要会话身份")
	}
	e.SessionID = s.ID
	e.ID, e.Status, e.SupersededBy, e.Location = "", "", "", "" // 这些由外层归位时决定
	rec, err := t.Inbox.Record(e)
	if err != nil {
		return "", err
	}
	if rec.Kind == knowledge.None {
		return "已确认：本轮没有需要跨会话保留的知识。", nil
	}
	return fmt.Sprintf("已记录 %s「%s」，任务结束后由 Harness 统一归位。", rec.Kind, rec.Subject), nil
}
