package harness

import (
	"fmt"
	"slices"
	"time"
)

type Status string

const (
	Queued    Status = "queued"  // ready for the engine to run its next step
	Running   Status = "running" // a worker is on it
	Waiting   Status = "waiting" // needs a human; see Wait
	Done      Status = "done"
	Abandoned Status = "abandoned"
)

type Step string

const (
	StepRefinement Step = "refinement"
	StepSpec       Step = "spec"
	StepBuild      Step = "build"
	StepCI         Step = "ci"
	StepReview     Step = "review"
	StepRisk       Step = "risk"
	StepMerge      Step = "merge"
	StepDeploy     Step = "deploy"
	StepSmoke      Step = "smoke"
	// StepResume picks up a branch a human worked on, then goes to CI.
	StepResume Step = "resume"
)

// Wait is why a ticket needs a human, which decides what they can do about it.
type Wait string

const (
	WaitQuestions Wait = "questions" // refinement needs more detail
	WaitSpec      Wait = "spec"      // a human approves the spec before the build
	WaitEscalated Wait = "escalated" // the loop is stuck, or a deploy was reverted
	WaitApproval  Wait = "approval"  // the risk gate
	WaitTakeover  Wait = "takeover"  // a human is working on the branch
	WaitError     Wait = "error"     // a step kept failing for infrastructure reasons
)

type Action string

const (
	ActionAnswer   Action = "answer"
	ActionApprove  Action = "approve"
	ActionRevise   Action = "revise"
	ActionRetry    Action = "retry"
	ActionTakeOver Action = "takeover"
	ActionResume   Action = "resume"
	ActionAbandon  Action = "abandon"
)

// actions lists what a human can do for each kind of wait. Anything else is
// rejected with a reason rather than silently ignored.
var actions = map[Wait][]Action{
	WaitQuestions: {ActionAnswer, ActionAbandon},
	WaitSpec:      {ActionApprove, ActionRevise, ActionAbandon},
	WaitEscalated: {ActionRetry, ActionTakeOver, ActionAbandon},
	WaitApproval:  {ActionApprove, ActionRetry, ActionTakeOver, ActionAbandon},
	WaitTakeover:  {ActionResume, ActionAbandon},
	WaitError:     {ActionRetry, ActionAbandon},
}

// Ticket is one unit of work and all the state the engine needs to carry it
// on after a restart. It's saved after every step.
type Ticket struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`

	Status   Status `json:"status"`
	Step     Step   `json:"step"`
	Wait     Wait   `json:"wait,omitempty"`
	WaitText string `json:"waitText,omitempty"`
	Outcome  string `json:"outcome,omitempty"`

	Round    int     `json:"round"`    // build rounds since the last human retry/resume; capped
	Builds   int     `json:"builds"`   // build agent runs in total
	Attempt  int     `json:"attempt"`  // which branch; a revert starts a new one
	Failures int     `json:"failures"` // consecutive infrastructure failures of this step
	Branch   string  `json:"branch,omitempty"`
	SHA      string  `json:"sha,omitempty"`
	Feedback string  `json:"feedback,omitempty"`
	Risk     string  `json:"risk,omitempty"`
	Merged   string  `json:"merged,omitempty"`
	Deployed string  `json:"deployed,omitempty"`
	CostUSD  float64 `json:"costUSD"`

	// Spec is the markdown committed as specs/<id>.md with the change.
	// SpecFiles is what it plans to change, checked against the real diff.
	Spec      string   `json:"spec,omitempty"`
	SpecFiles []string `json:"specFiles,omitempty"`

	Questions int    `json:"questions"`       // questions refinement asked, in total
	Retro     string `json:"retro,omitempty"` // pending once finished; see RetroOnce

	// Assumptions are calls the agents made for the author, shown on the
	// ticket rather than buried in a summary.
	Assumptions []Assumption `json:"assumptions,omitempty"`

	// Verdict is the human's answer to "did this do what you wanted?":
	// "yes", "no", or empty if they didn't say. The retro's best evidence.
	Verdict        string `json:"verdict,omitempty"`
	VerdictComment string `json:"verdictComment,omitempty"`

	// Where the ticket came from: "" for the New ticket button, otherwise a
	// source from pipeline.yaml. ExternalID identifies the problem at the
	// source, so a repeat report is recognised. Reactive is copied from the
	// source when the ticket is made, so editing the config later doesn't
	// change the rules for tickets already in flight.
	Source       string `json:"source,omitempty"`
	ExternalID   string `json:"externalID,omitempty"`
	ExternalURL  string `json:"externalURL,omitempty"`
	Reactive     bool   `json:"reactive,omitempty"`
	Occurrences  int    `json:"occurrences,omitempty"`  // read from its own column; see Store.CreateReported
	RegressionOf string `json:"regressionOf,omitempty"` // the ticket whose deployed fix didn't hold

	NotBefore time.Time `json:"notBefore"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

type Assumption struct {
	Step Step   `json:"step"`
	Text string `json:"text"`
}

// setAssumptions replaces the assumptions from one step: a new build round
// restates its own, so they don't pile up.
func (t *Ticket) setAssumptions(s Step, texts []string) {
	t.Assumptions = slices.DeleteFunc(t.Assumptions, func(a Assumption) bool { return a.Step == s })
	for _, text := range texts {
		t.Assumptions = append(t.Assumptions, Assumption{Step: s, Text: text})
	}
}

// Actions is what a human can do with the ticket right now.
func (t *Ticket) Actions() []Action {
	if t.Status != Waiting {
		return nil
	}
	return actions[t.Wait]
}

// branch is ai/<ticket>/<attempt>. A fresh attempt after a revert gets a fresh
// branch, because re-merging a reverted branch wouldn't bring its changes back.
func (t *Ticket) branch() string { return fmt.Sprintf("ai/%s/%d", t.ID, t.Attempt) }

// Event is one entry in a ticket's timeline: what happened, at which step,
// and anything worth expanding (an agent's summary, a CI log).
type Event struct {
	ID      int64     `json:"id"`
	Ticket  string    `json:"ticket"`
	At      time.Time `json:"at"`
	Step    Step      `json:"step"`
	Kind    string    `json:"kind"` // agent, check, gate, human, ship, error, info
	Message string    `json:"message"`
	Detail  string    `json:"detail,omitempty"`
	CostUSD float64   `json:"costUSD,omitempty"`
	By      string    `json:"by,omitempty"` // for agent runs: runner/model
}
