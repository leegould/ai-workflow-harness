package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aiharness/agent"
	"aiharness/target"
)

// fakeAgent stands in for the agents. Readiness: tickets marked THIN aren't
// ready until the prompt carries answers. Build: always reports a change. It
// renders the real step files, so a broken template fails these tests.
type fakeAgent struct{ fail error }

func (f fakeAgent) Run(_ context.Context, step agent.StepDef, prompt, _ string) (agent.AgentResult, error) {
	if f.fail != nil {
		return agent.AgentResult{}, f.fail
	}
	var v any = map[string]any{"summary": "did the thing", "assumptions": []string{"left poster.json as it was"}}
	if step.Name == "retro" {
		v = fakeRetro(prompt)
	}
	if step.Name == "readiness" {
		rd := readiness{Ready: true, Reason: "clear"}
		if strings.Contains(prompt, "THIN") && !strings.Contains(prompt, "Answers:") {
			rd = readiness{Reason: "no acceptance criteria", Questions: []string{"What should it return?"}}
		}
		v = rd
	}
	out, err := json.Marshal(v)
	return agent.AgentResult{Output: out, CostUSD: 0.01}, err
}

// fakeRepo stands in for git, CI and deploy. ci and smoke decide each run's
// result; changed is what the risk gate sees.
type fakeRepo struct {
	n        int
	branches []string
	reverted []string
	ci       func(n int) bool
	cis      int
	smoke    func(n int) bool
	smokes   int
	changed  []target.FileChange
	broken   error // returned by Checkout, to simulate infrastructure failure
}

func (f *fakeRepo) sha() string { f.n++; return fmt.Sprintf("sha%07d", f.n) }

func (f *fakeRepo) Checkout(_ context.Context, _, branch string) (string, error) {
	if f.broken != nil {
		return "", f.broken
	}
	if branch != "main" {
		f.branches = append(f.branches, branch)
	}
	return "", nil
}
func (f *fakeRepo) CommitAndPush(context.Context, string, string, string) (string, error) {
	return f.sha(), nil
}
func (f *fakeRepo) HeadSHA(context.Context, string) (string, error)       { return f.sha(), nil }
func (f *fakeRepo) Merge(context.Context, string, string) (string, error) { return f.sha(), nil }
func (f *fakeRepo) Deploy(context.Context) (string, error)                { return f.sha(), nil }
func (f *fakeRepo) Revert(_ context.Context, sha string) (string, error) {
	f.reverted = append(f.reverted, sha)
	return f.sha(), nil
}
func (f *fakeRepo) ChangedFiles(context.Context, string) ([]target.FileChange, error) {
	return f.changed, nil
}

func (f *fakeRepo) RunCI(_ context.Context, sha string) (target.CIResult, error) {
	f.cis++
	if f.ci != nil && !f.ci(f.cis) {
		return target.CIResult{SHA: sha, Log: "--- FAIL: TestRender"}, nil
	}
	return target.CIResult{SHA: sha, Passed: true}, nil
}

func (f *fakeRepo) Smoke(context.Context) (target.SmokeResult, error) {
	f.smokes++
	if f.smoke != nil && !f.smoke(f.smokes) {
		return target.SmokeResult{Log: "poster exited: exit status 2"}, nil
	}
	return target.SmokeResult{Passed: true}, nil
}

