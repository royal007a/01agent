// Package tools 是工具与执行层：注册、描述暴露与路由分发。
package tools

import (
	"context"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// Registry 定义了工具的注册与分发执行接口。
type Registry interface {
	// GetAvailableTools 返回当前系统挂载的所有可用工具的 Schema
	GetAvailableTools() []schema.ToolDefinition
	// Execute 实际执行模型请求的工具，并返回结果
	Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
