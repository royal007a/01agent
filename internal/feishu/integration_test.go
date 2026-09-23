package feishu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// rmProvider：第一轮请求执行 rm -rf build，第二轮把工具结果复述出来。
type rmProvider struct {
	mu   sync.Mutex
	last map[string]string // sessionless：按最后一条 tool 结果回复
}

func (p *rmProvider) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	last := msgs[len(msgs)-1]
	if last.IsToolResult() {
		return &schema.Message{Role: schema.RoleAssistant, Content: "结果: " + last.Content}, nil
	}
	args, _ := json.Marshal(map[string]string{"command": "rm -rf build && echo cleaned"})
	return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "call_rm", Name: "bash", Arguments: args}}}, nil
}

var taskIDRe = regexp.MustCompile(`任务 ID: ([0-9a-f]+)`)

func waitFor(t *testing.T, s *fakeSender, re *regexp.Regexp, from int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		msgs := append([]string(nil), s.msgs...)
		s.mu.Unlock()
		for _, m := range msgs[min(from, len(msgs)):] {
			if g := re.FindStringSubmatch(m); g != nil {
				return g[len(g)-1]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时:\n%s", re, s.all())
	return ""
}

func TestFeishuApprovalEndToEnd(t *testing.T) {
	wd := t.TempDir()
	reg := tools.NewRegistry()
	reg.Register(tools.NewBashTool(wd))
	reg.Use(permission.Middleware(permission.NewPolicy("")))
	eng := engine.NewAgentEngine(&rmProvider{}, reg, false, false)
	sessions := ctxpkg.NewSessionManager("")

	sender := &fakeSender{}
	bot := NewFeishuBot(sender, func(ctx context.Context, chatID, prompt string, r engine.Reporter) error {
		return eng.Chat(ctx, sessions.GetOrCreate(chatID, wd), prompt, r)
	})
	bot.Approvals = permission.NewApprovalManager(5 * time.Second)
	bot.UseCommand(ApprovalCommand(bot.Approvals, sender))

	for _, tc := range []struct {
		chat, verdict string
		wantGone      bool
	}{{"oc_reject", "reject", false}, {"oc_approve", "approve", true}} {
		os.MkdirAll(filepath.Join(wd, "build"), 0o755)
		from := len(sender.msgs)
		bot.HandleMessage(context.Background(), "m_"+tc.chat, tc.chat, "清理一下 build 目录")
		id := waitFor(t, sender, taskIDRe, from)
		// 其他群的人不能批准
		bot.HandleMessage(context.Background(), "x_"+tc.chat, "oc_other", "approve "+id)
		bot.HandleMessage(context.Background(), "v_"+tc.chat, tc.chat, tc.verdict+" "+id)
		bot.Wait()
		_, err := os.Stat(filepath.Join(wd, "build"))
		if gone := os.IsNotExist(err); gone != tc.wantGone {
			t.Fatalf("%s: build 删除状态=%v，期望 %v\n%s", tc.verdict, gone, tc.wantGone, sender.all())
		}
	}
	out := sender.all()
	for _, want := range []string{"oc_other|⚠️ 任务", "不属于当前会话", "oc_reject|🚫 已拒绝任务", "oc_approve|✅ 已批准任务", "oc_approve|结果: cleaned", "oc_reject|结果: 执行被系统拦截"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
}
