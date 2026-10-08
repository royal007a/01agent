package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/core/httpserverext"

	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
)

type fakeSender struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fakeSender) SendText(_ context.Context, chatID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, chatID+"|"+text)
	return nil
}

func (f *fakeSender) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.msgs, "\n")
}

func eventBody(msgID, chatID, text string) string {
	return eventBodyIn(msgID, chatID, text, "p2p", false)
}

func eventBodyIn(msgID, chatID, text, chatType string, mention bool) string {
	content, _ := json.Marshal(map[string]string{"text": text})
	ev := map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_id": "ev_" + msgID, "token": "vt", "create_time": "1", "event_type": "im.message.receive_v1", "tenant_key": "t", "app_id": "cli_x"},
		"event": map[string]any{
			"sender": map[string]any{"sender_type": "user", "sender_id": map[string]any{"open_id": "ou_1"}},
			"message": func() map[string]any {
				m := map[string]any{"message_id": msgID, "chat_id": chatID, "chat_type": chatType, "message_type": "text", "content": string(content)}
				if mention {
					m["mentions"] = []any{map[string]any{"key": "@_user_1", "name": "claw", "id": map[string]any{"open_id": "ou_bot"}}}
				}
				return m
			}(),
		},
	}
	b, _ := json.Marshal(ev)
	return string(b)
}

func TestWebhookFlow(t *testing.T) {
	sender := &fakeSender{}
	var mu sync.Mutex
	var prompts []string
	bot := NewFeishuBot(sender, func(ctx context.Context, chatID, prompt string, r engine.Reporter) error {
		mu.Lock()
		prompts = append(prompts, chatID+":"+prompt)
		mu.Unlock()
		r.OnToolCall(ctx, "bash", `{"command":"ls"}`)
		r.OnToolResult(ctx, "bash", "a.txt", false)
		r.OnMessage(ctx, "完成："+prompt)
		return nil
	})
	srv := httptest.NewServer(http.HandlerFunc(httpserverext.NewEventHandlerFunc(bot.EventDispatcher("vt", ""))))
	defer srv.Close()

	// 1. URL 校验 challenge
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"challenge":"abc123","token":"vt","type":"url_verification"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "abc123") {
		t.Fatalf("challenge 校验失败: %s", b)
	}

	// 2. 两条消息 + 一条重复推送
	for _, body := range []string{
		eventBody("om_1", "oc_A", "@_user_1 帮我看 README"),
		eventBody("om_1", "oc_A", "@_user_1 帮我看 README"),
		eventBody("om_2", "oc_B", "查一下 IP"),
		eventBodyIn("om_3", "oc_G", "今天中午吃什么？", "group", false), // 群聊闲聊：不唤醒
		eventBodyIn("om_4", "oc_G", "@_user_1 看下磁盘", "group", true),
	} {
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("event post: %v %v", err, resp.Status)
		}
	}
	bot.Wait()
	if len(prompts) != 3 {
		t.Fatalf("应只处理 3 条（去重 + 群聊未 @ 过滤），got %v", prompts)
	}
	if strings.Contains(strings.Join(prompts, ","), "吃什么") || !strings.Contains(strings.Join(prompts, ","), "oc_G:看下磁盘") {
		t.Fatalf("意图过滤错误: %v", prompts)
	}
	joined := strings.Join(prompts, ",")
	if !strings.Contains(joined, "oc_A:帮我看 README") || !strings.Contains(joined, "oc_B:查一下 IP") {
		t.Fatalf("@占位符未剥离或会话错配: %v", prompts)
	}
	out := sender.all()
	for _, want := range []string{"oc_A|🛠️ 正在执行工具：bash", "oc_A|✅ 执行成功 (bash)", "oc_B|完成：查一下 IP"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少消息 %q:\n%s", want, out)
		}
	}
}