type harness struct {
	t     *testing.T
	e     *Engine
	repo  *fakeRepo
	clock time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg, err := LoadConfig("../pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// A copy, because applying a retro proposal really edits the step files.
	steps := t.TempDir()
	if err := os.CopyFS(steps, os.DirFS("../steps")); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, repo: &fakeRepo{}, clock: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	h.e = &Engine{Store: store, Repo: h.repo, Runner: fakeAgent{}, StepsDir: steps, Config: cfg,
		Now: func() time.Time { return h.clock }}
	return h
}

func (h *harness) create(desc string) string {
	h.t.Helper()
	tk, err := h.e.Create(context.Background(), "Test ticket", desc)
	if err != nil {
		h.t.Fatal(err)
	}
	return tk.ID
}

// drain runs steps until nothing is due, then returns the ticket.
func (h *harness) drain(id string) Ticket {
	h.t.Helper()
	for range 100 {
		ran, err := h.e.RunOnce(context.Background())
		if err != nil {
			h.t.Fatal(err)
		}
		if !ran {
			break
		}
	}
	tk, err := h.e.Store.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return tk
}

func (h *harness) act(id string, a Action, comment string) {
	h.t.Helper()
	if _, err := h.e.Act(context.Background(), id, a, comment); err != nil {
		h.t.Fatal(err)
	}
}

func expectWaiting(t *testing.T, tk Ticket, w Wait) {
	t.Helper()
	if tk.Status != Waiting || tk.Wait != w {
		t.Fatalf("want waiting %s, got %s/%s (%s)", w, tk.Status, tk.Wait, tk.WaitText)
	}
}

func expectDeployed(t *testing.T, tk Ticket) {
	t.Helper()
	if tk.Status != Done || tk.Outcome != "deployed" || tk.Deployed == "" {
		t.Fatalf("want deployed, got %+v", tk)
	}
}

func TestHappyPathDeploys(t *testing.T) {
	h := newHarness(t)
	id := h.create("Add a dark theme")
	tk := h.drain(id)
	expectDeployed(t, tk)
	if tk.Builds != 1 || tk.Risk != "low" || tk.CostUSD <= 0 {
		t.Fatalf("got %+v", tk)
	}
	evs, err := h.e.Store.Events(context.Background(), id)
	if err != nil || len(evs) < 8 {
		t.Fatalf("expected a full timeline, got %d events (%v)", len(evs), err)
	}
}

func TestThinTicketWaitsForAnswers(t *testing.T) {
	h := newHarness(t)
	id := h.create("make it THIN")
	expectWaiting(t, h.drain(id), WaitQuestions)
	h.act(id, ActionAnswer, "a blue background")
	expectDeployed(t, h.drain(id))
}

func TestCIFailuresGoBackToTheBuilderThenEscalate(t *testing.T) {
	h := newHarness(t)
	h.repo.ci = func(int) bool { return false }
	id := h.create("Add a dark theme")
	tk := h.drain(id)
	expectWaiting(t, tk, WaitEscalated)
	if tk.Builds != 3 || !strings.Contains(tk.Feedback, "FAIL: TestRender") {
		t.Fatalf("got %+v", tk)
	}
	h.act(id, ActionAbandon, "")
	if tk := h.drain(id); tk.Status != Abandoned {
		t.Fatalf("got %s", tk.Status)
	}
}

func TestRetryWithGuidanceResetsRounds(t *testing.T) {
	h := newHarness(t)
	h.repo.ci = func(n int) bool { return n > 3 }
	id := h.create("Add a dark theme")
	expectWaiting(t, h.drain(id), WaitEscalated)
	h.act(id, ActionRetry, "use the existing theme map")
	tk := h.drain(id)
	expectDeployed(t, tk)
	if tk.Builds != 4 {
		t.Fatalf("builds %d", tk.Builds)
	}
}

// The risk floor is code reading pipeline.yaml: touching the agent's own
// rules always needs a human, whatever any agent thinks.
func TestRiskyPathsWaitForApproval(t *testing.T) {
	h := newHarness(t)
	h.repo.changed = []target.FileChange{{Path: "poster.go", Added: 5}, {Path: "CLAUDE.md", Added: 1}}
	id := h.create("Add a dark theme")
	tk := h.drain(id)
	expectWaiting(t, tk, WaitApproval)
	if tk.Risk != "high" || !strings.Contains(tk.WaitText, "CLAUDE.md") {
		t.Fatalf("got %+v", tk)
	}
	h.act(id, ActionApprove, "")
	expectDeployed(t, h.drain(id))
}

func TestActionsThatDontFitAreRejected(t *testing.T) {
	h := newHarness(t)
	h.repo.ci = func(int) bool { return false }
	id := h.create("Add a dark theme")
	expectWaiting(t, h.drain(id), WaitEscalated)
	if _, err := h.e.Act(context.Background(), id, ActionApprove, ""); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("approve while escalated: got %v", err)
	}
	h.act(id, ActionAbandon, "")
	if _, err := h.e.Act(context.Background(), id, ActionRetry, ""); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("retry after abandon: got %v", err)
	}
}

