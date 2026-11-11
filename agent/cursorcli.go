package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

var (
	ErrUnsupportedStep = errors.New("runner can't run this step")
	ErrBadOutput       = errors.New("agent output doesn't match the step's schema")
)

// readOnlyTools are the only tools a Cursor step may list: it runs in Cursor's
// ask mode, which can read the workspace but can't write files or run shell
// commands.
var readOnlyTools = []string{"Read", "Glob", "Grep"}

// CursorCLI runs a step through the Cursor CLI in headless mode, using the
// machine's Cursor login. It runs read-only steps only. Cursor's print mode
// otherwise has every tool, including write and shell, and the step file's
// tool and permission settings don't map onto Cursor's, so a step that
// needs more is refused rather than given more than it asked for.
//
// Cursor has no schema flag, so the schema goes in the prompt and the output
// is checked here. It reports tokens, not cost, so CostUSD is 0.
type CursorCLI struct {
	Bin string // defaults to cursor-agent on PATH, then ~/.local/bin
}

type cursorOutput struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// Check reports whether a step can run on Cursor, so a bad choice is refused
// when it's made rather than when a ticket reaches the step.
func (c CursorCLI) Check(step StepDef) error {
	if step.PermissionMode != "" || len(step.AllowedTools) > 0 {
		return fmt.Errorf("step %s: %w: cursor-cli runs read-only steps, and this one has permissions", step.Name, ErrUnsupportedStep)
	}
	for _, tool := range step.Tools {
		if !slices.Contains(readOnlyTools, tool) {
			return fmt.Errorf("step %s: %w: cursor-cli runs read-only steps, and this one uses %s", step.Name, ErrUnsupportedStep, tool)
		}
	}
	return nil
}

func (c CursorCLI) Run(ctx context.Context, step StepDef, prompt, dir string) (AgentResult, error) {
	if err := c.Check(step); err != nil {
		return AgentResult{}, err
	}
	if dir == "" {
		tmp, err := os.MkdirTemp("", "aiw-step-")
		if err != nil {
			return AgentResult{}, fmt.Errorf("step %s: temp dir: %w", step.Name, err)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}
	if step.Schema != nil {
		schema, err := json.Marshal(step.Schema)
		if err != nil {
			return AgentResult{}, fmt.Errorf("step %s schema: %w", step.Name, err)
		}
		prompt += "\n\nReply with only a JSON object that matches this JSON schema. No prose and no code fence.\n" + string(schema)
	}
	args := []string{
		"-p", prompt,
		"--model", step.Model,
		"--output-format", "json",
		// Ask mode is read-only: it can't write files or run commands.
		"--mode", "ask",
		"--workspace", dir,
		// A fresh checkout each time; without this it waits for a prompt.
		"--trust",
	}
	cmd := exec.CommandContext(ctx, c.bin(), args...) //nolint:gosec // args come from reviewed step files
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return AgentResult{}, fmt.Errorf("step %s: run cursor-agent: %w", step.Name, err)
	}
	var res cursorOutput
	if err := json.Unmarshal(out, &res); err != nil {
		return AgentResult{}, fmt.Errorf("step %s: parse cursor-agent output: %w", step.Name, err)
	}
	if res.IsError {
		return AgentResult{}, fmt.Errorf("step %s: %w: %s", step.Name, ErrAgentFailed, res.Result)
	}
	if step.Schema == nil {
		text, _ := json.Marshal(res.Result)
		return AgentResult{Output: text}, nil
	}
	output, err := schemaOutput(step, res.Result)
	if err != nil {
		return AgentResult{}, err
	}
	return AgentResult{Output: output}, nil
}

// schemaOutput pulls the JSON object out of a text reply (tolerating a code
// fence or a sentence around it) and checks it has the schema's required
// fields. It's a light check, not full JSON Schema validation: the harness
// decodes into typed structs next, which catches wrong types.
func schemaOutput(step StepDef, text string) (json.RawMessage, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("step %s: %w: no JSON object in %q", step.Name, ErrBadOutput, clip(text))
	}
	raw := json.RawMessage(text[start : end+1])
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("step %s: %w: %w", step.Name, ErrBadOutput, err)
	}
	required, _ := step.Schema["required"].([]any)
	for _, r := range required {
		if name, ok := r.(string); ok {
			if _, found := obj[name]; !found {
				return nil, fmt.Errorf("step %s: %w: missing %q", step.Name, ErrBadOutput, name)
			}
		}
	}
	return raw, nil
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func (c CursorCLI) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	if p, err := exec.LookPath("cursor-agent"); err == nil {
		return p
	}
	// The installer puts it here, which isn't always on PATH.
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "bin", "cursor-agent")
	}
	return "cursor-agent"
}
