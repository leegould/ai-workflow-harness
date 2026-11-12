package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"aiharness/agent"
)

// Editable lists the steps whose prompts a retro may propose changes to. Not
// the retro's own: an agent shouldn't rewrite its own instructions.
var Editable = []string{"refinement", "spec", "build"}

var ErrProposalClosed = errors.New("proposal is no longer open")

// Retro states on a ticket.
const (
	RetroPending = "pending"
	RetroRunning = "running"
	RetroDone    = "done"
	RetroFailed  = "failed"
)

// Proposal is a retro's suggested edit to a step's prompt, waiting for a human.
type Proposal struct {
	ID      string    `json:"id"`
	Ticket  string    `json:"ticket"`
	Step    string    `json:"step"`
	Find    string    `json:"find"`
	Replace string    `json:"replace"`
	Why     string    `json:"why"`
	Status  string    `json:"status"` // open, applied, dismissed, stale
	Created time.Time `json:"created"`
	Decided time.Time `json:"decided"`
}

const proposalSchema = `
CREATE TABLE IF NOT EXISTS proposals (
	id     TEXT PRIMARY KEY,
	seq    INTEGER NOT NULL,
	status TEXT NOT NULL,
	data   TEXT NOT NULL
);
`

func (s *Store) AddProposal(ctx context.Context, p Proposal) (Proposal, error) {
	var seq int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM proposals`).Scan(&seq); err != nil {
		return Proposal{}, fmt.Errorf("next proposal id: %w", err)
	}
	p.ID = fmt.Sprintf("P-%d", seq)
	data, err := json.Marshal(p)
	if err != nil {
		return Proposal{}, fmt.Errorf("encode proposal: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO proposals (id, seq, status, data) VALUES (?, ?, ?, ?)`, p.ID, seq, p.Status, data); err != nil {
		return Proposal{}, fmt.Errorf("insert proposal: %w", err)
	}
	return p, nil
}

func (s *Store) SaveProposal(ctx context.Context, p Proposal) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode proposal: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE proposals SET status = ?, data = ? WHERE id = ?`, p.Status, data, p.ID); err != nil {
		return fmt.Errorf("save proposal %s: %w", p.ID, err)
	}
	return nil
}

func (s *Store) GetProposal(ctx context.Context, id string) (Proposal, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM proposals WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: proposal %s", ErrNotFound, id)
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("get proposal %s: %w", id, err)
	}
	var p Proposal
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return Proposal{}, fmt.Errorf("decode proposal: %w", err)
	}
	return p, nil
}

// Proposals returns all proposals, open ones first, newest first.
func (s *Store) Proposals(ctx context.Context) ([]Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM proposals ORDER BY status = 'open' DESC, seq DESC`)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	defer rows.Close()
	ps := []Proposal{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("list proposals: %w", err)
		}
		var p Proposal
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, fmt.Errorf("decode proposal: %w", err)
		}
		ps = append(ps, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	return ps, nil
}

