// Package tools 是工具与执行层：注册、描述暴露与路由分发。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// BaseTool 是所有具体工具必须实现的通用接口（第 05 讲）。
type BaseTool interface {
	// Name 返回工具的全局唯一名称（大模型通过这个名字调用它）
	Name() string
	// Definition 返回用于提交给大模型的工具元信息和参数 JSON Schema
	Definition() schema.ToolDefinition
	// Execute 接收大模型吐出的 JSON 参数，执行具体逻辑；反序列化由工具内部自行处理
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// ReadOnlyTool 是可选接口：声明自己没有副作用的工具，可以被引擎并发执行（第 08 讲"只读并发、涉写串行"）。
type ReadOnlyTool interface {
	ReadOnly() bool
}

// MiddlewareFunc 在工具真正执行前拦截调用（第 16 讲）。
// 返回 allowed=false 时底层 Execute 绝不会被触发，rejectReason 会作为错误回灌给模型。
type MiddlewareFunc func(ctx context.Context, call schema.ToolCall) (allowed bool, rejectReason string)

// Registry 定义了工具的注册与分发接口。
type Registry interface {
	// Register 挂载一个新的工具到系统中
	Register(tool BaseTool)
	// Use 挂载一个全局 Middleware，按挂载顺序依次执行
	Use(mw MiddlewareFunc)
	// GetAvailableTools 返回当前系统挂载的所有工具的 Schema
	GetAvailableTools() []schema.ToolDefinition
	// Execute 实际路由并执行模型请求的工具调用
	Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
	// IsReadOnly 判断某个工具是否只读（未注册的工具视为非只读）
	IsReadOnly(name string) bool
}

type registryImpl struct {
	mu    sync.RWMutex
	tools map[string]BaseTool
	// order 保持注册顺序：课程用 map 遍历导出 Schema，顺序随机，
	// 会让每轮请求的 tools 字段抖动，破坏提示词缓存（prompt cache）。
	order       []string
	middlewares []MiddlewareFunc
}

func (r *registryImpl) Use(mw MiddlewareFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middlewares = append(r.middlewares, mw)
}

func NewRegistry() Registry {
	return &registryImpl{tools: make(map[string]BaseTool)}
}

func (r *registryImpl) Register(tool BaseTool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := tool.Name()
	if _, exists := r.tools[name]; exists {
		log.Printf("[Warning] 工具 '%s' 已经被注册，将被覆盖。", name)
	} else {
		r.order = append(r.order, name)
	}
	r.tools[name] = tool
	log.Printf("[Registry] 成功挂载工具: %s", name)
}

func (r *registryImpl) GetAvailableTools() []schema.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]schema.ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name].Definition())
	}
	return defs
}

func (r *registryImpl) IsReadOnly(name string) bool {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	ro, ok := t.(ReadOnlyTool)
	return ok && ro.ReadOnly()
}

func (r *registryImpl) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	// 1. 路由查找：找不到说明模型产生了幻觉，直接向模型抛出错误让它纠正
	r.mu.RLock()
	tool, exists := r.tools[call.Name]
	mws := append([]MiddlewareFunc(nil), r.middlewares...)
	r.mu.RUnlock()
	if !exists {
		return schema.ToolResult{
			ToolCallID: call.ID,
			Output:     fmt.Sprintf("Error: 系统中不存在名为 '%s' 的工具。", call.Name),
			IsError:    true,
		}
	}
	// 2. 核心防御：执行前依次运行所有 Middleware，任何一个拒绝即拦截
	for _, mw := range mws {
		if allowed, reason := mw(ctx, call); !allowed {
			log.Printf("[Registry] ⚠️ 工具 %s 被 Middleware 拦截: %s", call.Name, reason)
			return schema.ToolResult{
				ToolCallID: call.ID,
				Output:     fmt.Sprintf("执行被系统拦截。原因: %s", reason),
				IsError:    true, // 必须是 Error，强制大模型阅读拒绝理由
			}
		}
	}
	// 3. 执行工具逻辑：将原始 JSON 字节流直接丢给具体工具
	output, err := tool.Execute(ctx, call.Arguments)
	// 4. 封装结果
	if err != nil {
		return schema.ToolResult{
			ToolCallID: call.ID,
			Output:     fmt.Sprintf("Error executing %s: %v", call.Name, err),
			IsError:    true,
		}
	}
	return schema.ToolResult{ToolCallID: call.ID, Output: output}
}