func TestSmokeFailureRevertsThenRetriesOnAFreshBranch(t *testing.T) {
	h := newHarness(t)
	h.repo.smoke = func(n int) bool { return n != 1 }
	id := h.create("Add a dark theme")
	tk := h.drain(id)
	expectWaiting(t, tk, WaitEscalated)
	if len(h.repo.reverted) != 1 || !strings.Contains(tk.Feedback, "reverted") {
		t.Fatalf("got %+v, reverted %v", tk, h.repo.reverted)
	}
	h.act(id, ActionRetry, "")
	expectDeployed(t, h.drain(id))
	if want := []string{"ai/T-1/1", "ai/T-1/2"}; !slices.Equal(h.repo.branches, want) {
		t.Fatalf("branches %v, want %v", h.repo.branches, want)
	}
}

func TestTakeOverThenResumeGoesBackThroughCI(t *testing.T) {
	h := newHarness(t)
	h.repo.ci = func(n int) bool { return n > 3 }
	id := h.create("Add a dark theme")
	expectWaiting(t, h.drain(id), WaitEscalated)
	h.act(id, ActionTakeOver, "")
	expectWaiting(t, h.drain(id), WaitTakeover)
	h.act(id, ActionResume, "")
	tk := h.drain(id)
	expectDeployed(t, tk)
	if tk.Builds != 3 || h.repo.cis != 4 {
		t.Fatalf("builds %d, CI runs %d", tk.Builds, h.repo.cis)
	}
}

// Infrastructure failures retry with backoff, then wait for a human. They
// never use a build round or reach the builder as feedback.
func TestInfrastructureFailureRetriesThenWaits(t *testing.T) {
	h := newHarness(t)
	h.repo.broken = errors.New("disk full")
	id := h.create("Add a dark theme")
	for range MaxFailures {
		h.drain(id)
		h.clock = h.clock.Add(time.Minute)
	}
	tk := h.drain(id)
	expectWaiting(t, tk, WaitError)
	if tk.Round != 0 || tk.Feedback != "" || !strings.Contains(tk.WaitText, "disk full") {
		t.Fatalf("got %+v", tk)
	}
	h.repo.broken = nil
	h.act(id, ActionRetry, "")
	expectDeployed(t, h.drain(id))
}

// A ticket left running by a crash is picked up again from its last step.
func TestRecoverRequeuesInterruptedTickets(t *testing.T) {
	h := newHarness(t)
	id := h.create("Add a dark theme")
	ctx := context.Background()
	if _, ok, err := h.e.Store.Claim(ctx, h.clock); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if n, err := h.e.Store.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("recovered %d, %v", n, err)
	}
	expectDeployed(t, h.drain(id))
}

