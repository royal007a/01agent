package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveInWorkDir 把模型给出的路径解析为工作区内的绝对路径。
//
// 课程第 05 讲留了一个 TODO："生产环境中需要做路径穿越检测防范，防止 ../../etc/passwd"。
// 这里补齐：相对路径以 workDir 为基准；绝对路径也允许，但都必须落在 workDir 之内。
// （bash 工具按 YOLO 哲学不受此限制，文件工具则坚守工作区边界。）
func resolveInWorkDir(workDir, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path 参数不能为空")
	}
	base, err := filepath.Abs(workDir)
	if err != nil {
		return "", err
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(base, p)
	}
	full = filepath.Clean(full)
	rel, err := filepath.Rel(base, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径 %q 越出了工作区边界 %s，已被拒绝", p, base)
	}
	return full, nil
}
