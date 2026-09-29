package engine

import (
	"context"
	"strings"
	"testing"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/knowledge"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// knowledgeScript：会话 1 中用户裁决"不要引入 logrus"，模型在交付前被提醒后记录为决策；
// 会话 2（新会话、同一工作区）检查 System Prompt 里是否已带上这条规则。
type knowledgeScript struct {
	sawRuleInSession2 bool
	nudges            int
}

func (k *knowledgeScript) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	last := msgs[len(msgs)-1]
	firstUser := ""
	for _, m := range msgs {
		if m.Role == schema.RoleUser && !m.IsToolResult() {
			firstUser = m.Content
			break
		}
	}
	session2 := strings.Contains(firstUser, "HTTP 客户端")
	if session2 && strings.Contains(msgs[0].Content, "不要引入 logrus") {
		k.sawRuleInSession2 = true
	}
	switch {
	case strings.Contains(last.Content, "交付前知识清点"):
		k.nudges++
		args := `{"kind":"decision","subject":"日志库选型","content":"不要引入 logrus，统一使用 log/slog","reason":"用户明确要求","scope":"本项目所有 Go 代码","source":"user","rule":true}`
		if session2 {
			args = `{"kind":"none"}`
		}
		return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "k", Name: "record_knowledge", Arguments: []byte(args)}}}, nil
	case last.IsToolResult():
		return &schema.Message{Role: schema.RoleAssistant, Content: "好的，已完成。"}, nil
	default:
		return &schema.Message{Role: schema.RoleAssistant, Content: "收到，已照做。"}, nil
	}
}

func TestDynamicKnowledgeAcrossSessions(t *testing.T) {
	wd := t.TempDir()
	km := knowledge.NewManager(t.TempDir())
	reg := tools.NewRegistry()
	reg.Register(tools.NewRecordKnowledgeTool(km.Inbox))
	p := &knowledgeScript{}
	eng := NewAgentEngine(p, reg, false, false)
	eng.Knowledge = km

	s1 := ctxpkg.NewSession("chat-A", wd)
	if err := eng.Chat(context.Background(), s1, "以后不要再引入 logrus 了，统一用标准库 slog。", nil); err != nil {
		t.Fatal(err)
	}
	if p.nudges != 1 {
		t.Fatalf("会话 1 应恰好提醒一次清点，got %d", p.nudges)
	}
	if !strings.Contains(knowledge.RenderPrompt(wd), "不要引入 logrus") {
		t.Fatal("外层未把决策归位到工作区知识载体")
	}
	// 再次唤醒同一会话：清点状态按"本次唤醒"计算，应再提醒一次
	s2 := ctxpkg.NewSession("chat-B", wd)
	if err := eng.Chat(context.Background(), s2, "帮我写个 HTTP 客户端的小工具。", nil); err != nil {
		t.Fatal(err)
	}
	if !p.sawRuleInSession2 {
		t.Fatal("新会话的 System Prompt 应包含上个会话裁决的规则")
	}
	if p.nudges != 2 {
		t.Fatalf("会话 2 也应清点一次（结果为 none），got %d", p.nudges)
	}
	if es, _ := km.Inbox.Pending("chat-B"); len(es) != 0 {
		t.Fatal("归位后暂存区应清空")
	}
}

func TestNoKnowledgeNoNudge(t *testing.T) {
	p := &knowledgeScript{}
	eng := NewAgentEngine(p, tools.NewRegistry(), false, false)
	if err := eng.Chat(context.Background(), ctxpkg.NewSession("x", t.TempDir()), "hi", nil); err != nil || p.nudges != 0 {
		t.Fatalf("未启用知识更新时不应提醒: %v %d", err, p.nudges)
	}
}