func TestAgentErrorIsAnInfrastructureFailure(t *testing.T) {
	h := newHarness(t)
	h.e.Runner = fakeAgent{fail: errors.New("not logged in")}
	id := h.create("Add a dark theme")
	tk := h.drain(id)
	if tk.Failures != 1 || tk.Status != Queued || tk.NotBefore.IsZero() {
		t.Fatalf("got %+v", tk)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	h := newHarness(t)
	id := h.create("Add a dark theme")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- h.e.Run(ctx, 2) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tk, _ := h.e.Store.Get(context.Background(), id)
		if tk.Status == Done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	tk, _ := h.e.Store.Get(context.Background(), id)
	expectDeployed(t, tk)
}

// fakeRetro proposes nothing, unless the ticket says PROPOSE: then two valid
// edits to the same sentence, one to a step it may not edit, and one whose
// text isn't in the prompt.
func fakeRetro(prompt string) any {
	type proposal struct{ Step, Find, Replace, Why string }
	out := struct {
		Notes     string     `json:"notes"`
		Proposals []proposal `json:"proposals"`
	}{Notes: "went fine", Proposals: []proposal{}}
	if strings.Contains(prompt, "what the human wanted? no") {
		out.Proposals = []proposal{{"readiness", "always matters: ask", "always matters: ask, and never switch it on silently", "the human said it didn't do what they wanted"}}
	}
	if strings.Contains(prompt, "PROPOSE") {
		out.Proposals = []proposal{
			{"readiness", "ask at most 3 questions", "ask at most 1 question", "only one was answered"},
			{"readiness", "ask at most 3 questions", "ask at most 2 questions", "a gentler version"},
			{"retro", "Most tickets need no change.", "Always propose.", "self-edit"},
			{"build", "no such sentence", "x", "made up"},
		}
	}
	return out
}

func TestRetroProposesAndAHumanApplies(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.create("Add a dark theme PROPOSE")
	expectDeployed(t, h.drain(id))
	if ran, err := h.e.RetroOnce(ctx); err != nil || !ran {
		t.Fatalf("retro: %v %v", ran, err)
	}
	if ran, _ := h.e.RetroOnce(ctx); ran {
		t.Fatal("retro ran twice")
	}
	ps, err := h.e.Store.Proposals(ctx)
	if err != nil || len(ps) != 2 {
		t.Fatalf("want 2 proposals kept (2 discarded), got %d %v", len(ps), err)
	}
	stats, err := h.e.Stats(ctx)
	if err != nil || stats.OpenProposals != 2 || stats.Finished != 1 {
		t.Fatalf("stats %+v %v", stats, err)
	}

	// Apply the first; the second edits the same sentence, so it's now stale.
	first, second := ps[1], ps[0] // newest first
	if _, err := h.e.Decide(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(h.e.StepsDir, "readiness.md")) //nolint:gosec // test copy
	if !strings.Contains(string(body), "ask at most 1 question") {
		t.Fatal("proposal not applied to the step file")
	}
	if _, err := h.e.Decide(ctx, second.ID, true); !errors.Is(err, ErrProposalClosed) {
		t.Fatalf("stale proposal: got %v", err)
	}
	if _, err := h.e.Decide(ctx, first.ID, false); !errors.Is(err, ErrProposalClosed) {
		t.Fatalf("deciding twice: got %v", err)
	}
	evs, _ := h.e.Store.Events(ctx, id)
	discarded := 0
	for _, ev := range evs {
		if strings.Contains(ev.Message, "discarded") {
			discarded++
		}
	}
	if discarded != 2 {
		t.Fatalf("want 2 discarded proposals on the timeline, got %d", discarded)
	}
}

func TestAbandonedTicketsGetARetroToo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.repo.ci = func(int) bool { return false }
	id := h.create("Add a dark theme")
	expectWaiting(t, h.drain(id), WaitEscalated)
	h.act(id, ActionAbandon, "")
	if ran, err := h.e.RetroOnce(ctx); err != nil || !ran {
		t.Fatalf("retro: %v %v", ran, err)
	}
	tk, _ := h.e.Store.Get(ctx, id)
	if tk.Retro != RetroDone {
		t.Fatalf("retro %q", tk.Retro)
	}
}

// A failing retro is noted and never changes the ticket's outcome.
func TestRetroFailureLeavesTheOutcomeAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.create("Add a dark theme")
	expectDeployed(t, h.drain(id))
	h.e.Runner = fakeAgent{fail: errors.New("rate limited")}
	if _, err := h.e.RetroOnce(ctx); err != nil {
		t.Fatal(err)
	}
	tk, _ := h.e.Store.Get(ctx, id)
	if tk.Retro != RetroFailed || tk.Outcome != "deployed" {
		t.Fatalf("got retro %q outcome %q", tk.Retro, tk.Outcome)
	}
}

