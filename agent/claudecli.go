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
	Bin string // defaults to "claude"
}

type claudeOutput struct {
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	NumTurns         int             `json:"num_turns"`
}

func (c ClaudeCLI) Run(ctx context.Context, step StepDef, prompt, dir string) (AgentResult, error) {
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

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // args come from reviewed step files
	cmd.Dir = dir

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
