package prompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

const maximumInstructionBytes = 256 << 10

var safeSkillName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Skill struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
	Body        string `json:"body,omitempty" yaml:"-"`
	Digest      string `json:"digest" yaml:"-"`
	Path        string `json:"path" yaml:"-"`
}

type Snapshot struct {
	SystemPrompt string           `json:"system_prompt"`
	Digest       string           `json:"digest"`
	AgentsDigest string           `json:"agents_digest,omitempty"`
	SkillsDigest string           `json:"skills_digest"`
	AgentsPath   string           `json:"agents_path,omitempty"`
	Skills       map[string]Skill `json:"-"`
}

type Composer interface {
	Snapshot(context.Context, string, string) (Snapshot, error)
}

// FilesystemComposer loads workspace-local instructions from AGENTS.md and
// skill metadata from .01agent/skills/<name>/SKILL.md. Skill bodies are kept
// out of the system prompt and exposed only through read_skill.
type FilesystemComposer struct{}

func (FilesystemComposer) Snapshot(ctx context.Context, workDir, basePrompt string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve prompt workspace: %w", err)
	}
	abs = filepath.Clean(abs)
	basePrompt = strings.TrimSpace(basePrompt)
	if basePrompt == "" {
		return Snapshot{}, errors.New("base system prompt is required")
	}

	snapshot := Snapshot{Skills: make(map[string]Skill)}
	agentsPath := filepath.Join(abs, "AGENTS.md")
	agents, exists, err := readOptionalRegular(agentsPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load AGENTS.md: %w", err)
	}
	if exists {
		snapshot.AgentsPath = agentsPath
		snapshot.AgentsDigest = digest(agents)
	}
	if err := loadSkills(filepath.Join(abs, ".01agent", "skills"), snapshot.Skills); err != nil {
		return Snapshot{}, err
	}

	skills := sortedSkills(snapshot.Skills)
	skillManifest := make([]map[string]string, 0, len(skills))
	for _, skill := range skills {
		skillManifest = append(skillManifest, map[string]string{
			"name": skill.Name, "description": skill.Description, "digest": skill.Digest,
		})
	}
	encodedSkills, _ := json.Marshal(skillManifest)
	snapshot.SkillsDigest = digest(encodedSkills)

	var prompt strings.Builder
	prompt.WriteString(basePrompt)
	prompt.WriteString("\n\nWorkspace: ")
	prompt.WriteString(abs)
	if exists && strings.TrimSpace(string(agents)) != "" {
		prompt.WriteString("\n\n# Workspace instructions (AGENTS.md)\n")
		prompt.WriteString(strings.TrimSpace(string(agents)))
	}
	if len(skills) > 0 {
		prompt.WriteString("\n\n# Available skills\n")
		prompt.WriteString("Skill bodies are not embedded. Call read_skill with the exact name before following a relevant skill.\n")
		for _, skill := range skills {
			fmt.Fprintf(&prompt, "- %s: %s (revision %s)\n", skill.Name, skill.Description, skill.Digest[:12])
		}
	}
	snapshot.SystemPrompt = prompt.String()
	manifest := struct {
		SystemPrompt string              `json:"system_prompt"`
		AgentsDigest string              `json:"agents_digest"`
		Skills       []map[string]string `json:"skills"`
	}{snapshot.SystemPrompt, snapshot.AgentsDigest, skillManifest}
	encoded, _ := json.Marshal(manifest)
	snapshot.Digest = digest(encoded)
	return snapshot, nil
}

func loadSkills(root string, target map[string]Skill) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect skill directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New(".01agent/skills must be a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list skills: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(root, entry.Name(), "SKILL.md")
		content, exists, err := readOptionalRegular(path)
		if err != nil {
			return fmt.Errorf("load skill %q: %w", entry.Name(), err)
		}
		if !exists {
			continue
		}
		skill, err := parseSkill(path, content)
		if err != nil {
			return err
		}
		if _, duplicate := target[skill.Name]; duplicate {
			return fmt.Errorf("duplicate skill name %q", skill.Name)
		}
		target[skill.Name] = skill
	}
	return nil
}

func parseSkill(path string, content []byte) (Skill, error) {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Skill{}, fmt.Errorf("skill %q must start with YAML frontmatter", path)
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return Skill{}, fmt.Errorf("skill %q has unterminated YAML frontmatter", path)
	}
	end += 4
	var metadata struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(text[4:end]), &metadata); err != nil {
		return Skill{}, fmt.Errorf("parse skill %q frontmatter: %w", path, err)
	}
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = strings.TrimSpace(metadata.Description)
	if !safeSkillName.MatchString(metadata.Name) || metadata.Description == "" {
		return Skill{}, fmt.Errorf("skill %q requires a safe name and non-empty description", path)
	}
	body := strings.TrimSpace(text[end+5:])
	if body == "" {
		return Skill{}, fmt.Errorf("skill %q has an empty body", path)
	}
	return Skill{
		Name: metadata.Name, Description: metadata.Description, Body: body,
		Digest: digest(content), Path: path,
	}, nil
}

func readOptionalRegular(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximumInstructionBytes {
		return nil, false, fmt.Errorf("%q must be a regular file no larger than %d bytes", path, maximumInstructionBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximumInstructionBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(content) > maximumInstructionBytes {
		return nil, false, fmt.Errorf("%q exceeds %d bytes", path, maximumInstructionBytes)
	}
	return content, true, nil
}

func sortedSkills(items map[string]Skill) []Skill {
	result := make([]Skill, 0, len(items))
	for _, skill := range items {
		result = append(result, skill)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

type snapshotContextKey struct{}

func WithSnapshot(ctx context.Context, snapshot Snapshot) context.Context {
	return context.WithValue(ctx, snapshotContextKey{}, snapshot)
}

func SnapshotFromContext(ctx context.Context) (Snapshot, bool) {
	snapshot, ok := ctx.Value(snapshotContextKey{}).(Snapshot)
	return snapshot, ok
}
