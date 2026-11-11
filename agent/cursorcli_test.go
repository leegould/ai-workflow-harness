package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCursorCLI(t *testing.T) {
	step, err := LoadStep("../steps", "spec")
	if err != nil {
		t.Fatal(err)
	}
	step.Model = "composer-2.5"
	reply := `{"goal":"g","in_scope":[],"out_of_scope":[],"decisions":[],"files":[],"done_when":[]}`
	tests := []struct {
		name    string
		output  string
		wantErr error
	}{
		{"bare JSON", `{"is_error":false,"result":` + quote(reply) + `}`, nil},
		{"fenced JSON", `{"is_error":false,"result":` + quote("Here it is:\n```json\n"+reply+"\n```") + `}`, nil},
		{"missing a required field", `{"is_error":false,"result":` + quote(`{"goal":"g"}`) + `}`, ErrBadOutput},
		{"no JSON at all", `{"is_error":false,"result":"I couldn't read the repo."}`, ErrBadOutput},
		{"agent error", `{"is_error":true,"result":"not logged in"}`, ErrAgentFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin, argsFile := fakeClaude(t, tc.output)
			dir := t.TempDir()
			res, err := CursorCLI{Bin: bin}.Run(context.Background(), step, "the prompt", dir)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || string(res.Output) != reply {
				t.Fatalf("got %s %v", res.Output, err)
			}
			raw, _ := os.ReadFile(argsFile) //nolint:gosec // test fixture
			args := string(raw)
			for _, want := range []string{"--mode\nask\n", "--model\ncomposer-2.5\n", "--workspace\n" + dir + "\n", "--trust\n", "matches this JSON schema"} {
				if !strings.Contains(args, want) {
					t.Errorf("args missing %q:\n%s", want, args)
				}
			}
		})
	}
}

// Cursor runs in ask mode, so only read-only steps are allowed on it.
func TestCursorCLIRunsReadOnlyStepsOnly(t *testing.T) {
	for name, ok := range map[string]bool{"readiness": true, "spec": true, "retro": true, "build": false} {
		step, err := LoadStep("../steps", name)
		if err != nil {
			t.Fatal(err)
		}
		err = CursorCLI{}.Check(step)
		if ok != (err == nil) {
			t.Errorf("%s: got %v", name, err)
		}
		if !ok && !errors.Is(err, ErrUnsupportedStep) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := (CursorCLI{}).Run(context.Background(), StepDef{Name: "x", Tools: []string{"Bash"}}, "", ""); !errors.Is(err, ErrUnsupportedStep) {
		t.Fatalf("got %v", err)
	}
}

func TestRunnersCheck(t *testing.T) {
	r := Runners{"cursor-cli": CursorCLI{}, "claude-cli": ClaudeCLI{}}
	if err := r.Check(StepDef{Name: "x", Runner: "nope"}); !errors.Is(err, ErrUnknownRunner) {
		t.Fatalf("got %v", err)
	}
	if err := r.Check(StepDef{Name: "x", Runner: "claude-cli", Tools: []string{"Bash"}}); err != nil {
		t.Fatalf("claude-cli runs anything: got %v", err)
	}
	if err := r.Check(StepDef{Name: "x", Runner: "cursor-cli", Tools: []string{"Bash"}}); !errors.Is(err, ErrUnsupportedStep) {
		t.Fatalf("got %v", err)
	}
}

func quote(s string) string {
	b := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
	return `"` + b + `"`
}
