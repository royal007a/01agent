package knowledge

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Layout 工作区的知识布局：只有外层循环知道。可由 <工作区>/.claw/knowledge.json 覆盖。
type Layout struct {
	ADRDir          string `json:"adr_dir"`          // 决策记录目录
	RulesFile       string `json:"rules_file"`       // 所有任务都要遵守的约束
	PreferencesFile string `json:"preferences_file"` // 用户偏好（倾向）
	DiscoveriesFile string `json:"discoveries_file"` // 执行中核对出的事实
	IndexFile       string `json:"index_file"`       // 结构化索引（事实来源，markdown 由它渲染）
}

// LoadLayout 读取布局：默认值 + 可选的 .claw/knowledge.json；工作区已有 docs/adr 时优先沿用。
func LoadLayout(workDir string) Layout {
	l := Layout{
		ADRDir:          ".claw/knowledge/decisions",
		RulesFile:       ".claw/knowledge/rules.md",
		PreferencesFile: ".claw/knowledge/preferences.md",
		DiscoveriesFile: ".claw/knowledge/discoveries.md",
		IndexFile:       ".claw/knowledge/index.json",
	}
	if fi, err := os.Stat(filepath.Join(workDir, "docs", "adr")); err == nil && fi.IsDir() {
		l.ADRDir = "docs/adr"
	}
	if b, err := os.ReadFile(filepath.Join(workDir, ".claw", "knowledge.json")); err == nil {
		var o Layout
		if json.Unmarshal(b, &o) == nil {
			for _, p := range []struct {
				dst *string
				v   string
			}{{&l.ADRDir, o.ADRDir}, {&l.RulesFile, o.RulesFile}, {&l.PreferencesFile, o.PreferencesFile}, {&l.DiscoveriesFile, o.DiscoveriesFile}, {&l.IndexFile, o.IndexFile}} {
				if strings.TrimSpace(p.v) != "" {
					*p.dst = path.Clean(filepath.ToSlash(p.v))
				}
			}
		}
	}
	return l
}

// Placer 外层循环：把暂存区的条目统一归位到工作区的知识载体。
type Placer struct {
	mu sync.Mutex // 同一进程内串行归位，避免多个会话同时改同一工作区的知识文件
}

// Result 一次归位的统计。
type Result struct {
	Added, Duplicates, Superseded, Checkpoints int
	Files                                      []string
}

