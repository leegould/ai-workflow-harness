package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"aiharness/agent"
	"aiharness/target"
)

// MaxFailures is how many times a step may fail for infrastructure reasons
// (git, the agent CLI, the disk) before the ticket waits for a human. These
// never use up a build round or go to the builder as feedback.
const MaxFailures = 3

var ErrInvalidAction = errors.New("invalid action")

// Repo is everything the engine needs from the code host, CI and deploy.
// target.Local implements it locally; a GitHub-backed one could replace it.
type Repo interface {
	Checkout(ctx context.Context, key, branch string) (dir string, err error)
	CommitAndPush(ctx context.Context, dir, branch, message string) (sha string, err error)
	HeadSHA(ctx context.Context, branch string) (string, error)
	RunCI(ctx context.Context, sha string) (target.CIResult, error)
	ChangedFiles(ctx context.Context, branch string) ([]target.FileChange, error)
	Merge(ctx context.Context, branch, message string) (sha string, err error)
	Revert(ctx context.Context, mergeSHA string) (sha string, err error)
	Deploy(ctx context.Context) (sha string, err error)
	Smoke(ctx context.Context) (target.SmokeResult, error)
}

// Engine moves tickets through their steps. Each call to process runs one
// step, then saves the ticket, so a crash loses at most the step in progress.
type Engine struct {
	Store    *Store
	Repo     Repo
	Runner   agent.Runner
	StepsDir string
	Config   Config
	Now      func() time.Time

	wakeOnce sync.Once
	wake     chan struct{}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) wakeCh() chan struct{} {
	e.wakeOnce.Do(func() { e.wake = make(chan struct{}, 1) })
	return e.wake
}

// Wake tells idle workers there may be work, rather than making them wait for
// the next poll.
func (e *Engine) Wake() {
	select {
	case e.wakeCh() <- struct{}{}:
	default:
	}
}

// Run recovers interrupted tickets, then runs workers until ctx ends.
func (e *Engine) Run(ctx context.Context, workers int) error {
	n, err := e.Store.Recover(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("recovered %d interrupted tickets", n)
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { e.work(ctx) })
	}
	wg.Wait()
	return nil
}

func (e *Engine) work(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		ran, err := e.RunOnce(ctx)
		if err == nil && !ran {
			// Retros only run when no ticket is waiting for a step.
			ran, err = e.RetroOnce(ctx)
		}
		if err != nil {
			log.Printf("engine: %v", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-e.wakeCh():
		case <-tick.C:
		}
	}
}

// RunOnce claims one due ticket and runs its current step. It reports whether
// there was anything to do.
func (e *Engine) RunOnce(ctx context.Context) (bool, error) {
	t, ok, err := e.Store.Claim(ctx, e.now())
	if err != nil || !ok {
		return false, err
	}
	return true, e.process(ctx, t)
}

func (e *Engine) process(ctx context.Context, t Ticket) error {
	if err := e.step(ctx, &t); err != nil {
		e.failed(ctx, &t, err)
	} else {
		t.Failures = 0
	}
	t.Updated = e.now()
	return e.Store.Save(ctx, t)
}

// Create adds a ticket and queues its readiness check.
func (e *Engine) Create(ctx context.Context, title, description string) (Ticket, error) {
	now := e.now()
	t, err := e.Store.Create(ctx, Ticket{
		Title: title, Description: description,
		Status: Queued, Step: StepReadiness, Attempt: 1,
		Created: now, Updated: now,
	})
	if err != nil {
		return Ticket{}, err
	}
	e.event(ctx, &t, "info", "Ticket created", "")
	e.Wake()
	return t, nil
}

func (e *Engine) step(ctx context.Context, t *Ticket) error {
	switch t.Step {
	case StepReadiness:
		return e.readiness(ctx, t)
	case StepBuild:
		return e.build(ctx, t)
	case StepCI:
		return e.ci(ctx, t)
	case StepReview:
		return e.review(ctx, t)
	case StepRisk:
		return e.risk(ctx, t)
	case StepMerge:
		return e.merge(ctx, t)
	case StepDeploy:
		return e.deploy(ctx, t)
	case StepSmoke:
		return e.smoke(ctx, t)
	case StepResume:
		return e.resume(ctx, t)
	}
	return fmt.Errorf("unknown step %q", t.Step)
}

