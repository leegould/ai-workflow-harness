package harness

import (
	"fmt"
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
	StepReadiness Step = "readiness"
	StepBuild     Step = "build"
	StepCI        Step = "ci"
	StepReview    Step = "review"
	StepRisk      Step = "risk"
	StepMerge     Step = "merge"
	StepDeploy    Step = "deploy"
	StepSmoke     Step = "smoke"
	// StepResume picks up a branch a human worked on, then goes to CI.
	StepResume Step = "resume"
)

// Wait is why a ticket needs a human, which decides what they can do about it.
type Wait string

const (
	WaitQuestions Wait = "questions" // readiness needs more detail
	WaitEscalated Wait = "escalated" // the loop is stuck, or a deploy was reverted
	WaitApproval  Wait = "approval"  // the risk gate
	WaitTakeover  Wait = "takeover"  // a human is working on the branch
	WaitError     Wait = "error"     // a step kept failing for infrastructure reasons
)

type Action string

const (
	ActionAnswer   Action = "answer"
	ActionApprove  Action = "approve"
	ActionRetry    Action = "retry"
	ActionTakeOver Action = "takeover"
	ActionResume   Action = "resume"
	ActionAbandon  Action = "abandon"
)

// actions lists what a human can do for each kind of wait. Anything else is
// rejected with a reason rather than silently ignored.
var actions = map[Wait][]Action{
	WaitQuestions: {ActionAnswer, ActionAbandon},
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

	Questions int    `json:"questions"`       // questions readiness asked, in total
	Retro     string `json:"retro,omitempty"` // pending once finished; see RetroOnce

	NotBefore time.Time `json:"notBefore"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
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
}
