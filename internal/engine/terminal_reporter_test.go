package engine

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// 第 16/21 讲：终端审批不能无限阻塞——要响应取消、有超时，并行子 Agent 的审批要串行提问。
func TestTerminalApprovalHonorsCancelAndTimeout(t *testing.T) {
	pr, pw := io.Pipe() // 永远不输入
	defer pw.Close()
	r := &TerminalReporter{Out: io.Discard, In: pr, ApprovalTimeout: 50 * time.Millisecond}
	start := time.Now()
	if ok, reason := r.RequestApproval(context.Background(), permission.ApprovalRequest{ToolName: "bash"}); ok || !strings.Contains(reason, "超时") {
		t.Fatalf("超时应拒绝: ok=%v reason=%q", ok, reason)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("超时未生效")
	}
	r.ApprovalTimeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if ok, reason := r.RequestApproval(ctx, permission.ApprovalRequest{ToolName: "bash"}); ok || !strings.Contains(reason, "取消") {
		t.Fatalf("ctx 取消应拒绝: ok=%v reason=%q", ok, reason)
	}
}

func TestTerminalApprovalSerializesConcurrentRequests(t *testing.T) {
	r := &TerminalReporter{Out: io.Discard, In: strings.NewReader("y\nn\n"), ApprovalTimeout: time.Second}
	var wg sync.WaitGroup
	var mu sync.Mutex
	approved := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := r.RequestApproval(context.Background(), permission.ApprovalRequest{ToolName: "bash"}); ok {
				mu.Lock()
				approved++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if approved != 1 {
		t.Fatalf("两次审批应各读到一行输入（y 与 n），approved=%d", approved)
	}
}

func TestTerminalShowThinking(t *testing.T) {
	var buf bytes.Buffer
	r := &TerminalReporter{Out: &buf}
	r.OnThinkingContent(context.Background(), "先读 README")
	if buf.Len() != 0 {
		t.Fatal("ShowThinking 关闭时不应输出")
	}
	r.ShowThinking = true
	r.OnThinkingContent(context.Background(), "先读 README")
	if !strings.Contains(buf.String(), "先读 README") {
		t.Fatalf("ShowThinking 开启时应展示思考内容: %q", buf.String())
	}
}

type thinkCapture struct {
	NopReporter
	got string
}

func (c *thinkCapture) OnThinkingContent(_ context.Context, s string) { c.got = s }

type planThenAnswer struct{}

func (planThenAnswer) Generate(_ context.Context, _ []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	if tools == nil {
		return &schema.Message{Role: schema.RoleAssistant, Content: "计划：先读 README"}, nil
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "完成。"}, nil
}

func TestEngineForwardsThinkingContent(t *testing.T) {
	c := &thinkCapture{}
	if err := NewAgentEngine(planThenAnswer{}, &fakeRegistry{}, true, false).Chat(context.Background(), ctxpkg.NewSession("th", t.TempDir()), "做", c); err != nil {
		t.Fatal(err)
	}
	if c.got != "计划：先读 README" {
		t.Fatalf("Phase 1 的思考内容应交给 Reporter: %q", c.got)
	}
}
