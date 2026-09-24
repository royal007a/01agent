// Package trace 是第 19 讲的极简链路追踪：借助 context.Context 自动挂接父子 Span，
// 把一次 Agent.Run 固化为可回放的 JSON 决策树。无第三方依赖，被 engine 等底层包直接引用。
package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type key struct{}

// Span 链路追踪中的一个时间跨度与操作节点。
type Span struct {
	Name       string         `json:"name"`
	StartTime  time.Time      `json:"start_time"`
	EndTime    time.Time      `json:"end_time"`
	DurationMs int64          `json:"duration_ms"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Children   []*Span        `json:"children,omitempty"`

	mu     sync.Mutex
	parent *Span
}

// Start 开启一个新 Span；若 ctx 中已有 Span 则自动挂为其子节点。
func Start(ctx context.Context, name string) (context.Context, *Span) {
	s := &Span{Name: name, StartTime: time.Now(), Attributes: map[string]any{}}
	if parent, ok := ctx.Value(key{}).(*Span); ok && parent != nil {
		s.parent = parent
		parent.mu.Lock()
		parent.Children = append(parent.Children, s)
		parent.mu.Unlock()
	}
	return context.WithValue(ctx, key{}, s), s
}

// FromContext 取出当前 Span（可能为 nil）。
func FromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(key{}).(*Span)
	return s
}

// End 结束 Span 并计算耗时；重复调用只以第一次为准。
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.EndTime.IsZero() {
		return
	}
	s.EndTime = time.Now()
	s.DurationMs = s.EndTime.Sub(s.StartTime).Milliseconds()
}

// Set 记录元数据（并发安全，nil Span 安全）。
func (s *Span) Set(k string, v any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Attributes[k] = v
	s.mu.Unlock()
}

// Add 对数值型属性做累加。
func (s *Span) Add(k string, delta int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	n, _ := s.Attributes[k].(int)
	s.Attributes[k] = n + delta
	s.mu.Unlock()
}

// Get 读取属性。
func (s *Span) Get(k string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Attributes[k]
}

// Walk 深度优先遍历整棵树（导出/统计用，调用时树应已结束）。
func (s *Span) Walk(fn func(sp *Span, depth int)) {
	var rec func(*Span, int)
	rec = func(sp *Span, d int) {
		fn(sp, d)
		sp.mu.Lock()
		children := append([]*Span(nil), sp.Children...)
		sp.mu.Unlock()
		for _, c := range children {
			rec(c, d+1)
		}
	}
	rec(s, 0)
}

// Count 统计树中名为 name 的 Span 数量。
func (s *Span) Count(name string) int {
	n := 0
	s.Walk(func(sp *Span, _ int) {
		if sp.Name == name {
			n++
		}
	})
	return n
}

// MarshalJSON 在锁内序列化，避免与仍在写入的子节点竞争。
func (s *Span) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	type alias struct {
		Name       string         `json:"name"`
		StartTime  time.Time      `json:"start_time"`
		EndTime    time.Time      `json:"end_time"`
		DurationMs int64          `json:"duration_ms"`
		Attributes map[string]any `json:"attributes,omitempty"`
		Children   []*Span        `json:"children,omitempty"`
	}
	attrs := make(map[string]any, len(s.Attributes))
	for k, v := range s.Attributes {
		attrs[k] = v
	}
	a := alias{s.Name, s.StartTime, s.EndTime, s.DurationMs, attrs, append([]*Span(nil), s.Children...)}
	s.mu.Unlock()
	return json.Marshal(a)
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// Export 把整棵树写成两份文件：
//   - trace_<session>_<ts>.json：课程原版的嵌套决策树，便于人读/grep；
//   - trace_<session>_<ts>.perfetto.json：Chrome Trace Event 格式（第 19 讲思考题），
//     拖进 https://ui.perfetto.dev 或 chrome://tracing 即可看到并发工具重叠的甘特图。
//
// 返回决策树文件路径。
func Export(root *Span, dir, sessionID string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(dir, fmt.Sprintf("trace_%s_%s", unsafe.ReplaceAllString(sessionID, "_"), root.StartTime.Format("20060102-150405.000")))
	tree, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(base+".json", tree, 0o644); err != nil {
		return "", err
	}
	events, err := json.Marshal(map[string]any{"traceEvents": chromeEvents(root), "displayTimeUnit": "ms"})
	if err == nil {
		_ = os.WriteFile(base+".perfetto.json", events, 0o644)
	}
	return base + ".json", nil
}

// chromeEvents 转换为 Trace Event "X"（complete）事件。
// 并发的兄弟 Span 分配到不同的 tid 轨道，才能在甘特图里并排显示。
func chromeEvents(root *Span) []map[string]any {
	var out []map[string]any
	origin := root.StartTime
	nextTid := 1
	var rec func(sp *Span, tid int)
	rec = func(sp *Span, tid int) {
		sp.mu.Lock()
		end := sp.EndTime
		if end.IsZero() {
			end = time.Now()
		}
		args := map[string]any{}
		for k, v := range sp.Attributes {
			args[k] = v
		}
		children := append([]*Span(nil), sp.Children...)
		sp.mu.Unlock()
		out = append(out, map[string]any{
			"name": sp.Name, "ph": "X", "pid": 1, "tid": tid,
			"ts": sp.StartTime.Sub(origin).Microseconds(), "dur": end.Sub(sp.StartTime).Microseconds(), "args": args,
		})
		// 与前一个兄弟时间重叠的子节点放到新轨道
		var prevEnd time.Time
		for _, c := range children {
			ct := tid
			if !prevEnd.IsZero() && c.StartTime.Before(prevEnd) {
				nextTid++
				ct = nextTid
			}
			rec(c, ct)
			c.mu.Lock()
			if c.EndTime.After(prevEnd) {
				prevEnd = c.EndTime
			}
			c.mu.Unlock()
		}
	}
	rec(root, 1)
	return out
}
