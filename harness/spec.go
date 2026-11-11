package harness

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"aiharness/target"
)

// specOutput is the spec agent's answer. The harness renders and commits the
// file itself, so the agent never needs write access.
type specOutput struct {
	Goal       string         `json:"goal"`
	InScope    []string       `json:"in_scope"`
	OutOfScope []string       `json:"out_of_scope"`
	Decisions  []specDecision `json:"decisions"`
	Files      []string       `json:"files"`
	DoneWhen   []string       `json:"done_when"`
}

type specDecision struct {
	Decision string `json:"decision"`
	Why      string `json:"why"`
}

// specRequest is what the spec step's prompt is rendered from.
type specRequest struct {
	Ticket     Ticket
	Previous   string   // the last spec, when the author asked for changes
	Refinement []string // the refinement step's assumptions
}

// specPath is where a ticket's spec is committed. One per change, never
// updated afterwards: a record, not a living document.
func specPath(id string) string { return "specs/" + id + ".md" }

// spec writes the spec from the code on main, read-only. If the files it plans
// to change are risky (or pipeline.yaml says always), a human approves it
// before anything is built.
func (e *Engine) spec(ctx context.Context, t *Ticket) error {
	dir, err := e.Repo.Checkout(ctx, t.ID+"-spec", "main")
	if err != nil {
		return fmt.Errorf("check out: %w", err)
	}
	req := specRequest{Ticket: *t, Previous: t.Spec}
	for _, a := range t.Assumptions {
		if a.Step == StepRefinement {
			req.Refinement = append(req.Refinement, a.Text)
		}
	}
	var out specOutput
	run, err := e.runAgent(ctx, "spec", req, dir, &out)
	if err != nil {
		return err
	}
	t.CostUSD += run.Cost
	t.Spec = out.markdown(t)
	t.SpecFiles = out.Files
	e.costEvent(ctx, t, "agent", fmt.Sprintf("Spec written: %d decisions, %d files planned", len(out.Decisions), len(out.Files)), t.Spec, run)

	planned := make([]target.FileChange, len(out.Files))
	for i, f := range out.Files {
		planned[i] = target.FileChange{Path: f}
	}
	level, why := e.Config.Risk.Assess(planned)
	switch {
	case e.Config.Spec.Approval == SpecAlways:
		e.wait(ctx, t, WaitSpec, "pipeline.yaml asks for every spec to be approved before anything is built.")
	case e.Config.Spec.Approval != SpecNever && level > Low:
		e.wait(ctx, t, WaitSpec, fmt.Sprintf("The files this plans to change make it %s risk, so check the spec before anything is built:\n%s", level, strings.Join(why, "\n")))
	default:
		e.next(t, StepBuild)
	}
	return nil
}

func (s specOutput) markdown(t *Ticket) string {
	var b strings.Builder
	list := func(heading string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", heading)
		for _, it := range items {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	}
	fmt.Fprintf(&b, "# %s: %s\n\n%s\n", t.ID, t.Title, s.Goal)
	list("In scope", s.InScope)
	list("Out of scope", s.OutOfScope)
	decisions := make([]string, len(s.Decisions))
	for i, d := range s.Decisions {
		decisions[i] = fmt.Sprintf("%s (%s)", d.Decision, d.Why)
	}
	list("Decisions", decisions)
	files := make([]string, len(s.Files))
	for i, f := range s.Files {
		files[i] = "`" + f + "`"
	}
	list("Files expected to change", files)
	list("Done when", s.DoneWhen)
	return b.String()
}

// writeSpec puts the ticket's spec in the checkout, so it's committed with the
// change. It runs after the build agent, so the committed spec is always the
// one that was written (and approved), whatever the agent did to the file.
func writeSpec(dir string, t *Ticket) error {
	if t.Spec == "" {
		return nil
	}
	file := filepath.Join(dir, filepath.FromSlash(specPath(t.ID)))
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return fmt.Errorf("write spec: %w", err)
	}
	if err := os.WriteFile(file, []byte(t.Spec), 0o600); err != nil {
		return fmt.Errorf("write spec: %w", err)
	}
	return nil
}

// unplanned lists the changed files the spec didn't plan. A planned entry
// ending in "/" covers everything under it.
func unplanned(planned []string, changes []target.FileChange) []string {
	if len(planned) == 0 {
		return nil
	}
	var out []string
	for _, c := range changes {
		ok := false
		for _, p := range planned {
			if c.Path == path.Clean(p) || strings.HasSuffix(p, "/") && strings.HasPrefix(c.Path, p) {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, c.Path)
		}
	}
	return out
}
