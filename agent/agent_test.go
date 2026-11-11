package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type ticket struct{ ID, Title, Description string }

type buildRequest struct {
	Ticket   ticket
	Branch   string
	Feedback string
	Spec     string
	SpecPath string
}

type specRequest struct {
	Ticket    ticket
	Previous  string
	Readiness []string
}

// The real step files must load and render; this stops a bad edit reaching a run.
func TestStepFilesLoad(t *testing.T) {
	tk := ticket{ID: "T-9", Title: "Health", Description: "Add /health"}
	tests := []struct {
		step string
		data any
		want []string
	}{
		{"readiness", tk, []string{"Ticket T-9: Health", "Add /health"}},
		{"build", buildRequest{Ticket: tk, Branch: "ai/T-9/1"}, []string{"Ticket T-9: Health", "branch ai/T-9/1"}},
		{"build", buildRequest{Ticket: tk, Feedback: "CI failed: vet"}, []string{"follow-up round", "CI failed: vet"}},
		{"build", buildRequest{Ticket: tk, Spec: "# T-9: Health", SpecPath: "specs/T-9.md"}, []string{"has a spec", "# T-9: Health", "as specs/T-9.md"}},
		{"spec", specRequest{Ticket: tk, Readiness: []string{"JSON, not text"}}, []string{"Ticket T-9: Health", "- JSON, not text"}},
		{"spec", specRequest{Ticket: tk, Previous: "# old spec"}, []string{"asked for changes", "# old spec"}},
	}
	for _, tc := range tests {
		t.Run(tc.step, func(t *testing.T) {
			s, err := LoadStep("../steps", tc.step)
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.Render(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(p, want) {
					t.Errorf("prompt missing %q:\n%s", want, p)
				}
			}
		})
	}
}

func TestLoadStepErrors(t *testing.T) {
	if _, err := LoadStep("../steps", "nope"); err == nil {
		t.Fatal("expected an error for a missing step")
	}
	if _, err := LoadStep("does-not-exist", "readiness"); err == nil {
		t.Fatal("expected an error for a missing dir")
	}
}

func TestUnknownRunner(t *testing.T) {
	_, err := Runners{}.Run(context.Background(), StepDef{Name: "x", Runner: "openrouter"}, "", "")
	if !errors.Is(err, ErrUnknownRunner) {
		t.Fatalf("got %v", err)
	}
}