// Place 归位条目：去重 → 同主题取代 → 写 ADR → 重新渲染规则/偏好/发现文件。
// 所有文件访问经 os.Root，知识布局即使被配置成 "../x" 也写不出工作区。
func (p *Placer) Place(workDir string, entries []Entry) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var res Result
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return res, err
	}
	defer root.Close()
	l := LoadLayout(workDir)
	if err := recoverJournal(root, l); err != nil {
		return res, err
	}

	idx, err := loadIndex(root, l.IndexFile)
	if err != nil {
		return res, err
	}
	touched := map[string]bool{}
	for _, e := range entries {
		if e.Kind == None {
			res.Checkpoints++
			continue
		}
		if dup := findDuplicate(idx, e); dup != nil {
			res.Duplicates++
			continue
		}
		// 同主题已有生效的决策时，偏好只是它的弱化表达，不再单独入库（实测模型常把同一件事同时记成决策和偏好）
		if e.Kind == UserTriage && hasActive(idx, Decision, e.Subject) {
			res.Duplicates++
			continue
		}
		e.Status = Active
		e.ID = nextID(idx, e.Kind)
		// 同主题的旧条目被取代（决策、偏好、发现都适用：新事实覆盖旧事实）
		for i := range idx {
			old := &idx[i]
			sameKind := old.Kind == e.Kind
			// 决策高于偏好：新决策同时取代同主题的偏好
			decisionOverPref := e.Kind == Decision && old.Kind == UserTriage
			if old.Status == Active && (sameKind || decisionOverPref) && Norm(old.Subject) == Norm(e.Subject) {
				old.Status, old.SupersededBy = Superseded, e.ID
				res.Superseded++
				if old.Kind == Decision && old.Location != "" {
					touched[old.ID] = true
				}
			}
		}
		switch e.Kind {
		case Decision:
			e.Location = path.Join(l.ADRDir, fmt.Sprintf("%s-%s.md", strings.TrimPrefix(e.ID, "ADR-"), slug(e.Subject)))
			touched[e.ID] = true
		case UserTriage:
			e.Location = l.PreferencesFile
		case Discovery:
			e.Location = l.DiscoveriesFile
		}
		idx = append(idx, e)
		res.Added++
	}
	if res.Added == 0 && res.Superseded == 0 {
		return res, nil
	}
	// 两阶段提交：先把"新索引 + 全部待写文件"写成一份日志（journal），再逐个写文件，最后写索引、删日志。
	// 任何一步失败/进程崩溃，下次 Place 开始时都会先重放日志，保证文件与索引最终一致，不会漂移。
	var writes []fileWrite
	for i := range idx {
		e := idx[i]
		if e.Kind == Decision && touched[e.ID] {
			writes = append(writes, fileWrite{e.Location, renderADR(e)})
		}
	}
	writes = append(writes,
		fileWrite{l.RulesFile, renderRules(idx)},
		fileWrite{l.PreferencesFile, renderPreferences(idx)},
		fileWrite{l.DiscoveriesFile, renderDiscoveries(idx)},
	)
	b, _ := json.MarshalIndent(idx, "", "  ")
	writes = append(writes, fileWrite{l.IndexFile, string(b)}) // 索引最后写：它是提交点
	if err := commitJournal(root, journalPath(l), writes); err != nil {
		return res, err
	}
	for _, w := range writes {
		res.Files = append(res.Files, w.Path)
	}
	return res, nil
}

type fileWrite struct {
	Path string `json:"path"`
	Body string `json:"body"`
}

func journalPath(l Layout) string { return path.Join(path.Dir(l.IndexFile), ".journal.json") }

// commitJournal 写日志 → 应用 → 删日志。
func commitJournal(root *os.Root, jp string, writes []fileWrite) error {
	jb, _ := json.Marshal(writes)
	if err := writeFile(root, jp, string(jb)); err != nil {
		return fmt.Errorf("写知识归位日志失败: %w", err)
	}
	return applyJournal(root, jp, writes)
}

func applyJournal(root *os.Root, jp string, writes []fileWrite) error {
	for _, w := range writes {
		if err := writeFile(root, w.Path, w.Body); err != nil {
			return fmt.Errorf("归位写入 %s 失败（日志已保留，下次自动重放）: %w", w.Path, err)
		}
	}
	return root.Remove(jp)
}

// recoverJournal 若上次归位中途失败，先重放日志完成那次提交。
func recoverJournal(root *os.Root, l Layout) error {
	jp := journalPath(l)
	jb, err := root.ReadFile(jp)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var writes []fileWrite
	if err := json.Unmarshal(jb, &writes); err != nil {
		return fmt.Errorf("知识归位日志 %s 已损坏，拒绝继续（请人工检查）: %w", jp, err)
	}
	return applyJournal(root, jp, writes)
}

func hasActive(idx []Entry, k Kind, subject string) bool {
	for _, o := range idx {
		if o.Status == Active && o.Kind == k && Norm(o.Subject) == Norm(subject) {
			return true
		}
	}
	return false
}

func findDuplicate(idx []Entry, e Entry) *Entry {
	for i := range idx {
		o := &idx[i]
		if o.Status == Active && o.Kind == e.Kind && Norm(o.Subject) == Norm(e.Subject) && Norm(o.Content) == Norm(e.Content) && Norm(o.Scope) == Norm(e.Scope) {
			return o
		}
	}
	return nil
}

