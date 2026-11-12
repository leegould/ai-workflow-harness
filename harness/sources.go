package harness

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// A source is somewhere tickets come from besides the New ticket button: a
// webhook at POST /api/sources/<name>, with an adapter that turns the
// sender's payload into a report. Planned sources are a person asking for
// something (an issue tracker). Reactive sources are a system noticing
// something (an error tracker), and get stricter handling: see Report.
const (
	SourcePlanned  = "planned"
	SourceReactive = "reactive"
)

var (
	ErrUnknownSource = errors.New("no such source in pipeline.yaml")
	ErrBadSignature  = errors.New("report signature missing or wrong")
	ErrBadReport     = errors.New("report not understood")
)

type SourceConfig struct {
	Kind    string `yaml:"kind"`
	MaxOpen int    `yaml:"max_open"`
	// SecretEnv names the environment variable holding the signing secret.
	// When that variable is set, unsigned reports are refused.
	SecretEnv string `yaml:"secret_env"`
}

// report is what an adapter makes of a payload.
type report struct {
	ExternalID  string // the same problem reported twice has the same ID
	Title       string
	Description string
	URL         string
}

// An adapter parses one sender's payload. ok is false for payloads that
// aren't a problem to work on (Sentry telling us an issue was resolved, say).
type adapter struct {
	signatureHeader string
	parse           func(h http.Header, body []byte) (r report, ok bool, err error)
}

var adapters = map[string]adapter{
	"sentry":  {signatureHeader: "Sentry-Hook-Signature", parse: parseSentry},
	"webhook": {signatureHeader: "X-Harness-Signature", parse: parseWebhook},
}

// Outcomes of a report.
const (
	ReportCreated   = "created"   // a new ticket
	ReportDuplicate = "duplicate" // added to the open ticket for the same problem
	ReportLimited   = "limited"   // dropped: the source has max_open tickets open
	ReportIgnored   = "ignored"   // not a problem to work on
)

// Report turns a webhook from a source into a ticket. The guardrails are
// here rather than in each adapter, because a machine creating tickets can
// create a lot of them:
//   - a report of a problem that already has an open ticket adds to that
//     ticket instead of opening another
//   - a source has at most max_open tickets open at once; more are dropped
//   - a problem that comes back after its fix was deployed opens a new
//     ticket that points at the fix that didn't hold
//   - fixes for reactive sources always wait for a human before merging
func (e *Engine) Report(ctx context.Context, source string, h http.Header, body []byte) (Ticket, string, error) {
	cfg, ok := e.Config.Sources[source]
	if !ok {
		return Ticket{}, "", fmt.Errorf("%w: %s", ErrUnknownSource, source)
	}
	ad := adapters[source]
	if err := verify(cfg, ad, h, body); err != nil {
		return Ticket{}, "", err
	}
	r, ok, err := ad.parse(h, body)
	if err != nil {
		return Ticket{}, "", fmt.Errorf("%w: %w", ErrBadReport, err)
	}
	if !ok {
		return Ticket{}, ReportIgnored, nil
	}
	now := e.now()
	t := Ticket{
		Title: r.Title, Description: r.Description,
		Source: source, ExternalID: source + ":" + r.ExternalID, ExternalURL: r.URL,
		Reactive: cfg.Kind == SourceReactive, Occurrences: 1,
		Status: Queued, Step: StepRefinement, Attempt: 1, Created: now, Updated: now,
	}
	t, outcome, err := e.Store.CreateReported(ctx, t, cfg.MaxOpen)
	if err != nil {
		return Ticket{}, "", err
	}
	switch outcome {
	case ReportCreated:
		msg := "Raised by " + source
		if t.RegressionOf != "" {
			msg += ": it came back after " + t.RegressionOf + " was deployed"
		}
		e.event(ctx, &t, "info", msg, r.URL)
		e.Wake()
	case ReportDuplicate:
		e.event(ctx, &t, "info", fmt.Sprintf("Reported again by %s (%d times now)", source, t.Occurrences), "")
	}
	return t, outcome, nil
}

