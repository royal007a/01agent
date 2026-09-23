package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// EditFileTool 局部字符串替换，带四级模糊匹配降级链（第 07 讲）。
type EditFileTool struct{ workDir string }

func NewEditFileTool(workDir string) *EditFileTool { return &EditFileTool{workDir: workDir} }

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "对现有文件进行局部的字符串替换。这比重写整个文件更安全、更快速。请提供足够的 old_text 上下文以确保匹配的唯一性。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":     map[string]interface{}{"type": "string", "description": "要修改的文件路径"},
				"old_text": map[string]interface{}{"type": "string", "description": "文件中原有的文本。必须包含足够的上下文（建议上下各多包含几行），以确保在文件中的唯一性。"},
				"new_text": map[string]interface{}{"type": "string", "description": "要替换成的新文本"},
			},
			"required": []string{"path", "old_text", "new_text"},
		},
	}
}

type editFileArgs struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

// 固定的错误文案：第 14 讲的 RecoveryManager 依赖这些特征做分诊。
const (
	errEditNotFound  = "在文件中未找到 old_text，请检查内容和缩进"
	errEditAmbiguous = "匹配到了多处"
)

// fuzzyReplace 四级容错降级替换：L1 精确 → L2 换行归一 → L3 首尾 Trim → L4 逐行去缩进。
// 每一级都坚守"唯一性"底线：命中多处绝不替换。
func fuzzyReplace(originalContent, oldText, newText string) (string, error) {
	if oldText == "" {
		return "", fmt.Errorf("old_text 不能为空")
	}
	// L1: 精确匹配
	switch count := strings.Count(originalContent, oldText); {
	case count == 1:
		return strings.Replace(originalContent, oldText, newText, 1), nil
	case count > 1:
		return "", fmt.Errorf("old_text %s %d 处，请提供更多的上下文代码以确保唯一性", errEditAmbiguous, count)
	}
	// L2: 换行符归一化 (\r\n -> \n)
	normalizedContent := strings.ReplaceAll(originalContent, "\r\n", "\n")
	normalizedOld := strings.ReplaceAll(oldText, "\r\n", "\n")
	normalizedNew := strings.ReplaceAll(newText, "\r\n", "\n")
	switch count := strings.Count(normalizedContent, normalizedOld); {
	case count == 1:
		return strings.Replace(normalizedContent, normalizedOld, normalizedNew, 1), nil
	case count > 1:
		return "", fmt.Errorf("old_text %s %d 处，请提供更多的上下文代码以确保唯一性", errEditAmbiguous, count)
	}
	// L3: 忽略首尾空行与空格
	if trimmedOld := strings.TrimSpace(normalizedOld); trimmedOld != "" {
		switch count := strings.Count(normalizedContent, trimmedOld); {
		case count == 1:
			return strings.Replace(normalizedContent, trimmedOld, strings.TrimSpace(normalizedNew), 1), nil
		case count > 1:
			return "", fmt.Errorf("old_text %s %d 处，请提供更多的上下文代码以确保唯一性", errEditAmbiguous, count)
		}
	}
	// L4: 逐行去缩进匹配（消除模型遗漏缩进的幻觉）
	return lineByLineReplace(normalizedContent, normalizedOld, normalizedNew)
}

// lineByLineReplace 按行去首尾空白后做滑动窗口匹配，并把 newText 重新对齐到原始缩进。
func lineByLineReplace(content, oldText, newText string) (string, error) {
	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(strings.Trim(oldText, "\n"), "\n")
	for i := range oldLines {
		oldLines[i] = strings.TrimSpace(oldLines[i])
	}
	if len(oldLines) == 0 || len(contentLines) < len(oldLines) {
		return "", fmt.Errorf("%s", errEditNotFound)
	}
	matchCount, matchStart := 0, -1
	for i := 0; i <= len(contentLines)-len(oldLines); i++ {
		ok := true
		for j := range oldLines {
			if strings.TrimSpace(contentLines[i+j]) != oldLines[j] {
				ok = false
				break
			}
		}
		if ok {
			matchCount++
			matchStart = i
		}
	}
	if matchCount == 0 {
		return "", fmt.Errorf("%s；请先调用 read_file 仔细确认文件内容", errEditNotFound)
	}
	if matchCount > 1 {
		return "", fmt.Errorf("模糊%s %d 处相似代码，请提供更多上下行代码以精确定位", errEditAmbiguous, matchCount)
	}
	// 第 07 讲思考题：提取目标行的基础缩进，把 newText 重新对齐，避免替换后格式错乱。
	baseIndent := leadingWhitespace(contentLines[matchStart])
	replaced := reindent(strings.Trim(newText, "\n"), baseIndent)

	out := make([]string, 0, len(contentLines))
	out = append(out, contentLines[:matchStart]...)
	out = append(out, replaced)
	out = append(out, contentLines[matchStart+len(oldLines):]...)
	return strings.Join(out, "\n"), nil
}

func leadingWhitespace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// reindent 如果 newText 的首个非空行没有带上 baseIndent，就去掉公共缩进后整体补上 baseIndent。
func reindent(newText, baseIndent string) string {
	lines := strings.Split(newText, "\n")
	first := ""
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			first = l
			break
		}
	}
	if baseIndent == "" || strings.HasPrefix(first, baseIndent) {
		return newText
	}
	common := leadingWhitespace(first)
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		ws := leadingWhitespace(l)
		for !strings.HasPrefix(ws, common) {
			common = common[:len(common)-1]
		}
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = baseIndent + strings.TrimPrefix(l, common)
	}
	return strings.Join(lines, "\n")
}

func (t *EditFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input editFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	fullPath, err := resolveInWorkDir(t.workDir, input.Path)
	if err != nil {
		return "", err
	}
	contentBytes, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("读取文件失败，请确认路径是否正确: %w", err)
	}
	newContent, err := fuzzyReplace(string(contentBytes), input.OldText, input.NewText)
	if err != nil {
		return "", err
	}
	info, _ := os.Stat(fullPath)
	if err := os.WriteFile(fullPath, []byte(newContent), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("写回文件失败: %w", err)
	}
	return fmt.Sprintf("✅ 成功修改文件: %s", input.Path), nil
}
