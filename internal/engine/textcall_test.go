package engine

import (
	"testing"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

func TestRecoverTextToolCalls(t *testing.T) {
	defs := []schema.ToolDefinition{{Name: "read_file"}, {Name: "bash"}}
	cases := map[string]int{
		"我将调用 `read_file`。\n```json\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"config.json\"}}\n```": 1,
		"<tool_call>\n{\"name\": \"bash\", \"arguments\": {\"command\": \"ls\"}}\n</tool_call>":                  1,
		`{"name":"bash","arguments":"{\"command\":\"ls\"}"}`:                                                     1, // arguments 是字符串
		"```json\n{\"name\": \"rm_everything\", \"arguments\": {}}\n```":                                         0, // 不是可用工具
		"配置示例：\n```json\n{\"name\": \"tiny-claw\", \"version\": \"v1\"}\n```":                                    0, // 普通 JSON
		"修复完成。": 0,
	}
	for in, want := range cases {
		if got := recoverTextToolCalls(in, defs); len(got) != want {
			t.Errorf("%q => %d calls, want %d", in, len(got), want)
		}
	}
	if len(recoverTextToolCalls("```json\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n```", nil)) != 0 {
		t.Fatal("慢思考阶段（无工具）不应还原")
	}
}
