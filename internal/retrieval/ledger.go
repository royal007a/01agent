package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type LedgerStore struct {
	root string
	mu   sync.Mutex
}

func NewLedgerStore(root string) *LedgerStore { return &LedgerStore{root: root} }
func QueryFingerprint(query string, filters map[string]string, sources []string) string {
	var b strings.Builder
	b.WriteString(strings.Join(strings.Fields(strings.ToLower(query)), " "))
	keys := make([]string, 0, len(filters))
	for k := range filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strings.TrimSpace(filters[k]))
	}
	b.WriteByte('|')
	b.WriteString(strings.Join(sources, ","))
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}
func (s *LedgerStore) path(session, task string) string {
	return filepath.Join(s.root, fileName(session), fileName(task)+".json")
}
func fileName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		v = "_"
	}
	h := sha256.Sum256([]byte(v))
	return strings.NewReplacer("/", "_", "\\", "_").Replace(v) + "-" + hex.EncodeToString(h[:4])
}
func (s *LedgerStore) Load(session, task string) (State, error) {
	if session == "" || task == "" {
		return State{}, fmt.Errorf("retrieval ledger requires session and task")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path(session, task))
	if os.IsNotExist(err) {
		return State{Evidence: map[string]Evidence{}, Status: "running", Budget: Budget{StartedAt: time.Now()}}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err = json.Unmarshal(b, &st); err != nil {
		return State{}, err
	}
	if st.Evidence == nil {
		st.Evidence = map[string]Evidence{}
	}
	return st, nil
}
func (s *LedgerStore) Save(session, task string, st State) error {
	if session == "" || task == "" {
		return fmt.Errorf("retrieval ledger requires session and task")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.path(session, task)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Update performs a read-modify-write while holding the store lock, so
// concurrent search/evidence calls cannot lose ledger entries.
func (s *LedgerStore) Update(session, task string, fn func(*State) error) error {
	if session == "" || task == "" {
		return fmt.Errorf("retrieval ledger requires session and task")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.path(session, task)
	st := State{Evidence: map[string]Evidence{}, Status: "running", Budget: Budget{StartedAt: time.Now()}}
	if b, err := os.ReadFile(p); err == nil {
		if err = json.Unmarshal(b, &st); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if st.Evidence == nil {
		st.Evidence = map[string]Evidence{}
	}
	if err := fn(&st); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
