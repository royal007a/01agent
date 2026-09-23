package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// ReadFileMaxBytes 单次读取的物理截断上限（第 05 讲底线防御）。
const ReadFileMaxBytes = 8000

// ReadFileTool 读取本地文件内容，限制在工作区内。
type ReadFileTool struct{ workDir string }

func NewReadFileTool(workDir string) *ReadFileTool { return &ReadFileTool{workDir: workDir} }

func (t *ReadFileTool) Name() string   { return "read_file" }
func (t *ReadFileTool) ReadOnly() bool { return true }

func (t *ReadFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "读取指定路径的文件内容。请提供相对工作区的路径。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "要读取的文件路径，如 cmd/claw/main.go"},
			},
			"required": []string{"path"},
		},
	}
}

type readFileArgs struct {
	Path string `json:"path"`
}

func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input readFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	root, rel, err := workspace(t.workDir, input.Path)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Stat(rel)
	if err != nil {
		return "", fmt.Errorf("打开文件失败: %w", wrapRootErr(input.Path, err))
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s 是一个目录，不是文件；请用 bash 的 ls 查看目录内容", input.Path)
	}
	content, err := root.ReadFile(rel)
	if err != nil {
		return "", fmt.Errorf("读取文件内容失败: %w", wrapRootErr(input.Path, err))
	}
	if len(content) > ReadFileMaxBytes {
		return fmt.Sprintf("%s\n\n...[由于内容过长（共 %d 字节），已被系统截断至前 %d 字节。如需后续内容，请用 bash 的 sed -n '起,止p' 或 tail 分段读取]...",
			textutil.Head(string(content), ReadFileMaxBytes), len(content), ReadFileMaxBytes), nil
	}
	return string(content), nil
}
