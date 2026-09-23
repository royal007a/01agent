package tools

import (
	"fmt"
	"os"
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
	if !within(base, full) {
		return "", fmt.Errorf("路径 %q 越出了工作区边界 %s，已被拒绝", p, base)
	}
	// 文本层面在工作区内还不够：工作区里的符号链接可能指向外部。
	// 解析"最深的已存在祖先"的真实路径，再拼回尚不存在的部分做二次校验。
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		realBase = base
	}
	existing, rest := full, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("解析路径 %q 失败: %w", p, err)
	}
	if !within(realBase, filepath.Join(realExisting, rest)) {
		return "", fmt.Errorf("路径 %q 经符号链接解析后越出了工作区边界 %s，已被拒绝", p, base)
	}
	return full, nil
}

func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
