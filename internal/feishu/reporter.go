package feishu

import (
	"context"
	"fmt"
	"log"

	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// maxFeishuText 单条消息长度上限（飞书文本消息约 150KB，这里保守截断以免刷屏）。
const maxFeishuText = 4000

// FeishuReporter 将引擎的输出格式化后发给对应的飞书会话。
type FeishuReporter struct {
	sender    Sender
	chatID    string
	approvals *permission.ApprovalManager
}

func NewFeishuReporter(sender Sender, chatID string) *FeishuReporter {
	return &FeishuReporter{sender: sender, chatID: chatID}
}

// ChatID 返回该 Reporter 绑定的会话。
func (r *FeishuReporter) ChatID() string { return r.chatID }

// Send 直接发送一条文本。
func (r *FeishuReporter) Send(text string) {
	if err := r.sender.SendText(context.Background(), r.chatID, textutil.Truncate(text, maxFeishuText, "\n...(已截断)")); err != nil {
		log.Printf("[Feishu] 发送消息失败: %v", err)
	}
}

func (r *FeishuReporter) OnThinking(ctx context.Context) {
	r.Send("🤔 模型正在慢思考 (Thinking)...")
}

func (r *FeishuReporter) OnToolCall(ctx context.Context, toolName string, args string) {
	r.Send(fmt.Sprintf("🛠️ 正在执行工具：%s\n参数：%s", toolName, textutil.Truncate(args, 500, "...")))
}

func (r *FeishuReporter) OnToolResult(ctx context.Context, toolName string, result string, isError bool) {
	if isError {
		r.Send(fmt.Sprintf("⚠️ 执行报错 (%s)：\n%s", toolName, result))
		return
	}
	r.Send(fmt.Sprintf("✅ 执行成功 (%s)", toolName)) // 成功时不刷全量日志
}

func (r *FeishuReporter) OnMessage(ctx context.Context, content string) {
	r.Send(content)
}

// 编译时类型检查
var _ engine.Reporter = (*FeishuReporter)(nil)
