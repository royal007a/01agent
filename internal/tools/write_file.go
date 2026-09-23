package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// WriteFileTool 创建或覆盖写入一个文件（第 06 讲）。
type WriteFileTool struct{ workDir string }

func NewWriteFileTool(workDir string) *WriteFileTool { return &WriteFileTool{workDir: workDir} }

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "创建或覆盖写入一个文件。如果目录不存在会自动创建。请提供相对于工作区的相对路径。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string", "description": "要写入的文件路径，如 src/main.go"},
				"content": map[string]interface{}{"type": "string", "description": "要写入的完整文件内容"},
			},
			"required": []string{"path", "content"},
		},
	}
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input writeFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	root, rel, err := workspace(t.workDir, input.Path)
	if err != nil {
		return "", err
	}
	defer root.Close()
	// Root.MkdirAll 不跟随已存在的目录链接（即便指向工作区内），所以目录已存在时直接跳过
	if dir := filepath.Dir(rel); dir != "." {
		if fi, err := root.Stat(dir); err != nil || !fi.IsDir() {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return "", fmt.Errorf("创建父目录失败: %w", wrapRootErr(input.Path, err))
			}
		}
	}
	if err := atomicWrite(root, rel, []byte(input.Content), 0o644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", wrapRootErr(input.Path, err))
	}
	return fmt.Sprintf("成功将 %d 字节写入到文件: %s", len(input.Content), input.Path), nil
}

// atomicWrite 先写同目录临时文件再 Rename：读者要么看到旧内容、要么看到新内容，不会读到半截文件。
func atomicWrite(root *os.Root, rel string, data []byte, perm os.FileMode) error {
	tmp := fmt.Sprintf("%s.claw-tmp-%d", rel, time.Now().UnixNano())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		root.Remove(tmp)
		return err
	}
	return nil
}