var idNum = regexp.MustCompile(`(\d+)$`)

func nextID(idx []Entry, k Kind) string {
	prefix := map[Kind]string{Decision: "ADR-", UserTriage: "PREF-", Discovery: "DISC-"}[k]
	max := 0
	for _, e := range idx {
		if strings.HasPrefix(e.ID, prefix) {
			if m := idNum.FindString(e.ID); m != "" {
				if n, _ := strconv.Atoi(m); n > max {
					max = n
				}
			}
		}
	}
	return fmt.Sprintf("%s%04d", prefix, max+1)
}

var slugBad = regexp.MustCompile(`[^\p{L}\p{N}]+`)

func slug(s string) string {
	s = strings.Trim(slugBad.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40])
	}
	if s == "" {
		s = "decision"
	}
	return s
}

func loadIndex(root *os.Root, file string) ([]Entry, error) {
	b, err := root.ReadFile(file)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取知识索引失败: %w", err)
	}
	var idx []Entry
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("知识索引 %s 已损坏，拒绝覆盖（请人工检查）: %w", file, err)
	}
	return idx, nil
}

// writeFile 经 os.Root 原子写（临时文件 + rename）。
func writeFile(root *os.Root, rel, body string) error {
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := fmt.Sprintf("%s.tmp-%d", rel, time.Now().UnixNano())
	if err := root.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

func active(idx []Entry, k Kind) []Entry {
	var out []Entry
	for _, e := range idx {
		if e.Kind == k && e.Status == Active {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sourceLabel(s string) string {
	switch s {
	case "user":
		return "用户裁决"
	case "model":
		return "模型决策"
	}
	return s
}

func renderADR(e Entry) string {
	status := "已采纳"
	if e.Status == Superseded {
		status = "已被 " + e.SupersededBy + " 取代"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s %s\n\n", e.ID, e.Subject)
	fmt.Fprintf(&b, "- 状态：%s\n- 来源：%s（会话 %s）\n- 日期：%s\n- 适用范围：%s\n", status, sourceLabel(e.Source), e.SessionID, e.RecordedAt.Format("2006-01-02"), e.Scope)
	if e.Rule {
		b.WriteString("- 类型：全局约束（所有任务必须遵守）\n")
	}
	fmt.Fprintf(&b, "\n## 结论\n\n%s\n\n## 理由\n\n%s\n", e.Content, e.Reason)
	return b.String()
}

func renderRules(idx []Entry) string {
	var b strings.Builder
	b.WriteString("# 规则（由 Harness 根据已裁决的决策自动维护，请勿手工编辑本文件；修改决策请改对应 ADR）\n\n")
	for _, e := range active(idx, Decision) {
		if e.Rule {
			fmt.Fprintf(&b, "- %s（适用：%s；依据 %s，%s）\n", e.Content, e.Scope, e.ID, sourceLabel(e.Source))
		}
	}
	return b.String()
}

func renderPreferences(idx []Entry) string {
	var b strings.Builder
	b.WriteString("# 用户偏好（用户统筹：表达倾向，不构成最终决策）\n\n")
	for _, e := range active(idx, UserTriage) {
		fmt.Fprintf(&b, "- [%s] %s：%s（适用场景：%s；记录于 %s）\n", e.ID, e.Subject, e.Content, e.Scope, e.RecordedAt.Format("2006-01-02"))
	}
	return b.String()
}

func renderDiscoveries(idx []Entry) string {
	var b strings.Builder
	b.WriteString("# 执行中的发现（带来源与时间，可能随代码变化而过期）\n\n")
	for _, e := range active(idx, Discovery) {
		fmt.Fprintf(&b, "- [%s] %s：%s（来源：%s；%s）\n", e.ID, e.Subject, e.Content, e.Source, e.RecordedAt.Format("2006-01-02 15:04"))
	}
	return b.String()
}