// next queues the ticket for another step.
func (e *Engine) next(t *Ticket, s Step) {
	t.Step = s
	t.Status = Queued
	t.NotBefore = time.Time{}
}

// wait parks the ticket until a human acts.
func (e *Engine) wait(ctx context.Context, t *Ticket, w Wait, text string) {
	t.Status = Waiting
	t.Wait = w
	t.WaitText = text
	e.event(ctx, t, "gate", "Waiting for a human: "+string(w), text)
}

// failed handles an error from a step: retry with backoff, then hand it to a
// human. It never touches the build round or the builder's feedback.
func (e *Engine) failed(ctx context.Context, t *Ticket, err error) {
	t.Failures++
	e.event(ctx, t, "error", fmt.Sprintf("Step failed (attempt %d of %d)", t.Failures, MaxFailures), err.Error())
	if t.Failures < MaxFailures {
		t.Status = Queued
		t.NotBefore = e.now().Add(time.Duration(t.Failures) * 5 * time.Second)
		return
	}
	e.wait(ctx, t, WaitError, fmt.Sprintf("The %s step failed %d times for reasons outside the code:\n%v", t.Step, t.Failures, err))
}

func (e *Engine) event(ctx context.Context, t *Ticket, kind, message, detail string) {
	e.costEvent(ctx, t, kind, message, detail, 0)
}

func (e *Engine) costEvent(ctx context.Context, t *Ticket, kind, message, detail string, cost float64) {
	ev := Event{Ticket: t.ID, At: e.now(), Step: t.Step, Kind: kind, Message: message, Detail: detail, CostUSD: cost}
	if err := e.Store.AddEvent(ctx, ev); err != nil {
		log.Printf("event for %s: %v", t.ID, err)
	}
}

// --- steps ---

type readiness struct {
	Ready       bool     `json:"ready"`
	Reason      string   `json:"reason"`
	Questions   []string `json:"questions"`
	Assumptions []string `json:"assumptions"`
}

// readiness judges the ticket against the code on main, read-only.
func (e *Engine) readiness(ctx context.Context, t *Ticket) error {
	dir, err := e.Repo.Checkout(ctx, t.ID+"-readiness", "main")
	if err != nil {
		return fmt.Errorf("check out: %w", err)
	}
	var rd readiness
	cost, err := e.runAgent(ctx, "readiness", t, dir, &rd)
	if err != nil {
		return err
	}
	t.CostUSD += cost
	t.Questions += len(rd.Questions)
	t.setAssumptions(StepReadiness, rd.Assumptions)
	if rd.Ready {
		e.costEvent(ctx, t, "agent", "Ready to build", rd.Reason, cost)
		e.next(t, StepBuild)
		return nil
	}
	e.costEvent(ctx, t, "agent", "Needs more detail", rd.Reason, cost)
	var qs strings.Builder
	for i, q := range rd.Questions {
		fmt.Fprintf(&qs, "%d. %s\n", i+1, q)
	}
	e.wait(ctx, t, WaitQuestions, strings.TrimSpace(qs.String()))
	return nil
}

// buildRequest is what the build step's prompt is rendered from.
type buildRequest struct {
	Ticket   Ticket
	Branch   string
	Feedback string
}

func (e *Engine) build(ctx context.Context, t *Ticket) error {
	if t.Round >= e.Config.MaxRounds {
		e.wait(ctx, t, WaitEscalated, fmt.Sprintf("No clean build after %d rounds. Last feedback:\n%s", e.Config.MaxRounds, t.Feedback))
		return nil
	}
	branch := t.branch()
	dir, err := e.Repo.Checkout(ctx, t.ID, branch)
	if err != nil {
		return fmt.Errorf("check out: %w", err)
	}
	var out struct {
		Summary     string   `json:"summary"`
		Assumptions []string `json:"assumptions"`
	}
	cost, err := e.runAgent(ctx, "build", buildRequest{Ticket: *t, Branch: branch, Feedback: t.Feedback}, dir, &out)
	if err != nil {
		return err
	}
	sha, err := e.Repo.CommitAndPush(ctx, dir, branch, fmt.Sprintf("%s: %s\n\n%s", t.ID, t.Title, out.Summary))
	if err != nil {
		return fmt.Errorf("commit and push: %w", err)
	}
	t.Round++
	t.Builds++
	t.Branch = branch
	t.SHA = sha
	t.CostUSD += cost
	t.setAssumptions(StepBuild, out.Assumptions)
	e.costEvent(ctx, t, "agent", fmt.Sprintf("Round %d pushed %s to %s", t.Round, short(sha), branch), out.Summary, cost)
	e.next(t, StepCI)
	return nil
}

