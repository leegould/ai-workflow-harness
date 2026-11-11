package harness

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"aiharness/agent"
)

func TestWorkflowShowsEachAgentStepsModel(t *testing.T) {
	h := newHarness(t)
	wf, err := h.e.Workflow()
	if err != nil {
		t.Fatal(err)
	}
	var agents []string
	for _, s := range wf.Steps {
		if s.Agent != nil {
			agents = append(agents, s.Agent.Step)
			if s.Agent.Model == "" {
				t.Fatalf("%s has no model", s.ID)
			}
		}
	}
	if len(wf.Steps) != 9 || len(agents) != 3 || wf.Retro.Agent == nil || len(wf.Models) == 0 {
		t.Fatalf("got %d steps, agents %v, models %v", len(wf.Steps), agents, wf.Models)
	}
}

func TestSetModel(t *testing.T) {
	h := newHarness(t)
	h.e.Runner = agent.Runners{"claude-cli": fakeAgent{}, "cursor-cli": fakeCursor{}}
	if err := h.e.SetModel("build", "claude-cli", "claude-haiku-5-5"); err != nil {
		t.Fatal(err)
	}
	if s, err := agent.LoadStep(h.e.StepsDir, "build"); err != nil || s.Model != "claude-haiku-5-5" {
		t.Fatalf("got %q %v", s.Model, err)
	}
	// Tickets keep running: the step reads its file on every run.
	expectDeployed(t, h.drain(h.create("Add a dark theme")))
	for name, args := range map[string][3]string{
		"model not in pipeline.yaml":  {"build", "claude-cli", "gpt-9"},
		"model from another runner":   {"build", "claude-cli", "composer-2.5"},
		"runner that can't run build": {"build", "cursor-cli", "composer-2.5"},
		"not an agent step":           {"ci", "claude-cli", "claude-haiku-5-5"},
		"outside the steps dir":       {"../pipeline", "claude-cli", "claude-haiku-5-5"},
	} {
		if err := h.e.SetModel(args[0], args[1], args[2]); !errors.Is(err, ErrBadModel) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
	// A read-only step can move to Cursor, and its runs say so on the timeline.
	if err := h.e.SetModel("spec", "cursor-cli", "composer-2.5"); err != nil {
		t.Fatal(err)
	}
	id := h.create("Add a dark theme")
	expectDeployed(t, h.drain(id))
	evs, _ := h.e.Store.Events(context.Background(), id)
	var by []string
	for _, ev := range evs {
		if ev.By != "" {
			by = append(by, string(ev.Step)+"="+ev.By)
		}
	}
	if !slices.Contains(by, "spec=cursor-cli/composer-2.5") || !slices.Contains(by, "build=claude-cli/claude-haiku-5-5") {
		t.Fatalf("got %v", by)
	}
	wf, _ := h.e.Workflow()
	for _, s := range wf.Steps {
		if s.ID == StepBuild && slices.Contains(s.Agent.Runners, "cursor-cli") {
			t.Fatal("cursor-cli offered for build")
		}
	}
}

// fakeCursor runs like fakeAgent but refuses what the real CursorCLI refuses.
type fakeCursor struct{ fakeAgent }

func (fakeCursor) Check(step agent.StepDef) error { return agent.CursorCLI{}.Check(step) }

func TestWorkflowAPI(t *testing.T) {
	srv := Server(newHarness(t).e, "")
	if code, out := call(t, srv, "GET", "/api/workflow", ""); code != http.StatusOK || out["steps"] == nil {
		t.Fatalf("workflow: %d %v", code, out)
	}
	if code, _ := call(t, srv, "POST", "/api/workflow/steps/spec/model", `{"runner":"claude-cli","model":"claude-haiku-5-5"}`); code != http.StatusOK {
		t.Fatalf("set model: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/workflow/steps/spec/model", `{"model":"gpt-9"}`); code != http.StatusBadRequest {
		t.Fatalf("bad model: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/workflow/steps/spec/model", `nope`); code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
}
