package harness

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	"aiharness/agent"
)

var ErrBadModel = errors.New("model change not allowed")

// WorkflowStep describes one step of the flow for the UI. The flow itself is
// the code in engine.go; this is its description, kept next to it in order.
type WorkflowStep struct {
	ID      Step       `json:"id"`
	Label   string     `json:"label"`
	Kind    string     `json:"kind"` // agent, check, gate, ship
	Summary string     `json:"summary"`
	Loops   []string   `json:"loops,omitempty"`
	Agent   *AgentInfo `json:"agent,omitempty"`
}

// AgentInfo is what an agent step runs with, read from its step file, and
// which runners it could be switched to.
type AgentInfo struct {
	Step      string   `json:"step"`
	Runner    string   `json:"runner"`
	Model     string   `json:"model"`
	Tools     []string `json:"tools"`
	BudgetUSD float64  `json:"budgetUSD"`
	Runners   []string `json:"runners"`
}

// SourceInfo is one place tickets come from.
type SourceInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	MaxOpen int    `json:"maxOpen,omitempty"`
	Signed  bool   `json:"signed"` // its signing secret is set, so unsigned reports are refused
}

type Workflow struct {
	Sources []SourceInfo        `json:"sources"`
	Steps   []WorkflowStep      `json:"steps"`
	Retro   WorkflowStep        `json:"retro"`
	Models  map[string][]string `json:"models"`
}

// Workflow describes the flow as it runs now: the steps in order, the gates
// with their current settings from pipeline.yaml, and the model each agent
// step uses.
func (e *Engine) Workflow() (Workflow, error) {
	approval := map[string]string{
		SpecRisky:  "if the files it plans to change are medium or high risk, you approve it first",
		SpecAlways: "you approve every spec before the build",
		SpecNever:  "nobody approves it first",
	}[e.Config.Spec.Approval]
	promote := map[string]string{
		ProductionManual: "Manual: you check it on dev and ship it",
		ProductionAuto:   "Auto: a change that passes on dev ships",
	}[e.Config.Production.Approval]
	steps := []WorkflowStep{
		{ID: StepRefinement, Label: "Refinement", Kind: "agent",
			Summary: "Reads the ticket and the code on main. Asks you only what the code can't answer.",
			Loops:   []string{"Not ready: waits for your answers, then checks again"}},
		{ID: StepSpec, Label: "Spec", Kind: "agent",
			Summary: "Writes the brief: scope, decisions and the files it plans to change. Committed with the change.",
			Loops:   []string{"Approval: " + approval}},
		{ID: StepBuild, Label: "Build", Kind: "agent",
			Summary: "Changes the code on the ticket's branch, working from the spec. The harness commits and pushes.",
			Loops:   []string{fmt.Sprintf("Up to %d rounds, then it escalates to you", e.Config.MaxRounds)}},
		{ID: StepCI, Label: "CI", Kind: "check",
			Summary: "The harness runs gofmt, vet and the tests itself, whatever the agent says it ran.",
			Loops:   []string{"Fail: back to Build with the log"}},
		{ID: StepReview, Label: "Review", Kind: "check",
			Summary: "A stub for now: approves unless the ticket says never-approve.",
			Loops:   []string{"Changes asked for: back to Build"}},
		{ID: StepRisk, Label: "Risk gate", Kind: "gate",
			Summary: "Paths and diff size from pipeline.yaml, plus any file the spec didn't plan.",
			Loops:   []string{"Low: merges by itself", "Medium or high: you approve the merge"}},
		{ID: StepMerge, Label: "Merge", Kind: "ship", Summary: "Merges the branch to main."},
		{ID: StepDeploy, Label: "Deploy to dev", Kind: "ship", Summary: "Every merge is built and deployed to dev."},
		{ID: StepSmoke, Label: "Smoke test", Kind: "check",
			Summary: "Runs the version on dev.",
			Loops:   []string{"Fail: reverts main, redeploys dev, and escalates to you"}},
		{ID: StepPromote, Label: "Promote", Kind: "gate",
			Summary: "The production gate.",
			Loops:   []string{promote, "Send back: reverts it from main and dev, then rebuilds with your guidance"}},
		{ID: StepRelease, Label: "Production", Kind: "ship",
			Summary: "Deploys the commit that passed on dev to production, and smoke tests it there.",
			Loops:   []string{"Fail: production goes back to what it ran before, main and dev are reverted, and it escalates to you"}},
	}
	retro := WorkflowStep{ID: "retro", Label: "Retro", Kind: "agent",
		Summary: "After each ticket: reads its timeline and your verdict, and may propose prompt edits.",
		Loops:   []string{"Proposals wait in Improvements until you apply them"}}
	for i := range steps {
		if steps[i].Kind == "agent" {
			info, err := e.agentInfo(string(steps[i].ID))
			if err != nil {
				return Workflow{}, err
			}
			steps[i].Agent = &info
		}
	}
	info, err := e.agentInfo("retro")
	if err != nil {
		return Workflow{}, err
	}
	retro.Agent = &info
	sources := []SourceInfo{{Name: "ui", Kind: SourcePlanned}}
	for _, name := range slices.Sorted(maps.Keys(e.Config.Sources)) {
		c := e.Config.Sources[name]
		sources = append(sources, SourceInfo{Name: name, Kind: c.Kind, Path: "/api/sources/" + name,
			MaxOpen: c.MaxOpen, Signed: c.SecretEnv != "" && os.Getenv(c.SecretEnv) != ""})
	}
	return Workflow{Sources: sources, Steps: steps, Retro: retro, Models: e.Config.Models}, nil
}

