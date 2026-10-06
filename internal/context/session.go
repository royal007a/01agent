package context

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// Session 代表一次持续的人机交互过程，负责维护该会话的完整历史（第 11 讲）。
type Session struct {
	ID        string
	WorkDir   string // 该会话绑定的物理工作区
	CreatedAt time.Time
	UpdatedAt time.Time

	// 成本统计（第 18 讲）
	TotalPromptTokens     int
	TotalCompletionTokens int
	TotalCost             float64
	UsageKnown            bool // provider 至少返回过一次 Usage；未知时不能把 token 当作 0

	history []schema.Message
	mu      sync.RWMutex
	// logPath 非空时，每次 Append 都会以 JSONL 追加落盘（课程里的"持久化预留点"）。
	logPath string
}

func NewSession(id, workDir string) *Session {
	now := time.Now()
	return &Session{ID: id, WorkDir: workDir, CreatedAt: now, UpdatedAt: now}
}

// Append 线程安全地向 Session 追加消息；全量真实数据永远写在这里，不受 Compactor 影响。
func (s *Session) Append(msgs ...schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, msgs...)
	s.UpdatedAt = time.Now()
	if s.logPath != "" {
		if err := appendJSONL(s.logPath, msgs); err != nil {
			log.Printf("[Session] 持久化失败: %v", err)
		}
	}
}

// Len 返回历史消息条数。
func (s *Session) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.history)
}

// History 返回全量历史的拷贝。
func (s *Session) History() []schema.Message {
	return s.GetWorkingMemory(0)
}

// GetWorkingMemory 从后往前截取最近的 limit 条消息，形成"短期工作记忆"。limit<=0 表示全量。
func (s *Session) GetWorkingMemory(limit int) []schema.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := len(s.history)
	if limit <= 0 || total <= limit {
		res := make([]schema.Message, total)
		copy(res, s.history)
		return res
	}
	res := make([]schema.Message, limit)
	copy(res, s.history[total-limit:])
	// 驾驭防线：截断后首条若是"孤儿"工具结果（对应的 ToolCall 已被截掉），
	// 大模型 API 会直接 400。必须舍弃，顺延到下一条正常消息。
	for len(res) > 0 && res[0].IsToolResult() {
		res = res[1:]
	}
	return res
}

// RecordUsage 累加账单（由 CostTracker 调用）。
func (s *Session) RecordUsage(prompt, completion int, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalPromptTokens += prompt
	s.TotalCompletionTokens += completion
	s.TotalCost += cost
	s.UsageKnown = true
}

// UsageSnapshot 线程安全地读取累计账单。
func (s *Session) UsageSnapshot() (prompt, completion int, cost float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.TotalPromptTokens, s.TotalCompletionTokens, s.TotalCost
}

// UsageAvailable 表示当前会话是否拿到过模型 Usage。部分本地模型不返回 Usage，
// 此时预算守门只能依赖轮数和墙钟时间，不能静默按零 token 计算。
func (s *Session) UsageAvailable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.UsageKnown
}

// ==========================================
// SessionManager：多用户/多终端隔离
// ==========================================

type SessionManager struct {
	sessions map[string]*Session
	mu       sync.Mutex
	// persistDir 非空时，会话以 <persistDir>/<id>.jsonl 落盘，并在进程重启后自动恢复。
	persistDir string
}

// GlobalSessionMgr 纯内存的全局会话管理器（课程默认）。
var GlobalSessionMgr = NewSessionManager("")

func NewSessionManager(persistDir string) *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session), persistDir: persistDir}
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// fileNameFor 把会话 ID 映射为文件名：可读前缀 + ID 哈希，
// 避免 "a/b" 与 "a_b" 这类经过字符替换后相同的 ID 共用同一个历史文件。
func fileNameFor(id string) string {
	sum := sha256.Sum256([]byte(id))
	return unsafeID.ReplaceAllString(id, "_") + "-" + hex.EncodeToString(sum[:4]) + ".jsonl"
}

// GetOrCreate 获取或创建一个会话。
func (sm *SessionManager) GetOrCreate(id, workDir string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sess, ok := sm.sessions[id]; ok {
		return sess
	}
	sess := NewSession(id, workDir)
	if sm.persistDir != "" {
		if err := os.MkdirAll(sm.persistDir, 0o755); err == nil {
			sess.logPath = filepath.Join(sm.persistDir, fileNameFor(id))
			msgs, err := readJSONL(sess.logPath)
			if err != nil && !os.IsNotExist(err) {
				// 典型场景：进程在 Append 中途被杀，尾部留下半行 JSON。
				// 保留已解析的合法前缀，把原文件隔离备份，再用合法前缀重写，而不是整段丢弃。
				backup := fmt.Sprintf("%s.corrupt-%d", sess.logPath, time.Now().Unix())
				if rerr := os.Rename(sess.logPath, backup); rerr == nil {
					if werr := appendJSONL(sess.logPath, msgs); werr != nil {
						log.Printf("[Session] 重写会话 %s 失败: %v", id, werr)
					}
				}
				log.Printf("[Session] ⚠️ 会话 %s 的历史文件损坏（%v），已保留前 %d 条合法消息，原文件备份为 %s", id, err, len(msgs), backup)
			}
			if len(msgs) > 0 {
				sess.history = msgs
				log.Printf("[Session] 从磁盘恢复会话 %s：%d 条消息", id, len(msgs))
			}
		}
	}
	sm.sessions[id] = sess
	return sess
}

// Reset 清空某个会话（内存与磁盘）。
func (sm *SessionManager) Reset(id string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sess, ok := sm.sessions[id]; ok {
		sess.mu.Lock()
		sess.history = nil
		if sess.logPath != "" {
			_ = os.Remove(sess.logPath)
		}
		sess.mu.Unlock()
		return
	}
	// A service restart may leave the session only on disk. Reset must remove
	// that persisted history too; otherwise GetOrCreate resurrects it later.
	if sm.persistDir != "" {
		if err := os.Remove(filepath.Join(sm.persistDir, fileNameFor(id))); err != nil && !os.IsNotExist(err) {
			log.Printf("[Session] 重置持久化会话 %s 失败: %v", id, err)
		}
	}
}

func appendJSONL(path string, msgs []schema.Message) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return nil
}

func readJSONL(path string) ([]schema.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []schema.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var m schema.Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return out, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
		out = append(out, m)
	}
	return out, sc.Err()
}
