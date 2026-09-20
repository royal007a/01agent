package prompt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposerUsesProgressiveDisclosureAndPinnedSkillBodies(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("Run focused tests."), 0o600); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(workDir, ".01agent", "skills", "review")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	skillText := "---\nname: review\ndescription: Review concrete evidence.\n---\n# Private body\nInspect the tests.\n"
	if err := os.WriteFile(skillPath, []byte(skillText), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := (FilesystemComposer{}).Snapshot(context.Background(), workDir, "Base prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.SystemPrompt, "Run focused tests.") || !strings.Contains(snapshot.SystemPrompt, "review: Review concrete evidence.") {
		t.Fatalf("system prompt=%q", snapshot.SystemPrompt)
	}
	if strings.Contains(snapshot.SystemPrompt, "Inspect the tests.") {
		t.Fatalf("skill body was eagerly injected: %q", snapshot.SystemPrompt)
	}
	if snapshot.Digest == "" || snapshot.AgentsDigest == "" || snapshot.SkillsDigest == "" {
		t.Fatalf("snapshot=%#v", snapshot)
	}

	if err := os.WriteFile(skillPath, []byte(strings.ReplaceAll(skillText, "Inspect the tests.", "MUTATED")), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := NewReadSkillTool()
	output, err := tool.Execute(WithSnapshot(context.Background(), snapshot), json.RawMessage(`{"name":"review"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "Inspect the tests.") || strings.Contains(output, "MUTATED") {
		t.Fatalf("read_skill did not use pinned body: %s", output)
	}
}

func TestComposerRejectsMalformedAndSymlinkedInstructionSources(t *testing.T) {
	t.Run("malformed skill", func(t *testing.T) {
		workDir := t.TempDir()
		skillDir := filepath.Join(workDir, ".01agent", "skills", "bad")
		if err := os.MkdirAll(skillDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("missing frontmatter"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (FilesystemComposer{}).Snapshot(context.Background(), workDir, "base"); err == nil || !strings.Contains(err.Error(), "frontmatter") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("symlink agents", func(t *testing.T) {
		workDir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("escape"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workDir, "AGENTS.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := (FilesystemComposer{}).Snapshot(context.Background(), workDir, "base"); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestReadSkillRejectsUnknownOrUnpinnedSkills(t *testing.T) {
	tool := NewReadSkillTool()
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"review"}`)); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("missing snapshot error=%v", err)
	}
	snapshot := Snapshot{Skills: map[string]Skill{}}
	if _, err := tool.Execute(WithSnapshot(context.Background(), snapshot), json.RawMessage(`{"name":"review"}`)); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("unknown skill error=%v", err)
	}
}
