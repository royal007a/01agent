// Package provider 是模型适配层：把内部 schema.Message 翻译成各厂商协议，再翻译回来。
package provider

import (
	"context"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// LLMProvider 定义了与大模型通信的统一契约。
//
// 当 availableTools 为 nil 或长度为 0 时，代表引擎正在强制模型进入慢思考阶段（第 03 讲），
// 适配器不得向模型暴露任何工具 Schema。
type LLMProvider interface {
	Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error)
}