func (e *Engine) agentInfo(name string) (AgentInfo, error) {
	s, err := agent.LoadStep(e.StepsDir, name)
	if err != nil {
		return AgentInfo{}, fmt.Errorf("load step: %w", err)
	}
	info := AgentInfo{Step: name, Runner: s.Runner, Model: s.Model, Tools: s.Tools, BudgetUSD: s.MaxBudgetUSD}
	for _, runner := range slices.Sorted(maps.Keys(e.Config.Models)) {
		alt := s
		alt.Runner = runner
		if e.checkRunner(alt) == nil {
			info.Runners = append(info.Runners, runner)
		}
	}
	return info, nil
}

// checkRunner asks the runner whether it can run step, when it can say.
func (e *Engine) checkRunner(step agent.StepDef) error {
	if c, ok := e.Runner.(agent.Checker); ok {
		return c.Check(step) //nolint:wrapcheck // names the step and runner already
	}
	return nil
}

// agentSteps are the steps whose model can be chosen.
var agentSteps = []string{"refinement", "spec", "build", "retro"}

// SetModel changes the runner and model an agent step runs on, from the ones
// pipeline.yaml allows, and only to a runner that can run that step. It edits
// only the step file's runner and model lines, so it's a small diff in git.
// Tickets in flight use it from their next run of that step.
func (e *Engine) SetModel(step, runner, model string) error {
	if !slices.Contains(agentSteps, step) {
		return fmt.Errorf("%w: %s isn't an agent step", ErrBadModel, step)
	}
	if !slices.Contains(e.Config.Models[runner], model) {
		return fmt.Errorf("%w: %s/%s isn't in the models pipeline.yaml allows", ErrBadModel, runner, model)
	}
	def, err := agent.LoadStep(e.StepsDir, step)
	if err != nil {
		return fmt.Errorf("load step: %w", err)
	}
	def.Runner, def.Model = runner, model
	if err := e.checkRunner(def); err != nil {
		return fmt.Errorf("%w: %w", ErrBadModel, err)
	}
	if err := agent.SetRunnerModel(e.StepsDir, step, runner, model); err != nil {
		return fmt.Errorf("set model: %w", err)
	}
	return nil
}
