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
	ch      chan ApprovalResult
	scope   string // 只有同一 scope（如同一个飞书会话）的回复才能解除阻塞
	summary string
	since   time.Time
}

// PendingInfo 对外展示的待审批任务。
type PendingInfo struct {
	TaskID  string    `json:"task_id"`
	Scope   string    `json:"scope"`
	Summary string    `json:"summary"`
	Since   time.Time `json:"since"`
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
	return m.WaitWithSummary(ctx, taskID, scope, "", notify)
}

// WaitWithSummary 同 Wait，并附带一段可供列表展示的摘要。
func (m *ApprovalManager) WaitWithSummary(ctx context.Context, taskID, scope, summary string, notify func()) (bool, string) {
	ch := make(chan ApprovalResult, 1) // 容量为 1，Resolve 永不阻塞
	m.mu.Lock()
	m.pending[taskID] = &pending{ch: ch, scope: scope, summary: summary, since: time.Now()}
	m.mu.Unlock()
	// finish 在锁内注销任务，并做最后一次非阻塞接收：
	// 若 Resolve 已抢在超时/取消之前把结果放进了 channel，以人类的决定为准，
	// 避免出现"人类看到已批准、实际却被自动拒绝"的不一致。
	finish := func(fallback ApprovalResult) (bool, string) {
		m.mu.Lock()
		delete(m.pending, taskID)
		m.mu.Unlock()
		select {
		case r := <-ch:
			return r.Allowed, r.Reason
		default:
			return fallback.Allowed, fallback.Reason
		}
	}

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
		m.mu.Lock()
		delete(m.pending, taskID)
		m.mu.Unlock()
		return r.Allowed, r.Reason
	case <-timer.C:
		return finish(ApprovalResult{Reason: fmt.Sprintf("审批超时（%s 内无人响应），操作已自动拒绝。", timeout)})
	case <-ctx.Done():
		return finish(ApprovalResult{Reason: "任务已取消，审批作废。"})
	}
}

// Resolve 由人类回复触发。scope 不匹配时拒绝解锁，防止别的群随手批准。
func (m *ApprovalManager) Resolve(taskID, scope string, allowed bool, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock() // 持锁发送：与 Wait 的注销互斥，保证结果要么被消费、要么报"找不到"
	p, ok := m.pending[taskID]
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

// List 列出某个 scope（为空则全部）下等待中的任务。
func (m *ApprovalManager) List(scope string) []PendingInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []PendingInfo{}
	for id, p := range m.pending {
		if scope == "" || p.scope == scope {
			out = append(out, PendingInfo{TaskID: id, Scope: p.scope, Summary: p.summary, Since: p.since})
		}
	}
	return out
}

// Pending 返回当前等待中的任务数。
func (m *ApprovalManager) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}
