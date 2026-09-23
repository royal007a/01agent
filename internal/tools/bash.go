package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// BashMaxBytes 终端输出截断上限；保留头尾各一半（报错原因常在开头，总结常在结尾）。
const BashMaxBytes = 8000

// BashTool 在工作区执行任意 bash 命令：YOLO 哲学的核心（第 06 讲）。
type BashTool struct {
	workDir string
	Timeout time.Duration
}

func NewBashTool(workDir string) *BashTool {
	return &BashTool{workDir: workDir, Timeout: 30 * time.Second}
}

func (t *BashTool) Name() string { return "bash" }

func (t *BashTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "在当前工作区执行任意的 bash 命令。支持链式命令(如 &&)。返回标准输出(stdout)和标准错误(stderr)。常驻服务请用 nohup cmd > log 2>&1 & 转入后台。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "要执行的 bash 命令，例如: ls -la 或 go test ./..."},
			},
			"required": []string{"command"},
		},
	}
}

type bashArgs struct {
	Command string `json:"command"`
}

func (t *BashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input bashArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	// 驾驭底线 1：Time Budgeting
	timeoutCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "bash", "-c", input.Command)
	// 驾驭底线 2：绑定工作区目录
	cmd.Dir = t.workDir
	// 课程第 13 讲结尾的坑：`./server &` 会让 go-tiny-claw 整个卡死。
	// 原因是后台子进程继承了 stdout/stderr 管道，CombinedOutput 要等管道关闭才返回。
	// 修复：独立进程组 + 超时杀整组 + WaitDelay 在 bash 退出后强制关闭管道。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	out, err := cmd.CombinedOutput()
	outputStr := string(out)

	if errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("命令执行超时(%s)，已被系统强制终止 (DeadlineExceeded)。已有输出:\n%s", t.Timeout, clampOutput(outputStr))
	}
	var note string
	if errors.Is(err, exec.ErrWaitDelay) {
		// bash 本身已正常退出，只是后台进程仍占着输出管道
		err = nil
		note = "\n[提示: 检测到后台进程仍占用输出管道，已自动分离。后台服务请重定向输出: nohup cmd > app.log 2>&1 &]"
	}
	// 驾驭底线 3：错误原样回传，交给模型自纠错。
	// 课程版本在这里返回 (字符串, nil)，导致 ToolResult.IsError 永远是 false，
	// 后面第 14 讲的 Error Recovery、第 15 讲的死循环探测对 bash 完全失效。
	// Registry 会把 error 包装成 IsError=true 的 Observation 回灌给模型，同样不会阻断 Main Loop。
	if err != nil {
		return "", fmt.Errorf("执行报错: %v\n输出:\n%s", err, clampOutput(outputStr))
	}
	if outputStr == "" {
		return "命令执行成功，无终端输出。" + note, nil
	}
	// 驾驭底线 4：长度截断保护
	return clampOutput(outputStr) + note, nil
}

func clampOutput(s string) string {
	if len(s) <= BashMaxBytes {
		return s
	}
	half := BashMaxBytes / 2
	return fmt.Sprintf("%s\n\n...[终端输出过长（共 %d 字节），中间部分已截断]...\n\n%s", textutil.Head(s, half), len(s), textutil.Tail(s, half))
}