// ci runs the checks itself: the harness always runs them, whatever the agent
// says it ran.
func (e *Engine) ci(ctx context.Context, t *Ticket) error {
	res, err := e.Repo.RunCI(ctx, t.SHA)
	if err != nil {
		return fmt.Errorf("run CI: %w", err)
	}
	if !res.Passed {
		e.event(ctx, t, "check", "CI failed on "+short(t.SHA)+", back to the builder", res.Log)
		t.Feedback = "CI failed:\n" + res.Log
		e.next(t, StepBuild)
		return nil
	}
	e.event(ctx, t, "check", "CI passed on "+short(t.SHA), res.Log)
	e.next(t, StepReview)
	return nil
}

// review is still a stub: "never-approve" in the ticket forces the loop.
func (e *Engine) review(ctx context.Context, t *Ticket) error {
	if strings.Contains(t.Description, "never-approve") {
		e.event(ctx, t, "agent", "Review (stub) asked for changes", "handle the error path")
		t.Feedback = "Review: handle the error path"
		e.next(t, StepBuild)
		return nil
	}
	e.event(ctx, t, "agent", "Review (stub) approved", "")
	e.next(t, StepRisk)
	return nil
}

// risk applies the deterministic floor from pipeline.yaml: paths and size.
func (e *Engine) risk(ctx context.Context, t *Ticket) error {
	changes, err := e.Repo.ChangedFiles(ctx, t.Branch)
	if err != nil {
		return fmt.Errorf("list changed files: %w", err)
	}
	level, why := e.Config.Risk.Assess(changes)
	t.Risk = level.String()
	var files strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&files, "%s (+%d -%d)\n", c.Path, c.Added, c.Deleted)
	}
	e.event(ctx, t, "gate", "Risk: "+level.String(), strings.Join(why, "\n")+"\n\n"+files.String())
	if level == Low {
		e.next(t, StepMerge)
		return nil
	}
	e.wait(ctx, t, WaitApproval, fmt.Sprintf("Risk is %s, so a human approves the merge:\n%s", level, strings.Join(why, "\n")))
	return nil
}

func (e *Engine) merge(ctx context.Context, t *Ticket) error {
	sha, err := e.Repo.Merge(ctx, t.Branch, fmt.Sprintf("Merge %s: %s", t.ID, t.Title))
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	t.Merged = sha
	e.event(ctx, t, "ship", "Merged "+t.Branch+" to main as "+short(sha), "")
	e.next(t, StepDeploy)
	return nil
}

func (e *Engine) deploy(ctx context.Context, t *Ticket) error {
	sha, err := e.Repo.Deploy(ctx)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	t.Deployed = sha
	e.event(ctx, t, "ship", "Deployed "+short(sha), "")
	e.next(t, StepSmoke)
	return nil
}

// smoke checks the deployed version. On failure it reverts first, then asks:
// main goes back to known-good before anyone is involved, and a retry starts
// a fresh branch.
func (e *Engine) smoke(ctx context.Context, t *Ticket) error {
	res, err := e.Repo.Smoke(ctx)
	if err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	if res.Passed {
		e.event(ctx, t, "check", "Smoke test passed", res.Log)
		t.Status = Done
		t.Outcome = "deployed"
		t.Retro = RetroPending
		return nil
	}
	reverted, err := e.Repo.Revert(ctx, t.Merged)
	if err != nil {
		return fmt.Errorf("revert: %w", err)
	}
	if t.Deployed, err = e.Repo.Deploy(ctx); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	e.event(ctx, t, "ship", fmt.Sprintf("Smoke test failed: reverted %s and redeployed %s", short(t.Merged), short(reverted)), res.Log)
	t.Attempt++
	t.Branch = ""
	t.Feedback = "Your last change was merged, failed the smoke test after deploy and was reverted:\n" + res.Log
	e.wait(ctx, t, WaitEscalated, "Smoke test failed after deploy. Main has been reverted to the last good version:\n"+res.Log)
	return nil
}