func verify(cfg SourceConfig, ad adapter, h http.Header, body []byte) error {
	if cfg.SecretEnv == "" {
		return nil
	}
	secret := os.Getenv(cfg.SecretEnv)
	if secret == "" {
		return nil
	}
	got, err := hex.DecodeString(h.Get(ad.signatureHeader))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return ErrBadSignature
	}
	return nil
}

// parseWebhook is the generic source: anything that can send
// {"id", "title", "description", "url"}.
func parseWebhook(_ http.Header, body []byte) (report, bool, error) {
	var in struct{ ID, Title, Description, URL string }
	if err := json.Unmarshal(body, &in); err != nil {
		return report{}, false, fmt.Errorf("parse webhook: %w", err)
	}
	if in.ID == "" || strings.TrimSpace(in.Title) == "" {
		return report{}, false, errors.New("a report needs an id and a title")
	}
	return report{ExternalID: in.ID, Title: strings.TrimSpace(in.Title), Description: in.Description, URL: in.URL}, true, nil
}

// sentryIssue covers the fields used from both of Sentry's payloads: issue
// webhooks (data.issue) and issue alerts (data.event, which carries the
// issue's ID as issue_id).
type sentryIssue struct {
	ID       string          `json:"id"`
	IssueID  string          `json:"issue_id"`
	Title    string          `json:"title"`
	Culprit  string          `json:"culprit"`
	Level    string          `json:"level"`
	Count    json.RawMessage `json:"count"` // a string in Sentry's API
	WebURL   string          `json:"web_url"`
	Metadata struct {
		Type     string `json:"type"`
		Value    string `json:"value"`
		Filename string `json:"filename"`
		Function string `json:"function"`
	} `json:"metadata"`
}

// parseSentry handles new and regressed issues, and alert rules firing.
// Resolved, assigned and ignored issues aren't work, so they're ignored.
func parseSentry(_ http.Header, body []byte) (report, bool, error) {
	var in struct {
		Action string `json:"action"`
		Data   struct {
			Issue *sentryIssue `json:"issue"`
			Event *sentryIssue `json:"event"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return report{}, false, fmt.Errorf("parse sentry payload: %w", err)
	}
	is := in.Data.Issue
	if is == nil {
		is = in.Data.Event
	}
	if is == nil {
		return report{}, false, errors.New("sentry payload has no issue or event")
	}
	switch in.Action {
	case "created", "unresolved", "triggered":
	default:
		return report{}, false, nil
	}
	id := is.ID
	if is.IssueID != "" {
		id = is.IssueID
	}
	if id == "" || is.Title == "" {
		return report{}, false, errors.New("sentry issue has no id or title")
	}
	var d strings.Builder
	fmt.Fprintf(&d, "Sentry reported this %s in production.\n\n", orDefault(is.Level, "error"))
	fmt.Fprintf(&d, "Error: %s\n", is.Title)
	if m := is.Metadata; m.Type != "" || m.Value != "" {
		fmt.Fprintf(&d, "Type: %s %s\n", m.Type, m.Value)
	}
	if is.Culprit != "" {
		fmt.Fprintf(&d, "Where: %s\n", is.Culprit)
	}
	if m := is.Metadata; m.Filename != "" || m.Function != "" {
		fmt.Fprintf(&d, "In: %s %s\n", m.Filename, m.Function)
	}
	if c := strings.Trim(string(is.Count), `"`); c != "" {
		fmt.Fprintf(&d, "Seen: %s times\n", c)
	}
	if is.WebURL != "" {
		fmt.Fprintf(&d, "Issue: %s\n", is.WebURL)
	}
	d.WriteString("\nFind the cause in the code and fix it. The outcome is that this error stops " +
		"happening. If the code doesn't show the cause, say so rather than guessing.")
	return report{ExternalID: id, Title: "Fix: " + is.Title, Description: d.String(), URL: is.WebURL}, true, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