// ClaimRetro takes the oldest finished ticket whose retro hasn't run. The
// read and the update are one transaction, so two workers can't both take it.
func (s *Store) ClaimRetro(ctx context.Context) (Ticket, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("claim retro: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var data string
	err = tx.QueryRowContext(ctx, `SELECT data FROM tickets WHERE json_extract(data, '$.retro') = ? ORDER BY seq LIMIT 1`, RetroPending).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("claim retro: %w", err)
	}
	t, err := decode(data)
	if err != nil {
		return Ticket{}, false, err
	}
	t.Retro = RetroRunning
	if err := save(ctx, tx, t); err != nil {
		return Ticket{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Ticket{}, false, fmt.Errorf("claim retro: %w", err)
	}
	return t, true, nil
}

// RetroOnce runs the retro for one finished ticket. A retro failing is noted
// on the timeline and never changes the ticket's outcome.
func (e *Engine) RetroOnce(ctx context.Context) (bool, error) {
	t, ok, err := e.Store.ClaimRetro(ctx)
	if err != nil || !ok {
		return false, err
	}
	t.Retro = RetroDone
	if err := e.retro(ctx, &t); err != nil {
		t.Retro = RetroFailed
		e.event(ctx, &t, "error", "Retro failed", err.Error())
	}
	return true, e.Store.Save(ctx, t)
}

type stepPrompt struct{ Name, Body string }

type retroInput struct {
	Ticket   Ticket
	Timeline string
	Prompts  []stepPrompt
}

type retroOutput struct {
	Notes     string `json:"notes"`
	Proposals []struct {
		Step    string `json:"step"`
		Find    string `json:"find"`
		Replace string `json:"replace"`
		Why     string `json:"why"`
	} `json:"proposals"`
}

func (e *Engine) retro(ctx context.Context, t *Ticket) error {
	evs, err := e.Store.Events(ctx, t.ID)
	if err != nil {
		return err
	}
	in := retroInput{Ticket: *t, Timeline: timeline(evs)}
	for _, name := range Editable {
		body, err := agent.PromptBody(e.StepsDir, name)
		if err != nil {
			return fmt.Errorf("read %s prompt: %w", name, err)
		}
		in.Prompts = append(in.Prompts, stepPrompt{Name: name, Body: body})
	}
	var out retroOutput
	run, err := e.runAgent(ctx, "retro", in, "", &out)
	if err != nil {
		return err
	}
	t.CostUSD += run.Cost
	kept := 0
	for _, p := range out.Proposals {
		// Checked in code, not trusted: only editable steps, only prompt text,
		// and the edit has to apply cleanly to the file as it is now.
		if !slices.Contains(Editable, p.Step) {
			e.event(ctx, t, "error", "Retro proposal discarded: "+p.Step+" isn't an editable step", p.Why)
			continue
		}
		if err := agent.CheckEdit(e.StepsDir, p.Step, p.Find, p.Replace); err != nil {
			e.event(ctx, t, "error", "Retro proposal discarded", err.Error())
			continue
		}
		saved, err := e.Store.AddProposal(ctx, Proposal{Ticket: t.ID, Step: p.Step, Find: p.Find, Replace: p.Replace,
			Why: p.Why, Status: "open", Created: e.now()})
		if err != nil {
			return err
		}
		kept++
		e.event(ctx, t, "retro", fmt.Sprintf("Proposed %s: a change to the %s prompt", saved.ID, p.Step), p.Why)
	}
	msg := "Retro: no changes proposed"
	if kept > 0 {
		msg = fmt.Sprintf("Retro: %d change(s) proposed, waiting in Improvements", kept)
	}
	e.costEvent(ctx, t, "retro", msg, out.Notes, run)
	return nil
}

// Decide applies or dismisses a proposal. Applying re-checks it against the
// step file as it is now: if the file changed since, the proposal is stale.
func (e *Engine) Decide(ctx context.Context, id string, apply bool) (Proposal, error) {
	p, err := e.Store.GetProposal(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	if p.Status != "open" {
		return p, fmt.Errorf("%w: %s is %s", ErrProposalClosed, p.ID, p.Status)
	}
	p.Decided = e.now()
	p.Status = "dismissed"
	if apply {
		p.Status = "applied"
		if err := agent.EditPrompt(e.StepsDir, p.Step, p.Find, p.Replace); err != nil {
			p.Status = "stale"
			if saveErr := e.Store.SaveProposal(ctx, p); saveErr != nil {
				return p, saveErr
			}
			return p, fmt.Errorf("%w: %s no longer applies: %w", ErrProposalClosed, p.ID, err)
		}
	}
	if err := e.Store.SaveProposal(ctx, p); err != nil {
		return p, err
	}
	if t, err := e.Store.Get(ctx, p.Ticket); err == nil {
		e.event(ctx, &t, "human", fmt.Sprintf("You %s %s (%s prompt)", p.Status, p.ID, p.Step), "")
	}
	return p, nil
}

// SetVerdict records whether a finished ticket did what the human wanted, and
// queues its retro again so it learns from the answer. It's one transaction
// with the retro claim, so a retro can't run on a stale copy and lose it.
func (s *Store) SetVerdict(ctx context.Context, id string, good bool, comment string) (Ticket, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, fmt.Errorf("verdict: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var data string
	err = tx.QueryRowContext(ctx, `SELECT data FROM tickets WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("verdict: %w", err)
	}
	t, err := decode(data)
	if err != nil {
		return Ticket{}, err
	}
	switch {
	case t.Status != Done && t.Status != Abandoned:
		return t, fmt.Errorf("%w: %s hasn't finished yet", ErrInvalidAction, t.ID)
	case t.Retro == RetroRunning:
		return t, fmt.Errorf("%w: the retro for %s is running, try again in a moment", ErrInvalidAction, t.ID)
	}
	t.Verdict = "no"
	if good {
		t.Verdict = "yes"
	}
	t.VerdictComment = comment
	t.Retro = RetroPending
	if err := save(ctx, tx, t); err != nil {
		return Ticket{}, err
	}
	if err := tx.Commit(); err != nil {
		return Ticket{}, fmt.Errorf("verdict: %w", err)
	}
	return t, nil
}

// Feedback records the human's verdict on a finished ticket.
func (e *Engine) Feedback(ctx context.Context, id string, good bool, comment string) (Ticket, error) {
	comment = strings.TrimSpace(comment)
	t, err := e.Store.SetVerdict(ctx, id, good, comment)
	if err != nil {
		return t, err
	}
	msg := "You said it didn't do what you wanted"
	if good {
		msg = "You said it did what you wanted"
	}
	e.event(ctx, &t, "human", msg+"; the retro will take it into account", comment)
	e.Wake()
	return t, nil
}

// Stats summarises finished tickets, so a prompt change can be judged by
// whether the numbers move: fewer questions, fewer rounds, less spend.
type Stats struct {
	Finished      int     `json:"finished"`
	Deployed      int     `json:"deployed"`
	Rated         int     `json:"rated"` // finished tickets with a verdict
	Wanted        int     `json:"wanted"`
	AvgQuestions  float64 `json:"avgQuestions"`
	AvgBuilds     float64 `json:"avgBuilds"`
	AvgCostUSD    float64 `json:"avgCostUSD"`
	OpenProposals int     `json:"openProposals"`
}

func (e *Engine) Stats(ctx context.Context) (Stats, error) {
	ts, err := e.Store.List(ctx)
	if err != nil {
		return Stats{}, err
	}
	var s Stats
	for _, t := range ts {
		if t.Status != Done && t.Status != Abandoned {
			continue
		}
		s.Finished++
		if t.Outcome == "deployed" {
			s.Deployed++
		}
		if t.Verdict != "" {
			s.Rated++
			if t.Verdict == "yes" {
				s.Wanted++
			}
		}
		s.AvgQuestions += float64(t.Questions)
		s.AvgBuilds += float64(t.Builds)
		s.AvgCostUSD += t.CostUSD
	}
	if s.Finished > 0 {
		n := float64(s.Finished)
		s.AvgQuestions /= n
		s.AvgBuilds /= n
		s.AvgCostUSD /= n
	}
	ps, err := e.Store.Proposals(ctx)
	if err != nil {
		return Stats{}, err
	}
	for _, p := range ps {
		if p.Status == "open" {
			s.OpenProposals++
		}
	}
	return s, nil
}

// timeline renders events for the retro's prompt, with long details trimmed.
func timeline(evs []Event) string {
	var b strings.Builder
	for _, ev := range evs {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", ev.Step, ev.Kind, ev.Message)
		if d := strings.TrimSpace(ev.Detail); d != "" {
			if len(d) > 800 {
				d = d[:800] + "…"
			}
			fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(d, "\n", "\n  "))
		}
	}
	return b.String()
}