func TestRecoverRequeuesAnInterruptedRetro(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.create("Add a dark theme")
	expectDeployed(t, h.drain(id))
	if _, ok, err := h.e.Store.ClaimRetro(ctx); err != nil || !ok {
		t.Fatalf("claim retro: %v %v", ok, err)
	}
	if _, err := h.e.Store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if ran, err := h.e.RetroOnce(ctx); err != nil || !ran {
		t.Fatalf("retro after recover: %v %v", ran, err)
	}
}

func TestThinTicketCountsQuestions(t *testing.T) {
	h := newHarness(t)
	id := h.create("make it THIN")
	h.drain(id)
	h.act(id, ActionAnswer, "blue")
	if tk := h.drain(id); tk.Questions != 1 {
		t.Fatalf("questions %d", tk.Questions)
	}
}

// Two workers must never run the same retro (this happened live before
// claiming was a transaction).
func TestRetroClaimedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.create("Add a dark theme")
	expectDeployed(t, h.drain(id))
	ran := make(chan bool, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ok, err := h.e.RetroOnce(ctx)
			if err != nil {
				t.Error(err)
			}
			ran <- ok
		})
	}
	wg.Wait()
	close(ran)
	n := 0
	for ok := range ran {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("retro ran %d times", n)
	}
}

func TestAssumptionsAreRecordedPerStep(t *testing.T) {
	h := newHarness(t)
	h.repo.ci = func(n int) bool { return n > 1 }
	tk := h.drain(h.create("Add a blue theme"))
	expectDeployed(t, tk)
	// Two build rounds, but the build's assumptions are restated, not doubled.
	if tk.Builds != 2 || len(tk.Assumptions) != 1 || tk.Assumptions[0].Step != StepBuild {
		t.Fatalf("got builds %d, assumptions %+v", tk.Builds, tk.Assumptions)
	}
}

// "It didn't do what I wanted" reruns the retro with that verdict as evidence.
func TestNegativeFeedbackDrivesARetroProposal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.create("Add a blue theme")
	expectDeployed(t, h.drain(id))
	if ran, err := h.e.RetroOnce(ctx); err != nil || !ran {
		t.Fatalf("first retro: %v %v", ran, err)
	}
	if ps, _ := h.e.Store.Proposals(ctx); len(ps) != 0 {
		t.Fatalf("no verdict yet, want no proposals, got %d", len(ps))
	}
	if _, err := h.e.Feedback(ctx, id, false, "it added the theme but didn't switch to it"); err != nil {
		t.Fatal(err)
	}
	if ran, err := h.e.RetroOnce(ctx); err != nil || !ran {
		t.Fatalf("retro after feedback: %v %v", ran, err)
	}
	ps, err := h.e.Store.Proposals(ctx)
	if err != nil || len(ps) != 1 || !strings.Contains(ps[0].Why, "didn't do what they wanted") {
		t.Fatalf("got %+v %v", ps, err)
	}
	stats, _ := h.e.Stats(ctx)
	if stats.Rated != 1 || stats.Wanted != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestFeedbackOnlyOnFinishedTicketsWithNoRetroRunning(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.create("make it THIN")
	h.drain(id)
	if _, err := h.e.Feedback(ctx, id, true, ""); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("unfinished ticket: got %v", err)
	}
	h.act(id, ActionAnswer, "blue")
	expectDeployed(t, h.drain(id))
	if _, ok, err := h.e.Store.ClaimRetro(ctx); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, err := h.e.Feedback(ctx, id, true, ""); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("retro running: got %v", err)
	}
	if _, err := h.e.Feedback(ctx, "T-99", true, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ticket: got %v", err)
	}
}
