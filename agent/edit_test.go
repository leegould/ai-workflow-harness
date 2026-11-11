package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stepsCopy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../steps")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEditPromptChangesOnlyThePrompt(t *testing.T) {
	dir := stepsCopy(t)
	before, err := os.ReadFile(filepath.Join(dir, "readiness.md")) //nolint:gosec // test copy
	if err != nil {
		t.Fatal(err)
	}
	if err := EditPrompt(dir, "readiness", "ask at most 3 questions", "ask at most 1 question"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "readiness.md")) //nolint:gosec // test copy
	if err != nil {
		t.Fatal(err)
	}
	front := func(b []byte) string { f, _, _ := splitFrontmatter(string(b)); return f }
	if front(before) != front(after) {
		t.Fatal("frontmatter changed")
	}
	if !strings.Contains(string(after), "ask at most 1 question") {
		t.Fatal("edit not applied")
	}
	if _, err := LoadStep(dir, "readiness"); err != nil {
		t.Fatal(err)
	}
}

func TestEditPromptRefuses(t *testing.T) {
	dir := stepsCopy(t)
	tests := map[string][2]string{
		"text not in the prompt": {"no such sentence", "x"},
		"frontmatter only":       {"max_budget_usd", "x"},
		"empty find":             {"", "x"},
		"breaks the template":    {"{{.Description}}", "{{.Description"},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			if err := CheckEdit(dir, "readiness", edit[0], edit[1]); err == nil {
				t.Fatal("expected a refusal")
			}
			if err := EditPrompt(dir, "readiness", edit[0], edit[1]); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
	if err := CheckEdit(dir, "readiness", "no such sentence", "x"); !errors.Is(err, ErrEditNoMatch) {
		t.Fatalf("got %v", err)
	}
	if _, err := PromptBody(dir, "missing"); err == nil {
		t.Fatal("expected an error for a missing step")
	}
}

func TestSetRunnerModelChangesOnlyThoseLines(t *testing.T) {
	dir := stepsCopy(t)
	before, _ := os.ReadFile(filepath.Join(dir, "spec.md")) //nolint:gosec // test copy
	if err := SetRunnerModel(dir, "spec", "cursor-cli", "composer-2.5"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "spec.md")) //nolint:gosec // test copy
	s, err := LoadStep(dir, "spec")
	if err != nil || s.Runner != "cursor-cli" || s.Model != "composer-2.5" {
		t.Fatalf("got %s/%s %v", s.Runner, s.Model, err)
	}
	b, a := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
	changed := 0
	for i := range b {
		if b[i] != a[i] {
			changed++
		}
	}
	if len(a) != len(b) || changed != 2 {
		t.Fatalf("want a two-line change, got %d lines changed", changed)
	}
	if err := SetRunnerModel(dir, "spec", "claude-cli", "x\ntools: [Bash]"); err == nil {
		t.Fatal("a model that injects frontmatter must be refused")
	}
	if err := SetRunnerModel(dir, "missing", "claude-cli", "claude-haiku-5-5"); err == nil {
		t.Fatal("expected an error for a missing step")
	}
}
