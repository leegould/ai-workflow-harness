package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude writes a script that records its arguments to args.txt and
// prints output, standing in for the claude binary.
func fakeClaude(t *testing.T, output string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> " + argsFile + "; done\ncat <<'EOF'\n" + output + "\nEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestClaudeCLI(t *testing.T) {
	step := StepDef{
		Name: "refinement", Model: "claude-sonnet-5-5", MaxBudgetUSD: 0.25,
		Schema: map[string]any{"type": "object"},
	}
	tests := []struct {
		name     string
		output   string
		want     string
		wantErr  error
		wantCost float64
	}{
		{
			name:     "structured output",
			output:   `{"is_error":false,"structured_output":{"ready":true},"total_cost_usd":0.01,"num_turns":2}`,
			want:     `{"ready":true}`,
			wantCost: 0.01,
		},
		{
			name:   "plain text result when no schema output",
			output: `{"is_error":false,"result":"hello"}`,
			want:   `"hello"`,
		},
		{
			name:    "agent error",
			output:  `{"is_error":true,"result":"Not logged in"}`,
			wantErr: ErrAgentFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin, argsFile := fakeClaude(t, tc.output)
			res, err := ClaudeCLI{Bin: bin}.Run(context.Background(), step, "the prompt", t.TempDir())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(res.Output) != tc.want || res.CostUSD != tc.wantCost {
				t.Fatalf("got %s cost %v", res.Output, res.CostUSD)
			}
			args, err := os.ReadFile(argsFile) //nolint:gosec // test fixture
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"the prompt", "claude-sonnet-5-5", "--max-budget-usd\n0.25", `--json-schema` + "\n" + `{"type":"object"}`} {
				if !strings.Contains(string(args), want) {
					t.Errorf("args missing %q:\n%s", want, args)
				}
			}
		})
	}
}

func TestClaudeCLIBadOutput(t *testing.T) {
	bin, _ := fakeClaude(t, "not json")
	if _, err := (ClaudeCLI{Bin: bin}).Run(context.Background(), StepDef{Name: "x"}, "p", ""); err == nil {
		t.Fatal("expected parse error")
	}
}