func TestCommandInterceptAndSerialize(t *testing.T) {
	sender := &fakeSender{}
	release := make(chan struct{})
	var running, maxRunning int
	var mu sync.Mutex
	bot := NewFeishuBot(sender, func(ctx context.Context, chatID, prompt string, r engine.Reporter) error {
		mu.Lock()
		running++
		if running > maxRunning {
			maxRunning = running
		}
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return fmt.Errorf("boom")
	})
	bot.UseCommand(func(ctx context.Context, chatID, text string) bool { return strings.HasPrefix(text, "approve ") })
	bot.HandleMessage(context.Background(), "m0", "oc", "approve call_1")
	bot.HandleMessage(context.Background(), "m1", "oc", "任务1")
	bot.HandleMessage(context.Background(), "m2", "oc", "任务2")
	close(release)
	bot.Wait()
	if maxRunning != 1 {
		t.Fatalf("同一会话的任务应串行，max=%d", maxRunning)
	}
	if !strings.Contains(sender.all(), "❌ Agent 运行崩溃: boom") {
		t.Fatalf("错误未回传: %s", sender.all())
	}
}

func TestParseTextContent(t *testing.T) {
	got, err := ParseTextContent(`{"text":"@_user_1 第一行\n\"引号\""}`)
	if err != nil || got != "第一行\n\"引号\"" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestApprovalCommandOnlyForTaskIDs(t *testing.T) {
	cmd := ApprovalCommand(permission.NewApprovalManager(0), &fakeSender{})
	if cmd(context.Background(), "oc", "同意 这个方案") {
		t.Fatal("普通对话不应被当作审批口令")
	}
	if !cmd(context.Background(), "oc", "approve ab12cd") {
		t.Fatal("合法口令应被拦截")
	}
}

// 第 16/22 讲：群聊里运维直接回复 "approve <id>"（不 @ 机器人）也必须生效，
// 审批口令要在"群聊需 @"的意图过滤之前处理，否则任务只能等 10 分钟超时自动拒绝。
// 普通闲聊仍然不唤醒 Agent，也不触发其他命令。
func TestGroupApprovalWithoutMentionReachesCommands(t *testing.T) {
	var mu sync.Mutex
	var commands, runs []string
	bot := NewFeishuBot(&fakeSender{}, func(ctx context.Context, chatID, prompt string, r engine.Reporter) error {
		mu.Lock()
		runs = append(runs, prompt)
		mu.Unlock()
		return nil
	})
	bot.UseCommand(func(_ context.Context, chatID, text string) bool {
		if strings.HasPrefix(text, "approve ") || text == "/reset" {
			mu.Lock()
			commands = append(commands, chatID+":"+text)
			mu.Unlock()
			return true
		}
		return false
	})
	srv := httptest.NewServer(http.HandlerFunc(httpserverext.NewEventHandlerFunc(bot.EventDispatcher("vt", ""))))
	defer srv.Close()
	for _, body := range []string{
		eventBodyIn("om_a", "oc_G", "approve abc123", "group", false),
		eventBodyIn("om_a", "oc_G", "approve abc123", "group", false), // 飞书重推：不能执行两次
		eventBodyIn("om_b", "oc_G", "今天中午吃什么", "group", false),
	} {
		if resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body)); err != nil || resp.StatusCode != 200 {
			t.Fatalf("event post: %v", err)
		}
	}
	bot.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 1 || commands[0] != "oc_G:approve abc123" {
		t.Fatalf("群聊未 @ 的审批口令应恰好处理一次: %v", commands)
	}
	if len(runs) != 0 {
		t.Fatalf("群聊未 @ 的消息不应唤醒 Agent: %v", runs)
	}
}

func TestCostSummary(t *testing.T) {
	if got := CostSummary(1200, 300, 0.0123, true); !strings.Contains(got, "1200") || !strings.Contains(got, "¥0.0123") {
		t.Fatalf("成本汇总格式不对: %q", got)
	}
	if got := CostSummary(0, 0, 0, false); strings.Contains(got, "¥0") {
		t.Fatalf("没有 Usage 时不能显示 ¥0: %q", got)
	}
}
