package tools

import (
	"context"
	"encoding/json"
	"fmt"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// ReadSkillTool 按需加载技能正文（第 10 讲思考题：System Prompt 只放技能元数据）。
type ReadSkillTool struct{ Loader *ctxpkg.SkillLoader }

func NewReadSkillTool(workDir string) *ReadSkillTool {
	return &ReadSkillTool{Loader: ctxpkg.NewSkillLoader(workDir)}
}

func (t *ReadSkillTool) Name() string   { return "read_skill" }
func (t *ReadSkillTool) ReadOnly() bool { return true }
func (t *ReadSkillTool) Meta() ToolMeta { return ToolMeta{SideEffect: "none", MaxOutputBytes: 32000} }

func (t *ReadSkillTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "读取某个 Agent Skill 的完整执行指南。当任务符合系统提示中某个技能的触发条件、且其正文未预先加载时使用；name 必须是系统提示里列出的技能名称。不要用它读取普通文件（用 read_file）。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "技能名称（与系统提示中列出的完全一致）"},
			},
			"required": []string{"name"},
		},
	}
}

func (t *ReadSkillTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	body, ok := t.Loader.Body(in.Name)
	if !ok {
		return "", fmt.Errorf("技能 %q 不存在；请使用系统提示中列出的技能名称", in.Name)
	}
	return body, nil
}
