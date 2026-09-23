package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// mutatingShell 粗粒度识别会修改文件系统/仓库状态的 shell 片段。
// 这是尽力而为的黑名单，只用来缩小爆炸半径；子智能体注册表同样挂了 permission.Middleware 兜底。
var mutatingShell = regexp.MustCompile(strings.Join([]string{
	`&>|>{1,2}\s*[^&\s]`, // 任何输出重定向（下面单独放行 >/dev/null 与 >&N）
	`\b(rm|mv|cp|mkdir|rmdir|touch|chmod|chown|ln|tee|truncate|dd|kill|pkill|nohup|install|patch|curl|wget|kubectl|docker|ssh|scp|rsync)\b`,
	`\bsed\b[^|;&]*\s(-[a-zA-Z]*i|--in-place)`, `\bperl\b[^|;&]*\s-[a-zA-Z]*i`,
	`\b(python[0-9.]*|node|ruby|perl|php)\s+(-c|-e)\b`, `\|\s*(ba|z)?sh\b|\b(ba|z)?sh\s+-c\b|\beval\b|\bbase64\b`,
	`\bfind\b.*\s-(delete|exec|execdir|ok|fprint)\b`, `\bxargs\b`,
	`\bgit\s+(commit|push|reset|checkout|clean|merge|rebase|add|stash|tag|apply|am|rm|mv|restore|switch|pull|fetch|config)\b`,
	`\b(go|npm|pip[0-9]*|yarn|pnpm|cargo|make)\s`,
}, "|"))

// safeRedirect 只读命令里常见的无害重定向。
var safeRedirect = regexp.MustCompile(`[0-9]?>{1,2}\s*/dev/null|[0-9]?>&[0-9]`)

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
		if mutatingShell.MatchString(safeRedirect.ReplaceAllString(a.Command, "")) {
			return false, "你是只读的探索子智能体，禁止执行任何会修改文件或系统状态的命令（重定向写文件、rm/mv/sed -i/git commit 等）。请只用 find/grep/cat/ls/head/tail 等只读命令。"
		}
		return true, ""
	}
}
