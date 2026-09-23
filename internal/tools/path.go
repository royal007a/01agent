package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspace 以 os.Root 打开工作区，并把模型给出的路径换算成相对工作区的路径。
//
// 课程第 05 讲留了 TODO："生产环境中需要做路径穿越检测"。字符串层面的检查挡不住符号链接，
// "先 EvalSymlinks 校验、再按字符串路径打开"又有 TOCTOU 竞态（校验后把目录换成指向外部的链接）。
// os.Root 的每次操作都在内核层面相对工作区目录句柄解析路径，任何经 ".." 或符号链接逃逸都会失败，
// 检查与打开是同一个原子操作。调用方用完需 Close。
// （bash 工具按 YOLO 哲学不受此限制，文件工具则坚守工作区边界。）
func workspace(workDir, p string) (*os.Root, string, error) {
	if strings.TrimSpace(p) == "" {
		return nil, "", fmt.Errorf("path 参数不能为空")
	}
	base, err := filepath.Abs(workDir)
	if err != nil {
		return nil, "", err
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(base, p)
	}
	rel, err := filepath.Rel(base, filepath.Clean(full))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// 绝对路径也可能经 /var → /private/var 这类链接落在工作区内，再用真实路径比一次
		if rb, e1 := filepath.EvalSymlinks(base); e1 == nil {
			if rf, e2 := filepath.EvalSymlinks(filepath.Dir(full)); e2 == nil {
				if r2, e3 := filepath.Rel(rb, filepath.Join(rf, filepath.Base(full))); e3 == nil && r2 != ".." && !strings.HasPrefix(r2, ".."+string(filepath.Separator)) {
					rel, err = r2, nil
				}
			}
		}
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, "", fmt.Errorf("路径 %q 越出了工作区边界 %s，已被拒绝", p, base)
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, "", fmt.Errorf("打开工作区失败: %w", err)
	}
	return root, rel, nil
}

// wrapRootErr 把 os.Root 的逃逸错误翻译成 RecoveryManager 能识别的固定文案。
func wrapRootErr(p string, err error) error {
	if err != nil && strings.Contains(err.Error(), "path escapes from parent") {
		return fmt.Errorf("路径 %q 经符号链接解析后越出了工作区边界，已被拒绝", p)
	}
	return err
}
