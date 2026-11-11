package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aiharness/target"
)

func TestRiskAssess(t *testing.T) {
	cfg, err := LoadConfig("../pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		changes []target.FileChange
		want    Level
		reason  string
	}{
		{"small code change", []target.FileChange{{Path: "poster.go", Added: 10}}, Low, "no risky paths"},
		{"the CLI the smoke test runs", []target.FileChange{{Path: "main.go", Added: 2}}, Medium, "smoke test"},
		{"agent rules", []target.FileChange{{Path: "CLAUDE.md", Added: 1}}, High, "rules"},
		{"nested markdown matches by name", []target.FileChange{{Path: "docs/x.md", Added: 1}}, High, "docs/x.md"},
		{"dependencies", []target.FileChange{{Path: "go.mod", Added: 1}}, High, "dependencies"},
		{"big diff", []target.FileChange{{Path: "poster.go", Added: 120, Deleted: 40}}, Medium, "160 lines"},
		{"huge diff", []target.FileChange{{Path: "poster.go", Added: 500}}, High, "500 lines"},
		{"highest wins", []target.FileChange{{Path: "main.go", Added: 1}, {Path: "go.sum", Added: 1}}, High, "go.sum"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, why := cfg.Risk.Assess(tc.changes)
			if got != tc.want || !strings.Contains(strings.Join(why, "\n"), tc.reason) {
				t.Fatalf("got %s %v", got, why)
			}
		})
	}
}

func TestLoadConfigRejectsBadRules(t *testing.T) {
	tests := map[string]string{
		"unknown level": "risk:\n  rules:\n    - paths: [x]\n      level: severe\n",
		"bad pattern":   "risk:\n  rules:\n    - paths: [\"[\"]\n      level: low\n",
		"not yaml":      "risk: [",
		"bad approval":  "spec:\n  approval: sometimes\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "p.yaml")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(p); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := LoadConfig("missing.yaml"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestDefaultMaxRounds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(p, []byte("risk: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadConfig(p); err != nil || c.MaxRounds != 3 || c.Spec.Approval != SpecRisky {
		t.Fatalf("got %+v %v", c, err)
	}
}
