package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaudeEnv is fakeClaude that also records its environment to env.txt.
func fakeClaudeEnv(t *testing.T, output string) (bin, argsFile, envFile string) {
	t.Helper()
	bin, argsFile = fakeClaude(t, output)
	envFile = filepath.Join(filepath.Dir(bin), "env.txt")
	script, err := os.ReadFile(bin) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	withEnv := strings.Replace(string(script), "#!/bin/sh\n", "#!/bin/sh\nenv > "+envFile+"\n", 1)
	if err := os.WriteFile(bin, []byte(withEnv), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return bin, argsFile, envFile
}

func TestClaudeCLINativeSandbox(t *testing.T) {
	if _, err := NativeSandbox(); err != nil {
		t.Skipf("no OS sandbox here: %v", err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	bin, argsFile, envFile := fakeClaudeEnv(t, `{"is_error":false,"result":"ok"}`)
	c := ClaudeCLI{Bin: bin, Sandbox: SandboxPolicy{Network: []string{"proxy.golang.org"}, CacheDir: cache}}
	step := StepDef{Name: "build", Model: "m", Sandbox: SandboxNative}
	if _, err := c.Run(context.Background(), step, "p", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile) //nolint:gosec // test fixture
	lines := strings.Split(string(args), "\n")
	var settings string
	for i, a := range lines {
		if a == "--settings" && i+1 < len(lines) {
			settings = lines[i+1]
		}
	}
	var got struct {
		Sandbox struct {
			Enabled                  bool `json:"enabled"`
			FailIfUnavailable        bool `json:"failIfUnavailable"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
			Network                  struct {
				AllowedDomains  []string `json:"allowedDomains"`
				StrictAllowlist bool     `json:"strictAllowlist"`
			} `json:"network"`
			Filesystem struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(settings), &got); err != nil {
		t.Fatalf("settings %q: %v", settings, err)
	}
	sb := got.Sandbox
	if !sb.Enabled || !sb.FailIfUnavailable || sb.AllowUnsandboxedCommands || !sb.Network.StrictAllowlist ||
		strings.Join(sb.Network.AllowedDomains, ",") != "proxy.golang.org" ||
		strings.Join(sb.Filesystem.AllowWrite, ",") != cache {
		t.Fatalf("got %+v", sb)
	}
	env, _ := os.ReadFile(envFile) //nolint:gosec // test fixture
	for _, want := range []string{"GOCACHE=" + filepath.Join(cache, "go-build"), "GOMODCACHE=" + filepath.Join(cache, "go-mod")} {
		if !strings.Contains(string(env), want+"\n") {
			t.Errorf("env missing %s", want)
		}
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache dir not created: %v", err)
	}
}

func TestClaudeCLIUnsandboxedHasNoSettings(t *testing.T) {
	bin, argsFile, envFile := fakeClaudeEnv(t, `{"is_error":false,"result":"ok"}`)
	t.Setenv("GOCACHE", "/mine")
	c := ClaudeCLI{Bin: bin, Sandbox: SandboxPolicy{CacheDir: filepath.Join(t.TempDir(), "cache")}}
	if _, err := c.Run(context.Background(), StepDef{Name: "x", Model: "m", Sandbox: SandboxNone}, "p", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile) //nolint:gosec // test fixture
	env, _ := os.ReadFile(envFile)   //nolint:gosec // test fixture
	if strings.Contains(string(args), "--settings") || !strings.Contains(string(env), "GOCACHE=/mine\n") {
		t.Fatalf("an unsandboxed step got sandbox settings:\n%s", args)
	}
}

func TestSandboxChecks(t *testing.T) {
	bin, argsFile := fakeClaude(t, `{"is_error":false,"result":"ok"}`)
	step := StepDef{Name: "build", Model: "m", Sandbox: SandboxOpenSandbox}
	// Claude Code can't run in OpenSandbox yet, so it's refused rather than
	// quietly run without one.
	if _, err := (ClaudeCLI{Bin: bin}).Run(context.Background(), step, "p", t.TempDir()); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("claude ran")
	}
	step = StepDef{Name: "review", Model: "m", Tools: []string{"Read"}, Sandbox: SandboxNative}
	if err := (CursorCLI{}).Check(step); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestStepSandboxValues(t *testing.T) {
	s, err := parseStep("x", "---\nrunner: r\nmodel: m\n---\nbody")
	if err != nil || s.Sandbox != SandboxNone {
		t.Fatalf("got %q %v, want none by default", s.Sandbox, err)
	}
	if _, err := parseStep("x", "---\nrunner: r\nmodel: m\nsandbox: docker\n---\nbody"); err == nil {
		t.Fatal("expected an unknown sandbox to be refused")
	}
}

func TestSetSandbox(t *testing.T) {
	dir := stepsCopy(t)
	if err := SetSandbox(dir, "review", SandboxNative); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadStep(dir, "review"); err != nil || s.Sandbox != SandboxNative {
		t.Fatalf("got %q %v", s.Sandbox, err)
	}
	if err := SetSandbox(dir, "review", "docker"); err == nil {
		t.Fatal("expected an unknown sandbox to be refused")
	}
	if err := SetSandbox(dir, "review", "none\ntools: [Bash]"); err == nil {
		t.Fatal("a value that injects frontmatter must be refused")
	}
}
