package harness

import (
	"errors"
	"net/http"
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
	if err := h.e.SetModel("build", "claude-haiku-5-5"); err != nil {
		t.Fatal(err)
	}
	if s, err := agent.LoadStep(h.e.StepsDir, "build"); err != nil || s.Model != "claude-haiku-5-5" {
		t.Fatalf("got %q %v", s.Model, err)
	}
	// Tickets keep running: the step reads its file on every run.
	expectDeployed(t, h.drain(h.create("Add a dark theme")))
	for name, args := range map[string][2]string{
		"model not in pipeline.yaml": {"build", "gpt-9"},
		"not an agent step":          {"ci", "claude-haiku-5-5"},
		"outside the steps dir":      {"../pipeline", "claude-haiku-5-5"},
	} {
		if err := h.e.SetModel(args[0], args[1]); !errors.Is(err, ErrBadModel) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestWorkflowAPI(t *testing.T) {
	srv := Server(newHarness(t).e, "")
	if code, out := call(t, srv, "GET", "/api/workflow", ""); code != http.StatusOK || out["steps"] == nil {
		t.Fatalf("workflow: %d %v", code, out)
	}
	if code, _ := call(t, srv, "POST", "/api/workflow/steps/spec/model", `{"model":"claude-haiku-5-5"}`); code != http.StatusOK {
		t.Fatalf("set model: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/workflow/steps/spec/model", `{"model":"gpt-9"}`); code != http.StatusBadRequest {
		t.Fatalf("bad model: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/workflow/steps/spec/model", `nope`); code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
}
