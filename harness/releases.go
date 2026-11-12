package harness

import (
	"context"
	"fmt"
	"slices"

	"aiharness/target"
)

// ReleaseItem is a ticket that's merged but not in production yet.
type ReleaseItem struct {
	Ticket Ticket `json:"ticket"`
	// State: ready (passed on dev, waiting to ship), testing (on its way
	// through dev) or needs-you (waiting on a human for something else).
	State string `json:"state"`
	// Ships lists the earlier unreleased tickets that shipping this one
	// would ship too, because main as of its merge contains them.
	Ships []string `json:"ships"`
	// Blocked says why it can't ship yet, if it can't.
	Blocked string `json:"blocked,omitempty"`
}

type Releases struct {
	Production string        `json:"production"`
	Dev        string        `json:"dev"`
	Items      []ReleaseItem `json:"items"`
}

// Releases is what's on dev and not in production, in merge order, and what
// shipping each would take with it.
func (e *Engine) Releases(ctx context.Context) (Releases, error) {
	var r Releases
	var err error
	if r.Production, err = e.Repo.Deployed(ctx, target.Production); err != nil {
		return r, fmt.Errorf("read production version: %w", err)
	}
	if r.Dev, err = e.Repo.Deployed(ctx, target.Dev); err != nil {
		return r, fmt.Errorf("read dev version: %w", err)
	}
	pending, err := e.unreleased(ctx)
	if err != nil {
		return r, err
	}
	r.Items = []ReleaseItem{}
	for _, t := range pending {
		item := ReleaseItem{Ticket: t, State: "testing", Ships: []string{}}
		switch {
		case t.Wait == WaitPromote:
			item.State = "ready"
		case t.Status == Waiting:
			item.State = "needs-you"
		}
		batch, err := e.batch(ctx, t)
		if err != nil {
			return r, err
		}
		for _, b := range batch {
			item.Ships = append(item.Ships, b.ID)
			if b.Wait != WaitPromote && item.Blocked == "" {
				item.Blocked = b.ID + " is merged before it and hasn't passed on dev yet"
			}
		}
		r.Items = append(r.Items, item)
	}
	return r, nil
}

// unreleased lists tickets that are merged but not in production, oldest
// merge first.
func (e *Engine) unreleased(ctx context.Context) ([]Ticket, error) {
	all, err := e.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	pending := slices.DeleteFunc(all, func(t Ticket) bool {
		return t.Merged == "" || t.Released != "" || t.Status == Done || t.Status == Abandoned
	})
	// List is newest first; reverse it so tickets merged at the same moment
	// stay in the order they were created.
	slices.Reverse(pending)
	slices.SortStableFunc(pending, func(a, b Ticket) int { return a.MergedAt.Compare(b.MergedAt) })
	return pending, nil
}

// batch is the other unreleased tickets that shipping t would ship with it.
func (e *Engine) batch(ctx context.Context, t Ticket) ([]Ticket, error) {
	pending, err := e.unreleased(ctx)
	if err != nil {
		return nil, err
	}
	var out []Ticket
	for _, p := range pending {
		if p.ID == t.ID {
			continue
		}
		in, err := e.Repo.Contains(ctx, t.Merged, p.Merged)
		if err != nil {
			return nil, fmt.Errorf("compare %s with %s: %w", t.ID, p.ID, err)
		}
		if in {
			out = append(out, p)
		}
	}
	return out, nil
}

// shippedWith marks the tickets that went to production inside t's release
// as released too: their changes are live, and their gate is answered.
func (e *Engine) shippedWith(ctx context.Context, t *Ticket) error {
	batch, err := e.batch(ctx, *t)
	if err != nil {
		return err
	}
	var ids []string
	for _, b := range batch {
		if b.Wait != WaitPromote {
			continue
		}
		b.Released = t.Released
		b.Status, b.Outcome, b.Retro = Done, "deployed", RetroPending
		b.Wait, b.WaitText = "", ""
		b.Updated = e.now()
		if err := e.Store.Save(ctx, b); err != nil {
			return err
		}
		e.event(ctx, &b, "ship", "Released to production with "+t.ID+" ("+short(t.Released)+")", "")
		ids = append(ids, b.ID)
	}
	if len(ids) > 0 {
		e.event(ctx, t, "ship", fmt.Sprintf("Also released %v, merged before it", ids), "")
	}
	return nil
}
