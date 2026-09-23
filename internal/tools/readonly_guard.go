package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// safeRedirect 只读命令里常见的无害重定向。
var safeRedirect = regexp.MustCompile(`[0-9]?>{1,2}\s*/dev/null|[0-9]?>&[0-9]`)

// readOnlyCommands 只读子智能体允许的命令白名单（每个管道/分号段的首个词都必须在这里）。
// 黑名单永远列不全（python heredoc、lua、perl - …），所以默认拒绝，只放行确定无副作用的命令。
var readOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true, "rg": true,
	"find": true, "wc": true, "sort": true, "uniq": true, "cut": true, "tr": true, "file": true, "stat": true,
	"du": true, "df": true, "pwd": true, "echo": true, "printf": true, "tree": true, "diff": true, "cmp": true,
	"which": true, "basename": true, "dirname": true, "realpath": true, "readlink": true, "date": true,
	"test": true, "[": true, "true": true, "false": true, "nl": true, "column": true, "od": true, "hexdump": true,
	"strings": true, "md5": true, "md5sum": true, "shasum": true, "sha256sum": true, "jq": true, "cd": true,
	"git": true, "go": true, "uname": true, "whoami": true, "id": true,
}

var (
	gitReadOnly = map[string]bool{"log": true, "show": true, "diff": true, "status": true, "grep": true, "ls-files": true,
		"blame": true, "rev-parse": true, "branch": true, "describe": true, "shortlog": true, "cat-file": true, "ls-tree": true}
	// go list 可能触发模块下载 / -toolexec，故不放行；go env 另禁 -w/-u（会改用户配置）
	goReadOnly = map[string]bool{"version": true, "env": true, "doc": true}
	// argSideEffects 白名单命令里"带参数就能写文件或执行程序"的选项（第二轮 codex review）。
	argSideEffects = map[string]*regexp.Regexp{
		"sort": regexp.MustCompile(`\s(-[a-zA-Z]*o|--output(=|\s|$)|--compress-program)`),
		"git":  regexp.MustCompile(`\s(--output(=|\s|$)|-O\S*|--open-files-in-pager|--ext-diff|-c\s|--config-env|--exec-path)`),
		"go":   regexp.MustCompile(`\s-(w|u)\b|\s-(toolexec|exec)\b`),
		"tree": regexp.MustCompile(`\s-o\b`),
		"file": regexp.MustCompile(`\s(-[a-zA-Z]*C|--compile)`),
		"date": regexp.MustCompile(`\s(-s|--set)\b`),
		"rg":   regexp.MustCompile(`\s--pre(=|\s|$)`),
	}
	findSideEffect = regexp.MustCompile(`\s-(delete|exec|execdir|ok|okdir|fprint|fprint0|fprintf|fls)\b`)
	// 命令替换、进程替换、heredoc/herestring 都能在"只读命令"里夹带任意执行
	forbiddenSyntax = regexp.MustCompile("`|\\$\\(|<\\(|>\\(|<<")
	segmentSplit    = regexp.MustCompile(`&&|\|\||[;|&\n]`)
)

// checkReadOnly 返回空串表示放行，否则返回拒绝原因。
func checkReadOnly(command string) string {
	cmd := safeRedirect.ReplaceAllString(command, "")
	if forbiddenSyntax.MatchString(cmd) {
		return "禁止使用命令替换、进程替换或 heredoc（它们可以夹带任意执行）"
	}
	if strings.Contains(cmd, ">") {
		return "禁止输出重定向写文件"
	}
	for _, seg := range segmentSplit.Split(cmd, -1) {
		fields := strings.Fields(strings.TrimLeft(strings.TrimSpace(seg), "(!{"))
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimRight(fields[0], ")}")
		if strings.Contains(name, "=") || strings.ContainsAny(name, `"'\\$/`) {
			return "禁止变量赋值、带路径或引号的命令名: " + name
		}
		if !readOnlyCommands[name] {
			return "命令 " + name + " 不在只读白名单中"
		}
		if re, ok := argSideEffects[name]; ok && re.MatchString(" "+strings.Join(fields[1:], " ")) {
			return name + " 的该选项会写文件、改配置或执行外部程序"
		}
		switch name {
		case "git":
			if len(fields) < 2 || !gitReadOnly[fields[1]] {
				return "只允许只读的 git 子命令（log/show/diff/status/grep…）"
			}
			if fields[1] == "branch" && len(positional(fields[2:])) > 0 {
				return "git branch 只允许列出分支，不允许创建/删除"
			}
		case "uniq":
			if len(positional(fields[1:])) > 1 {
				return "uniq 的第二个文件参数会被当作输出文件"
			}
		case "go":
			if len(fields) < 2 || !goReadOnly[fields[1]] {
				return "只允许 go version/env/list/doc"
			}
		case "find":
			if findSideEffect.MatchString(seg) {
				return "find 不允许 -delete/-exec/-fprint 等副作用动作"
			}
		}
	}
	return ""
}

// positional 返回不以 - 开头的参数。
func positional(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// ReadOnlyGuard 给子智能体的注册表用：bash 只允许白名单内的只读命令，其余一律拒绝。
// 课程只是"不给 edit_file"，但 bash 本身就能写文件；这里补上这道闸门，真正限制爆炸半径。
func ReadOnlyGuard() MiddlewareFunc {
	return func(ctx context.Context, call schema.ToolCall) (bool, string) {
		if call.Name != "bash" {
			return true, ""
		}
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return false, "参数解析失败"
		}
		if why := checkReadOnly(a.Command); why != "" {
			return false, "你是只读的探索子智能体：" + why + "。请只用 ls/cat/head/tail/grep/rg/find/wc/git log|show|diff 等只读命令。"
		}
		return true, ""
	}
}
