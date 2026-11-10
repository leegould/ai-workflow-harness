// Package agent loads agent step definitions (steps/<name>.md) and runs them
// through a Runner: a harness like Claude Code, or a direct API loop later.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// StepDef is an agent step loaded from steps/<name>.md: YAML frontmatter for
// the run settings, and a prompt template as the body. Steps are read on every
// run, so editing one takes effect without a restart.
type StepDef struct {
	Name   string
	Runner string   `yaml:"runner"`
	Model  string   `yaml:"model"`
	Tools  []string `yaml:"tools"`
	// AllowedTools are permission rules the agent may use without asking,
	// e.g. "Bash(go test:*)". Headless runs can't ask, so anything else is denied.
	AllowedTools   []string       `yaml:"allowed_tools"`
	PermissionMode string         `yaml:"permission_mode"`
	MaxBudgetUSD   float64        `yaml:"max_budget_usd"`
	Schema         map[string]any `yaml:"schema"`
	prompt         *template.Template
}

func LoadStep(dir, name string) (StepDef, error) {
	raw, err := readStep(dir, name)
	if err != nil {
		return StepDef{}, err
	}
	return parseStep(name, raw)
}

func readStep(dir, name string) (string, error) {
	// os.Root confines reads to dir, so a step name can't reach outside it.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("open steps dir: %w", err)
	}
	defer root.Close()
	raw, err := root.ReadFile(name + ".md")
	if err != nil {
		return "", fmt.Errorf("load step %s: %w", name, err)
	}
	return string(raw), nil
}

func parseStep(name, raw string) (StepDef, error) {
	front, body, ok := splitFrontmatter(raw)
	if !ok {
		return StepDef{}, fmt.Errorf("step %s: missing --- frontmatter", name)
	}
	s := StepDef{Name: name}
	if err := yaml.Unmarshal([]byte(front), &s); err != nil {
		return StepDef{}, fmt.Errorf("step %s frontmatter: %w", name, err)
	}
	if s.Runner == "" || s.Model == "" {
		return StepDef{}, fmt.Errorf("step %s: runner and model are required", name)
	}
	var err error
	s.prompt, err = template.New(name).Option("missingkey=error").Parse(body)
	if err != nil {
		return StepDef{}, fmt.Errorf("step %s prompt: %w", name, err)
	}
	return s, nil
}

func splitFrontmatter(s string) (front, body string, ok bool) {
	rest, found := strings.CutPrefix(s, "---\n")
	if !found {
		return "", "", false
	}
	front, body, found = strings.Cut(rest, "\n---\n")
	return front, body, found
}

func (s StepDef) Render(data any) (string, error) {
	var b bytes.Buffer
	if err := s.prompt.Execute(&b, data); err != nil {
		return "", fmt.Errorf("render step %s: %w", s.Name, err)
	}
	return b.String(), nil
}

type AgentResult struct {
	Output  json.RawMessage // matches the step's schema
	CostUSD float64
	Turns   int
}

// Runner executes one agent step in dir (a repo checkout, or "" for a step
// that needs none). Implementations wrap a harness or an API (Claude Code CLI
// now; OpenRouter or another CLI later), chosen per step by StepDef.Runner.
type Runner interface {
	Run(ctx context.Context, step StepDef, prompt, dir string) (AgentResult, error)
}

var ErrUnknownRunner = errors.New("unknown runner")

// Runners maps StepDef.Runner names to implementations.
type Runners map[string]Runner

func (r Runners) Run(ctx context.Context, step StepDef, prompt, dir string) (AgentResult, error) {
	impl, ok := r[step.Runner]
	if !ok {
		return AgentResult{}, fmt.Errorf("step %s: %w %q", step.Name, ErrUnknownRunner, step.Runner)
	}
	res, err := impl.Run(ctx, step, prompt, dir)
	if err != nil {
		return AgentResult{}, fmt.Errorf("runner %s: %w", step.Runner, err)
	}
	return res, nil
}
