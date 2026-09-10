package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/skills"
)

func TestEmbeddedSkillHasFrontmatter(t *testing.T) {
	raw, err := skills.FS.ReadFile(skills.Name + "/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.HasPrefix(s, "---\nname: envctl\n") {
		t.Fatalf("SKILL.md must start with frontmatter naming the skill:\n%.80s", s)
	}
	if !strings.Contains(s, "description: ") || !strings.Contains(s, "Use when") {
		t.Fatal("SKILL.md description must say what it does and when to use it")
	}
	if lines := strings.Count(s, "\n"); lines > 100 {
		t.Errorf("SKILL.md is %d lines; keep it under 100 and move detail to REFERENCE.md", lines)
	}
}

func TestWriteSkillCopiesEveryFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".claude", "skills", "envctl")
	if err := writeSkill(dst); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"SKILL.md", "REFERENCE.md"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Errorf("%s not installed: %v", f, err)
		}
	}
}
