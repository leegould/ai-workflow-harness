package harness

import (
	"errors"
	"fmt"
	"maps"
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

type Workflow struct {
	Steps  []WorkflowStep      `json:"steps"`
	Retro  WorkflowStep        `json:"retro"`
	Models map[string][]string `json:"models"`
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
	steps := []WorkflowStep{
		{ID: StepReadiness, Label: "Readiness", Kind: "agent",
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
		{ID: StepDeploy, Label: "Deploy", Kind: "ship", Summary: "Builds and deploys main."},
		{ID: StepSmoke, Label: "Smoke test", Kind: "check",
			Summary: "Runs the deployed version.",
			Loops:   []string{"Fail: reverts main, redeploys, and escalates to you"}},
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
	return Workflow{Steps: steps, Retro: retro, Models: e.Config.Models}, nil
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
var agentSteps = []string{"readiness", "spec", "build", "retro"}

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
