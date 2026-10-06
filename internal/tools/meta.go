package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ToolMeta 是 Registry 的最小执行契约。它刻意不引入完整 JSON-Schema 引擎，
// 先覆盖生产中最容易造成事故的副作用、超时、输出和重试边界。
type ToolMeta struct {
	SideEffect     string // none, local, external
	TimeoutSeconds int
	MaxOutputBytes int
	Retryable      bool
}

type MetaProvider interface{ Meta() ToolMeta }

func metaOf(t BaseTool) ToolMeta {
	if m, ok := t.(MetaProvider); ok {
		return m.Meta()
	}
	if r, ok := t.(ReadOnlyTool); ok && r.ReadOnly() {
		return ToolMeta{SideEffect: "none", MaxOutputBytes: 8000}
	}
	return ToolMeta{SideEffect: "local", MaxOutputBytes: 8000}
}

// validateArgs 做最小确定性校验；具体工具仍负责业务语义校验。
func validateArgs(def schemaLike, raw json.RawMessage) error {
	if len(def.Required()) == 0 && len(def.Properties()) == 0 {
		// 兼容测试工具/无参数工具：具体 Execute 仍负责自己的解析。
		return nil
	}
	var obj map[string]any
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte(`{}`)
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	for _, key := range def.Required() {
		if _, ok := obj[key]; !ok {
			return fmt.Errorf("缺少必填参数 %q", key)
		}
	}
	for name, typ := range def.Properties() {
		v, ok := obj[name]
		if !ok {
			continue
		}
		if typ == "string" {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("参数 %q 必须是 string", name)
			}
		}
		if typ == "boolean" {
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("参数 %q 必须是 boolean", name)
			}
		}
		if typ == "number" || typ == "integer" {
			if _, ok := v.(float64); !ok {
				return fmt.Errorf("参数 %q 必须是 number", name)
			}
		}
	}
	return nil
}

type schemaLike interface {
	Required() []string
	Properties() map[string]string
}
type inputSchema struct {
	required   []string
	properties map[string]string
}

func (s inputSchema) Required() []string            { return s.required }
func (s inputSchema) Properties() map[string]string { return s.properties }

func parseSchema(v interface{}) inputSchema {
	m, _ := v.(map[string]interface{})
	out := inputSchema{properties: map[string]string{}}
	if xs, ok := m["required"].([]string); ok {
		out.required = xs
	}
	if xs, ok := m["required"].([]interface{}); ok {
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out.required = append(out.required, s)
			}
		}
	}
	if ps, ok := m["properties"].(map[string]interface{}); ok {
		for k, raw := range ps {
			if p, ok := raw.(map[string]interface{}); ok {
				if t, ok := p["type"].(string); ok {
					out.properties[k] = t
				}
			}
		}
	}
	return out
}

// WriteLock 是按工作区隔离的可取消写锁。它由 Registry 在权限/审批 middleware
// 全部通过后才获取，因此等待审批不会占锁。
type WriteLock struct {
	once sync.Once
	ch   chan struct{}
}

func (l *WriteLock) Acquire(ctx context.Context) error {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (l *WriteLock) Release() { <-l.ch }

func normalizeMeta(m ToolMeta) ToolMeta {
	m.SideEffect = strings.ToLower(strings.TrimSpace(m.SideEffect))
	if m.SideEffect == "" {
		m.SideEffect = "local"
	}
	if m.MaxOutputBytes <= 0 {
		m.MaxOutputBytes = 8000
	}
	return m
}
