// Package feishu 是飞书集成层：把 go-tiny-claw 接入飞书机器人的事件流（第 09 讲）。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// Sender 抽象"往某个会话发一条文本"，便于测试时替换掉真实的飞书 OpenAPI。
type Sender interface {
	SendText(ctx context.Context, chatID, text string) error
}

// LarkSender 基于飞书官方 SDK 发送消息。
type LarkSender struct{ Client *lark.Client }

func NewLarkSender(appID, appSecret string) *LarkSender {
	return &LarkSender{Client: lark.NewClient(appID, appSecret)}
}

func (s *LarkSender) SendText(ctx context.Context, chatID, text string) error {
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeText).
			Content(string(content)).
			Build()).
		Build()
	resp, err := s.Client.Im.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("飞书发送失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// RunFunc 把一次用户指令交给引擎处理。Bot 不关心引擎如何管理会话/记忆，由组装方注入。
type RunFunc func(ctx context.Context, chatID, prompt string, reporter engine.Reporter) error

// CommandFunc 允许在进入 Agent 之前拦截特殊口令（第 16 讲的 approve/reject）。
// 返回 handled=true 表示已处理，不再启动 Agent。
type CommandFunc func(ctx context.Context, chatID, text string) (handled bool)

// FeishuBot 封装了飞书机器人的事件监听与业务流。
type FeishuBot struct {
	sender   Sender
	run      RunFunc
	commands []CommandFunc

	// chatLocks 让同一个会话的任务排队串行（第 09 讲思考题：防止多条指令并发改同一工作区）。
	chatLocks sync.Map // chatID -> *sync.Mutex
	// seen 事件去重：飞书在回调超时时会重推同一条消息。
	seenMu sync.Mutex
	seen   map[string]struct{}
	order  []string
	wg     sync.WaitGroup
}

func NewFeishuBot(sender Sender, run RunFunc) *FeishuBot {
	return &FeishuBot{sender: sender, run: run, seen: make(map[string]struct{})}
}

// UseCommand 注册一个口令拦截器。
func (b *FeishuBot) UseCommand(c CommandFunc) { b.commands = append(b.commands, c) }

// Sender 暴露发送器，供审批等模块复用。
func (b *FeishuBot) Sender() Sender { return b.sender }

// Wait 等待所有后台任务结束（测试/优雅退出用）。
func (b *FeishuBot) Wait() { b.wg.Wait() }

var mentionRe = regexp.MustCompile(`@_user_\d+`)

// ParseTextContent 解析飞书 text 消息的 content JSON，并去掉 @机器人 占位符。
// 课程用 TrimPrefix(`{"text":"`) 粗暴剥壳，遇到换行/引号转义会出错，这里用 JSON 解析。
func ParseTextContent(raw string) (string, error) {
	var c struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return "", err
	}
	return strings.TrimSpace(mentionRe.ReplaceAllString(c.Text, "")), nil
}

func (b *FeishuBot) markSeen(id string) bool {
	if id == "" {
		return false
	}
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	if _, ok := b.seen[id]; ok {
		return true
	}
	b.seen[id] = struct{}{}
	b.order = append(b.order, id)
	if len(b.order) > 1024 {
		delete(b.seen, b.order[0])
		b.order = b.order[1:]
	}
	return false
}

// HandleMessage 处理一条已解析的用户消息（事件回调与测试共用）。
func (b *FeishuBot) HandleMessage(ctx context.Context, messageID, chatID, text string) {
	if b.markSeen(messageID) {
		log.Printf("[Feishu] 重复事件 %s，忽略", messageID)
		return
	}
	log.Printf("[Feishu] 收到会话 %s 消息: %s", chatID, textutil.Truncate(text, 200, "..."))
	for _, c := range b.commands {
		if c(ctx, chatID, text) {
			return
		}
	}
	// 驾驭并发：绝不能阻塞回调，每条指令一个 Goroutine；同一会话内部排队。
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.handleAgentRun(chatID, text)
	}()
}

func (b *FeishuBot) handleAgentRun(chatID, prompt string) {
	lockAny, _ := b.chatLocks.LoadOrStore(chatID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	if !lock.TryLock() {
		_ = b.sender.SendText(context.Background(), chatID, "⏳ 上一个任务还在执行，本条指令已排队。")
		lock.Lock()
	}
	defer lock.Unlock()

	reporter := NewFeishuReporter(b.sender, chatID)
	if err := b.run(context.Background(), chatID, prompt, reporter); err != nil {
		reporter.Send(fmt.Sprintf("❌ Agent 运行崩溃: %v", err))
	}
}

// EventDispatcher 构造飞书官方事件调度器，监听"接收消息"事件。
func (b *FeishuBot) EventDispatcher(verifyToken, encryptKey string) *dispatcher.EventDispatcher {
	return dispatcher.NewEventDispatcher(verifyToken, encryptKey).
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			msg := event.Event.Message
			if msg == nil || msg.ChatId == nil {
				return nil
			}
			if s := event.Event.Sender; s != nil && s.SenderType != nil && *s.SenderType != "user" {
				return nil // 忽略机器人自己/其他机器人的消息，防止回环
			}
			if msg.MessageType != nil && *msg.MessageType != "text" {
				_ = b.sender.SendText(ctx, *msg.ChatId, "目前只支持纯文本指令。")
				return nil
			}
			text, err := ParseTextContent(strOr(msg.Content))
			if err != nil || text == "" {
				return nil
			}
			b.HandleMessage(ctx, strOr(msg.MessageId), *msg.ChatId, text)
			return nil
		}).
		OnP2MessageReadV1(func(ctx context.Context, event *larkim.P2MessageReadV1) error {
			return nil // 已读事件静默忽略
		})
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
