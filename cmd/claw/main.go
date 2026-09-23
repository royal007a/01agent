package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// mockRegistry 第 04 讲：伪造的天气工具，用于验证 Provider 的工具提取能力。
type mockRegistry struct{}

func (m *mockRegistry) GetAvailableTools() []schema.ToolDefinition {
	return []schema.ToolDefinition{{
		Name:        "get_weather",
		Description: "获取指定城市的当前天气情况。",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"city": map[string]interface{}{"type": "string"}},
			"required":   []string{"city"},
		},
	}}
}

func (m *mockRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	log.Printf("  -> [Mock 工具执行] %s %s", call.Name, call.Arguments)
	return schema.ToolResult{ToolCallID: call.ID, Output: "API 返回：今天是晴天，气温 25 度。"}
}

func main() {
	prompt := flag.String("prompt", "我想去北京跑步，帮我查查天气适合吗？", "任务")
	thinking := flag.Bool("thinking", false, "开启慢思考")
	flag.Parse()
	p, err := provider.New(provider.ConfigFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	workDir, _ := os.Getwd()
	eng := engine.NewAgentEngine(p, &mockRegistry{}, workDir, *thinking)
	if err := eng.Run(context.Background(), *prompt); err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}
