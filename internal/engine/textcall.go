package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// 小模型（实测 Ollama 上的 qwen2.5:7b）经常把工具调用"写"进正文，而不是走结构化的 tool_calls：
//
//	我将调用 read_file 工具来读取文件。
//	```json
//	{"name": "read_file", "arguments": {"path": "config.json"}}
//	```
//
// 这和第 07 讲"把容错做在底层、吸收模型误差"是一个思路：Harness 识别这种写法，把它还原成真正的 ToolCall。
// 为了不误伤，只接受 name 属于当前可用工具、arguments 是 JSON 对象的写法。
var (
	fencedJSON = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")
	taggedJSON = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
)

func recoverTextToolCalls(content string, defs []schema.ToolDefinition) []schema.ToolCall {
	if len(defs) == 0 || !strings.Contains(content, "{") {
		return nil
	}
	valid := map[string]bool{}
	for _, d := range defs {
		valid[d.Name] = true
	}
	var candidates []string
	for _, re := range []*regexp.Regexp{taggedJSON, fencedJSON} {
		for _, m := range re.FindAllStringSubmatch(content, -1) {
			candidates = append(candidates, m[1])
		}
	}
	if len(candidates) == 0 {
		if t := strings.TrimSpace(content); strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
			candidates = append(candidates, t)
		}
	}
	var calls []schema.ToolCall
	for i, c := range candidates {
		var raw struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Params    json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal([]byte(c), &raw) != nil || !valid[raw.Name] {
			continue
		}
		args := raw.Arguments
		if len(args) == 0 {
			args = raw.Params
		}
		// arguments 可能是对象，也可能被模型写成 JSON 字符串
		var s string
		if json.Unmarshal(args, &s) == nil {
			args = json.RawMessage(s)
		}
		var obj map[string]any
		if json.Unmarshal(args, &obj) != nil {
			continue
		}
		calls = append(calls, schema.ToolCall{ID: fmt.Sprintf("textcall_%d", i), Name: raw.Name, Arguments: args})
	}
	return calls
}
