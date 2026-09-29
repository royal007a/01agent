package knowledge

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Manager 把内层暂存（Inbox）与外层归位（Placer）组合起来，供引擎在任务收尾时调用。
type Manager struct {
	Inbox  *Inbox
	Placer *Placer
}

func NewManager(inboxRoot string) *Manager {
	return &Manager{Inbox: NewInbox(inboxRoot), Placer: &Placer{}}
}

// NeedsCheckpoint 本轮任务（since 之后）是否还没做"交付前知识清点"。
func (m *Manager) NeedsCheckpoint(sessionID string, since time.Time) bool {
	return !m.Inbox.CheckedSince(sessionID, since)
}

// Settle 外层归位：取走会话暂存区的全部条目写入工作区知识载体；失败时条目保留，下次重试。
func (m *Manager) Settle(sessionID, workDir string) (Result, error) {
	var res Result
	err := m.Inbox.Take(sessionID, func(es []Entry) error {
		r, err := m.Placer.Place(workDir, es)
		res = r
		return err
	})
	return res, err
}

// MaxPromptBytes 注入 System Prompt 的知识摘要上限。
const MaxPromptBytes = 4000

// RenderPrompt 把工作区的已沉淀知识渲染为 System Prompt 片段：规则置顶、已裁决决策、用户偏好（倾向）、近期发现。
// 这是闭环的另一半：下一个会话一开始就能看到"用户说过不要用某库""这个单词已经掌握"。
func RenderPrompt(workDir string) string {
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return ""
	}
	defer root.Close()
	idx, err := loadIndex(root, LoadLayout(workDir).IndexFile)
	if err != nil || len(idx) == 0 {
		return ""
	}
	var rules, decisions, prefs, discs []string
	for _, e := range active(idx, Decision) {
		line := fmt.Sprintf("- %s：%s（适用：%s；%s，%s）", e.Subject, e.Content, e.Scope, sourceLabel(e.Source), e.ID)
		if e.Rule {
			rules = append(rules, line)
		} else {
			decisions = append(decisions, line)
		}
	}
	for _, e := range active(idx, UserTriage) {
		prefs = append(prefs, fmt.Sprintf("- %s：%s（适用场景：%s）", e.Subject, e.Content, e.Scope))
	}
	ds := active(idx, Discovery)
	if len(ds) > 8 {
		ds = ds[len(ds)-8:] // 只注入最近的发现
	}
	for _, e := range ds {
		discs = append(discs, fmt.Sprintf("- %s：%s（来源：%s，%s）", e.Subject, e.Content, e.Source, e.RecordedAt.Format("2006-01-02")))
	}
	var b strings.Builder
	b.WriteString("\n# 跨会话沉淀的知识（由 Harness 维护）\n")
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("\n## " + title + "\n")
		for _, l := range lines {
			if b.Len()+len(l) > MaxPromptBytes {
				b.WriteString("- …（更多条目见 .claw/knowledge/ 下的文件）\n")
				return
			}
			b.WriteString(l + "\n")
		}
	}
	section("必须遵守的规则（用户或项目已裁决，冲突时以此为准）", rules)
	section("已做出的决策", decisions)
	section("用户偏好（倾向，不是定论；与规则冲突时以规则为准）", prefs)
	section("近期发现（可能随代码变化过期，使用前可再核实）", discs)
	return b.String()
}
