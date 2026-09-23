package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"
)

// ApprovalResult 审批结果。
type ApprovalResult struct {
	Allowed bool
	Reason  string
}

type pending struct {
	ch    chan ApprovalResult
	scope string // 只有同一 scope（如同一个飞书会话）的回复才能解除阻塞
}

// ApprovalManager 基于 channel 的跨协程审批中枢：引擎协程挂起等待，Webhook 协程负责唤醒。
type ApprovalManager struct {
	mu      sync.Mutex
	pending map[string]*pending
	Timeout time.Duration
}

func NewApprovalManager(timeout time.Duration) *ApprovalManager {
	return &ApprovalManager{pending: make(map[string]*pending), Timeout: timeout}
}

// NewTaskID 生成人类好输入的短 ID（课程直接用 ToolCallID，形如 call_-7682501941879891797，手打很痛苦）。
func NewTaskID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Wait 注册一个待审批任务，调用 notify 把请求发给人类，然后阻塞直到被 Resolve、超时或 ctx 取消。
func (m *ApprovalManager) Wait(ctx context.Context, taskID, scope string, notify func()) (bool, string) {
	ch := make(chan ApprovalResult, 1) // 容量为 1，Resolve 永不阻塞
	m.mu.Lock()
	m.pending[taskID] = &pending{ch: ch, scope: scope}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.pending, taskID)
		m.mu.Unlock()
	}()

	notify()
	log.Printf("[Approval] 已发送审批请求 (TaskID: %s)，协程挂起等待...", taskID)
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.Allowed, r.Reason
	case <-timer.C:
		return false, fmt.Sprintf("审批超时（%s 内无人响应），操作已自动拒绝。", timeout)
	case <-ctx.Done():
		return false, "任务已取消，审批作废。"
	}
}

// Resolve 由人类回复触发。scope 不匹配时拒绝解锁，防止别的群随手批准。
func (m *ApprovalManager) Resolve(taskID, scope string, allowed bool, reason string) error {
	m.mu.Lock()
	p, ok := m.pending[taskID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("找不到待审批任务 %s，可能已超时或处理完毕", taskID)
	}
	if p.scope != scope {
		return fmt.Errorf("任务 %s 不属于当前会话，无权审批", taskID)
	}
	select {
	case p.ch <- ApprovalResult{Allowed: allowed, Reason: reason}:
		log.Printf("[Approval] 收到审批结果 (TaskID: %s, Allowed: %v)", taskID, allowed)
		return nil
	default:
		return fmt.Errorf("任务 %s 已经被审批过了", taskID)
	}
}

// Pending 返回当前等待中的任务数。
func (m *ApprovalManager) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}
