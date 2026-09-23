package provider

import (
	"fmt"
	"os"
)

// Config 描述如何构造一个 Provider，便于在 CLI / 服务端之间复用。
type Config struct {
	Kind    string // "openai"（默认）或 "claude"
	APIKey  string
	BaseURL string
	Model   string
}

// ConfigFromEnv 读取 CLAW_* 环境变量；未设置时回退到课程默认的智谱 GLM。
//
//	CLAW_PROVIDER  openai | claude
//	CLAW_API_KEY   不设置时回退 ZHIPU_API_KEY
//	CLAW_BASE_URL  不设置时按 Kind 选智谱对应端点
//	CLAW_MODEL     默认 glm-4.5-air
func ConfigFromEnv() Config {
	c := Config{
		Kind:    os.Getenv("CLAW_PROVIDER"),
		APIKey:  os.Getenv("CLAW_API_KEY"),
		BaseURL: os.Getenv("CLAW_BASE_URL"),
		Model:   os.Getenv("CLAW_MODEL"),
	}
	if c.Kind == "" {
		c.Kind = "openai"
	}
	if c.APIKey == "" {
		c.APIKey = os.Getenv("ZHIPU_API_KEY")
	}
	if c.BaseURL == "" {
		if c.Kind == "claude" {
			c.BaseURL = ZhipuAnthropicBaseURL
		} else {
			c.BaseURL = ZhipuBaseURL
		}
	}
	if c.Model == "" {
		c.Model = "glm-4.5-air"
	}
	return c
}

// New 根据配置构造 Provider。
func New(c Config) (LLMProvider, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("缺少 API Key：请设置 CLAW_API_KEY 或 ZHIPU_API_KEY")
	}
	switch c.Kind {
	case "openai":
		return NewOpenAIProvider(c.APIKey, c.BaseURL, c.Model), nil
	case "claude":
		return NewClaudeProvider(c.APIKey, c.BaseURL, c.Model), nil
	default:
		return nil, fmt.Errorf("未知的 CLAW_PROVIDER: %q（支持 openai / claude）", c.Kind)
	}
}
