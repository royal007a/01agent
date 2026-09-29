package knowledge

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Inbox 会话级暂存区：内层循环写入，外层循环取走归位。
// 放在 Harness 状态目录（工作区之外），模型无法用文件工具篡改。
type Inbox struct {
	root string
	mu   sync.Mutex
}

func NewInbox(root string) *Inbox { return &Inbox{root: root} }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func (b *Inbox) path(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(b.root, unsafeName.ReplaceAllString(sessionID, "_")+"-"+hex.EncodeToString(sum[:4])+".jsonl")
}

// Record 校验并追加一条知识（kind=none 也会记录，作为"本轮已清点"的凭据）。
func (b *Inbox) Record(e Entry) (Entry, error) {
	if e.SessionID == "" {
		return e, fmt.Errorf("记录知识需要会话身份")
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	if e.RecordedAt.IsZero() {
		e.RecordedAt = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(b.root, 0o755); err != nil {
		return e, err
	}
	f, err := os.OpenFile(b.path(e.SessionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return e, err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(e); err != nil {
		return e, err
	}
	return e, nil
}

// Pending 返回会话暂存区里尚未归位的条目。
func (b *Inbox) Pending(sessionID string) ([]Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read(sessionID)
}

func (b *Inbox) read(sessionID string) ([]Entry, error) {
	f, err := os.Open(b.path(sessionID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // 损坏行跳过，不影响其余条目
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// CheckedSince 本会话自 since 以来是否已经做过清点（记录了任意条目，含 none）。
func (b *Inbox) CheckedSince(sessionID string, since time.Time) bool {
	es, _ := b.Pending(sessionID)
	for _, e := range es {
		if !e.RecordedAt.Before(since) {
			return true
		}
	}
	return false
}

// Take 取出全部待归位条目并清空暂存区；只有 commit 返回 nil 才真正清空，失败时保留以便下次重试。
func (b *Inbox) Take(sessionID string, commit func([]Entry) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	es, err := b.read(sessionID)
	if err != nil || len(es) == 0 {
		return err
	}
	if err := commit(es); err != nil {
		return err
	}
	return os.Remove(b.path(sessionID))
}
