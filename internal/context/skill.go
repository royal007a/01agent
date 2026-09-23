// Package context 是上下文工程体系（第 10-14 讲）：提示词组装、技能加载、会话、压缩与错误自愈。
// 引用时建议起别名 ctxpkg，避免与标准库 context 冲突。
package context

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill 定义了从 SKILL.md 中解析出的标准化技能结构（Agent Skills 规范）。
type Skill struct {
	Name        string
	Description string
	Body        string // Markdown 正文指令
	Path        string
}

// SkillLoader 从 <workDir>/.claw/skills/*/SKILL.md 加载技能。
type SkillLoader struct {
	workDir string
}

func NewSkillLoader(workDir string) *SkillLoader { return &SkillLoader{workDir: workDir} }

// Discover 扫描并解析所有技能，按名称排序保证提示词稳定。
func (s *SkillLoader) Discover() []Skill {
	base := filepath.Join(s.workDir, ".claw", "skills")
	if _, err := os.Stat(base); err != nil {
		return nil
	}
	var skills []Skill
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == "SKILL.md" {
			if content, err := os.ReadFile(path); err == nil {
				sk := parseSkillMD(string(content))
				sk.Path = path
				skills = append(skills, sk)
			}
		}
		return nil
	})
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills
}

// LoadAll 渴望式加载：把所有技能的元数据与正文拼成一段系统提示词（第 10 讲的实现）。
func (s *SkillLoader) LoadAll() string {
	skills := s.Discover()
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n### 可用专业技能 (Agent Skills)\n")
	b.WriteString("以下是你拥有的标准化外挂技能，请在符合 description 描述的场景下严格遵循其正文指令：\n\n")
	for _, sk := range skills {
		fmt.Fprintf(&b, "#### 技能名称: %s\n**触发条件**: %s\n\n**执行指南**:\n%s\n\n---\n", sk.Name, sk.Description, sk.Body)
	}
	return b.String()
}

// parseSkillMD 极简解析带 YAML Frontmatter 的 Markdown。
func parseSkillMD(content string) Skill {
	skill := Skill{Name: "Unknown Skill", Description: "No description provided.", Body: strings.TrimSpace(content)}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return skill
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return skill
	}
	frontmatter := rest[:end]
	skill.Body = strings.TrimSpace(strings.TrimPrefix(rest[end+4:], "\n"))
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			skill.Name = unquote(v)
		} else if v, ok := strings.CutPrefix(line, "description:"); ok {
			skill.Description = unquote(v)
		}
	}
	return skill
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return v
}
