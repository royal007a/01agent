package tools

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// mutatingShell 粗粒度识别会修改文件系统/仓库状态的 shell 片段。
var mutatingShell = regexp.MustCompile(`(^|[^<>2&])>{1,2}\s*[^&\s]|\b(rm|mv|cp|mkdir|rmdir|touch|chmod|chown|ln|tee|truncate|dd|kill|pkill|nohup)\b|\bsed\s+(-[a-zA-Z]*\s+)*-i|\bgit\s+(commit|push|reset|checkout|clean|merge|rebase|add|stash|tag)\b|\b(go|npm|pip|yarn|pnpm)\s+(install|get|mod\s+tidy)\b`)

// ReadOnlyGuard 给子智能体的注册表用：bash 只允许只读命令（find/grep/cat/ls…），其余一律拒绝。
// 课程只是"不给 edit_file"，但 bash 本身就能写文件；这里补上这道闸门，真正限制爆炸半径。
func ReadOnlyGuard() MiddlewareFunc {
	return func(ctx context.Context, call schema.ToolCall) (bool, string) {
		if call.Name != "bash" {
			return true, ""
		}
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(call.Arguments, &a)
		if mutatingShell.MatchString(a.Command) {
			return false, "你是只读的探索子智能体，禁止执行任何会修改文件或系统状态的命令（重定向写文件、rm/mv/sed -i/git commit 等）。请只用 find/grep/cat/ls/head/tail 等只读命令。"
		}
		return true, ""
	}
}
