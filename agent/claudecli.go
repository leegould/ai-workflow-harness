package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

var ErrAgentFailed = errors.New("agent run failed")

// ClaudeCLI runs a step through Claude Code in headless mode, using the
// machine's Claude Code login.
type ClaudeCLI struct {
	Bin     string        // defaults to "claude"
	Sandbox SandboxPolicy // what a step with sandbox: native may reach
}

// Check reports whether this machine can run step's sandbox. Claude Code runs
// none or native; a native sandbox it can't start would otherwise fail the
// step at run time, or worse, run it unsandboxed.
func (c ClaudeCLI) Check(step StepDef) error {
	switch step.Sandbox {
	case "", SandboxNone:
		return nil
	case SandboxNative:
		if _, err := NativeSandbox(); err != nil {
			return fmt.Errorf("step %s: %w", step.Name, err)
		}
		return nil
	}
	return fmt.Errorf("step %s: %w: claude-cli doesn't run in %s yet", step.Name, ErrSandboxUnavailable, step.Sandbox)
}

type claudeOutput struct {
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	NumTurns         int             `json:"num_turns"`
}

func (c ClaudeCLI) Run(ctx context.Context, step StepDef, prompt, dir string) (AgentResult, error) {
	if err := c.Check(step); err != nil {
		return AgentResult{}, err
	}
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	if dir == "" {
		// A step with no repo runs in an empty directory, so the agent can't pick
		// up whatever happens to be in the worker's own folder.
		tmp, err := os.MkdirTemp("", "aiw-step-")
		if err != nil {
			return AgentResult{}, fmt.Errorf("step %s: temp dir: %w", step.Name, err)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}
	args := []string{
		"-p", prompt,
		"--model", step.Model,
		"--tools", strings.Join(step.Tools, ","),
		"--output-format", "json",
		"--no-session-persistence",
		// Ignore the machine's own settings and hooks: the step file is the whole
		// configuration. Restricted also confines file tools to dir.
		"--setting-sources", "",
		"--restricted",
		// Nobody is there to answer a permission prompt, so anything not
		// allowed below is denied rather than hanging.
		"--permission-prompts", "none",
	}
	if len(step.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(step.AllowedTools, ","))
	}
	if step.PermissionMode != "" {
		args = append(args, "--permission-mode", step.PermissionMode)
	}
	if step.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(step.MaxBudgetUSD, 'f', -1, 64))
	}
	if step.Schema != nil {
		schema, err := json.Marshal(step.Schema)
		if err != nil {
			return AgentResult{}, fmt.Errorf("step %s schema: %w", step.Name, err)
		}
		args = append(args, "--json-schema", string(schema))
	}
	var env []string // nil: the worker's own environment
	if step.Sandbox == SandboxNative {
		settings, sandboxEnv, err := claudeSandbox(c.Sandbox)
		if err != nil {
			return AgentResult{}, fmt.Errorf("step %s: %w", step.Name, err)
		}
		// --settings still applies under --setting-sources "" and --restricted.
		args = append(args, "--settings", settings)
		env = sandboxEnv
	}

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // args come from reviewed step files
	cmd.Dir = dir
	cmd.Env = env

	out, err := cmd.Output()
	if err != nil {
		return AgentResult{}, fmt.Errorf("step %s: run claude: %w", step.Name, err)
	}

	var res claudeOutput
	if err := json.Unmarshal(out, &res); err != nil {
		return AgentResult{}, fmt.Errorf("step %s: parse claude output: %w", step.Name, err)
	}
	if res.IsError {
		return AgentResult{}, fmt.Errorf("step %s: %w: %s", step.Name, ErrAgentFailed, res.Result)
	}
	output := res.StructuredOutput
	if output == nil {
		output = json.RawMessage(strconv.Quote(res.Result))
	}
	return AgentResult{Output: output, CostUSD: res.TotalCostUSD, Turns: res.NumTurns}, nil
}