// resume picks up a branch a human has worked on. Their work goes through CI,
// review and the risk gate like the agent's.
func (e *Engine) resume(ctx context.Context, t *Ticket) error {
	branch := t.branch()
	sha, err := e.Repo.HeadSHA(ctx, branch)
	if err != nil {
		return fmt.Errorf("read branch head: %w", err)
	}
	t.Branch = branch
	t.SHA = sha
	t.Round = 0
	t.Feedback = ""
	e.event(ctx, t, "human", "Resumed at "+short(sha), "")
	e.next(t, StepCI)
	return nil
}

// Act applies a human's decision. Actions that don't fit the current wait are
// rejected with a reason.
func (e *Engine) Act(ctx context.Context, id string, a Action, comment string) (Ticket, error) {
	t, err := e.Store.Get(ctx, id)
	if err != nil {
		return Ticket{}, err
	}
	if !slices.Contains(t.Actions(), a) {
		if t.Status != Waiting {
			return t, fmt.Errorf("%w: %s isn't waiting on a human (it's %s)", ErrInvalidAction, t.ID, t.Status)
		}
		return t, fmt.Errorf("%w: can't %s while the ticket is %s", ErrInvalidAction, a, t.Wait)
	}
	comment = strings.TrimSpace(comment)
	msg := "You chose " + string(a)
	switch a {
	case ActionAnswer:
		if comment == "" {
			return t, fmt.Errorf("%w: an answer needs a comment", ErrInvalidAction)
		}
		t.Description += "\n\nAnswers:\n" + comment
		e.next(&t, StepReadiness)
	case ActionApprove:
		e.next(&t, StepMerge)
	case ActionRetry:
		if t.Wait == WaitError {
			e.next(&t, t.Step)
			break
		}
		if comment != "" {
			t.Feedback = strings.TrimSpace(t.Feedback + "\n\nHuman guidance:\n" + comment)
		}
		t.Round = 0
		e.next(&t, StepBuild)
	case ActionTakeOver:
		t.Wait = WaitTakeover
		t.WaitText = fmt.Sprintf("You've taken this over. Push your changes to %s, then resume. It'll go through CI, review and the risk gate like the agent's work.", t.branch())
	case ActionResume:
		e.next(&t, StepResume)
	case ActionAbandon:
		t.Status = Abandoned
		t.Outcome = "abandoned"
		t.Retro = RetroPending
	}
	if t.Status != Waiting {
		t.Wait, t.WaitText = "", ""
	}
	t.Failures = 0
	t.Updated = e.now()
	if err := e.Store.Save(ctx, t); err != nil {
		return Ticket{}, err
	}
	e.event(ctx, &t, "human", msg, comment)
	e.Wake()
	return t, nil
}

// runAgent renders a step file with data, runs it in dir and decodes its
// structured output into out. It returns the run's cost.
func (e *Engine) runAgent(ctx context.Context, name string, data any, dir string, out any) (float64, error) {
	step, err := agent.LoadStep(e.StepsDir, name)
	if err != nil {
		return 0, fmt.Errorf("load step: %w", err)
	}
	prompt, err := step.Render(data)
	if err != nil {
		return 0, fmt.Errorf("render step: %w", err)
	}
	res, err := e.Runner.Run(ctx, step, prompt, dir)
	if err != nil {
		return 0, fmt.Errorf("run %s agent: %w", name, err)
	}
	if err := json.Unmarshal(res.Output, out); err != nil {
		return res.CostUSD, fmt.Errorf("%s agent output: %w", name, err)
	}
	return res.CostUSD, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
