// Package envlock 实现"环境锁定"：把 Agent 运行所依赖、又不在它控制范围内的外部条件
// （运行时、依赖、Skill、模型）拍成快照写进 lockfile，之后每次启动与之比对，暴露环境漂移。
//
// 锁定可控项：运行时/工具版本、依赖版本与校验和、Skill 内容哈希、模型请求名与服务端回报名。
// 隔离不可控项：外部 API 由 Provider 抽象吸收，模型静默滚动由 ModelGuard 在每次响应上检测。
package envlock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// LockFile 锁文件在工作区内的相对路径。
const LockFile = ".claw/env.lock.json"

// SchemaVersion 锁文件格式版本。
const SchemaVersion = 1

// Mode 锁定模式：warn 只报告漂移，strict 遇到错误级漂移拒绝启动/拒绝响应。
type Mode string

const (
	ModeWarn   Mode = "warn"
	ModeStrict Mode = "strict"
)

// DefaultTools Agent 的 bash/检索/评测实际会调用的外部命令。
var DefaultTools = []string{"bash", "git", "rg", "go", "grep", "sed"}

// ErrNoLock 工作区尚未锁定。
var ErrNoLock = errors.New("envlock: 工作区没有 " + LockFile)

// Tool 外部命令的位置与版本；Path 为空表示锁定时不存在。
type Tool struct {
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// Runtime 执行底座：操作系统、架构、Harness 的 Go 运行时、Agent 调用的外部命令。
type Runtime struct {
	OS    string          `json:"os"`
	Arch  string          `json:"arch"`
	Go    string          `json:"go"`
	Tools map[string]Tool `json:"tools"`
}

// Model 行为基础。Requested 是配置里写的名字，Observed 是服务端在响应里回报的实际版本。
type Model struct {
	Provider  string `json:"provider,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	Requested string `json:"requested,omitempty"`
	Observed  string `json:"observed,omitempty"`
}

// Lock 环境快照。Deps 的 value 是 "version sum"；Skills 的 value 是文件 sha256。
type Lock struct {
	Version   int               `json:"version"`
	Mode      Mode              `json:"mode"`
	CreatedAt time.Time         `json:"created_at"`
	Runtime   Runtime           `json:"runtime"`
	Deps      map[string]string `json:"deps"`
	Skills    map[string]string `json:"skills"`
	Model     Model             `json:"model"`
}

// CaptureOptions 拍快照的输入。
type CaptureOptions struct {
	WorkDir string
	Model   Model
	Tools   []string // 为空时用 DefaultTools
}

// Capture 拍下当前环境。
func Capture(o CaptureOptions) (*Lock, error) {
	skills, err := hashSkills(o.WorkDir)
	if err != nil {
		return nil, err
	}
	tools := o.Tools
	if len(tools) == 0 {
		tools = DefaultTools
	}
	l := &Lock{
		Version:   SchemaVersion,
		Mode:      ModeWarn,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		Runtime:   Runtime{OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(), Tools: map[string]Tool{}},
		Deps:      buildDeps(),
		Skills:    skills,
		Model:     o.Model,
	}
	for _, name := range tools {
		l.Runtime.Tools[name] = probeTool(name)
	}
	return l, nil
}

func probeTool(name string) Tool {
	path, err := exec.LookPath(name)
	if err != nil {
		return Tool{}
	}
	args := []string{"--version"}
	if name == "go" {
		args = []string{"version"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local") // 报告本机实际安装的 go，而不是被自动切换后的版本
	out, err := cmd.Output()
	ver := ""
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ver = line
			break
		}
	}
	if err != nil && ver == "" {
		ver = "unknown" // 如 BSD sed 不支持 --version：记录存在，但版本不可得
	}
	return Tool{Path: path, Version: ver}
}

// buildDeps 读取二进制内嵌的依赖清单（来自 go.mod/go.sum，编译期即确定）。
func buildDeps() map[string]string {
	deps := map[string]string{}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return deps
	}
	for _, d := range bi.Deps {
		m := d
		if d.Replace != nil {
			m = d.Replace
		}
		deps[d.Path] = strings.TrimSpace(m.Version + " " + m.Sum)
	}
	return deps
}

// hashSkills 对 .claw/skills 下每个文件做内容哈希。符号链接不跟随，记为 "symlink:<目标>"：
// 指向工作区外的 Skill 等于偷偷引入了全局 Skill，Check 会把它判为错误。
func hashSkills(workDir string) (map[string]string, error) {
	out := map[string]string{}
	base := filepath.Join(workDir, ".claw", "skills")
	if _, err := os.Lstat(base); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(workDir, path)
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(path)
			out[rel] = "symlink:" + target
			return nil
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("envlock: 计算 Skill 哈希失败: %w", err)
	}
	return out, nil
}

// Load 读取工作区锁文件。不存在返回 ErrNoLock；损坏则报错（绝不当作"未锁定"放行）。
func Load(workDir string) (*Lock, error) {
	b, err := os.ReadFile(filepath.Join(workDir, LockFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoLock
	}
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("envlock: %s 损坏: %w", LockFile, err)
	}
	if l.Version != SchemaVersion {
		return nil, fmt.Errorf("envlock: %s 版本 %d 不受支持（期望 %d）", LockFile, l.Version, SchemaVersion)
	}
	if l.Mode != ModeWarn && l.Mode != ModeStrict {
		return nil, fmt.Errorf("envlock: %s 的 mode %q 非法", LockFile, l.Mode)
	}
	return &l, nil
}

// Save 原子写入锁文件（排序后的 JSON，便于纳入版本控制与 diff）。
func Save(workDir string, l *Lock) error {
	path := filepath.Join(workDir, LockFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Severity 漂移严重级。
type Severity string

const (
	SevError Severity = "error"
	SevWarn  Severity = "warn"
)

// Drift 一条环境漂移。
type Drift struct {
	Category string   `json:"category"` // runtime/tool/dep/skill/model
	Key      string   `json:"key"`
	Locked   string   `json:"locked"`
	Current  string   `json:"current"`
	Severity Severity `json:"severity"`
}

func (d Drift) String() string {
	return fmt.Sprintf("[%s] %s %s: %q → %q", d.Severity, d.Category, d.Key, d.Locked, d.Current)
}

// Diff 比较锁定环境与当前环境，按类别和键排序输出，保证报告稳定。
func Diff(locked, cur *Lock) []Drift {
	var ds []Drift
	add := func(cat, key, a, b string, sev Severity) {
		ds = append(ds, Drift{Category: cat, Key: key, Locked: a, Current: b, Severity: sev})
	}
	if locked.Runtime.OS != cur.Runtime.OS {
		add("runtime", "os", locked.Runtime.OS, cur.Runtime.OS, SevError)
	}
	if locked.Runtime.Arch != cur.Runtime.Arch {
		add("runtime", "arch", locked.Runtime.Arch, cur.Runtime.Arch, SevError)
	}
	if locked.Runtime.Go != cur.Runtime.Go {
		add("runtime", "go", locked.Runtime.Go, cur.Runtime.Go, SevWarn)
	}
	// 只比较锁里声明过的工具：声明即契约，未声明的不管
	for _, name := range sortedKeys(locked.Runtime.Tools) {
		a, b := locked.Runtime.Tools[name], cur.Runtime.Tools[name]
		switch {
		case a.Path == "" && b.Path != "":
			add("tool", name, "(absent)", b.Version, SevWarn)
		case a.Path != "" && b.Path == "":
			add("tool", name, a.Version, "(absent)", SevError)
		case a.Version != b.Version:
			add("tool", name, a.Version, b.Version, SevError)
		case a.Path != b.Path:
			add("tool", name+".path", a.Path, b.Path, SevWarn)
		}
	}
	diffMap(locked.Deps, cur.Deps, func(k, a, b string) { add("dep", k, a, b, SevError) })
	diffMap(locked.Skills, cur.Skills, func(k, a, b string) { add("skill", k, a, b, SevError) })
	for _, k := range sortedKeys(cur.Skills) {
		if v := cur.Skills[k]; strings.HasPrefix(v, "symlink:") && locked.Skills[k] == v {
			add("skill", k, v, "Skill 必须本地化到工作区，禁止链接到外部", SevError)
		}
	}
	lm, cm := locked.Model, cur.Model
	for _, f := range [][3]string{{"provider", lm.Provider, cm.Provider}, {"base_url", lm.BaseURL, cm.BaseURL}, {"requested", lm.Requested, cm.Requested}} {
		if f[1] != f[2] {
			add("model", f[0], f[1], f[2], SevError)
		}
	}
	if lm.Observed != "" && cm.Observed != "" && lm.Observed != cm.Observed {
		add("model", "observed", lm.Observed, cm.Observed, SevError)
	}
	return ds
}

func diffMap(a, b map[string]string, f func(k, a, b string)) {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for _, k := range sortedKeys(keys) {
		av, aok := a[k]
		bv, bok := b[k]
		switch {
		case !aok:
			f(k, "(absent)", bv)
		case !bok:
			f(k, av, "(absent)")
		case av != bv:
			f(k, av, bv)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// HasErrors 是否存在错误级漂移。
func HasErrors(ds []Drift) bool {
	for _, d := range ds {
		if d.Severity == SevError {
			return true
		}
	}
	return false
}

// Enforce 按模式处置漂移：全部经 logf 报告；strict 下错误级漂移返回错误。
func Enforce(mode Mode, ds []Drift, logf func(string, ...any)) error {
	for _, d := range ds {
		logf("⚠️ 环境漂移 %s", d)
	}
	if mode == ModeStrict && HasErrors(ds) {
		return fmt.Errorf("envlock: strict 模式下检测到 %d 处环境漂移，拒绝启动（运行 claw env check 查看，确认后 claw env lock 重新锁定）", len(ds))
	}
	return nil
}

// ToolNames 锁里声明的工具名（check 时按同一集合探测）。
func (l *Lock) ToolNames() []string { return sortedKeys(l.Runtime.Tools) }
