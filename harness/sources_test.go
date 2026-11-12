package harness

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func sentryPayload(id, action string) []byte {
	return fmt.Appendf(nil, `{"action":%q,"data":{"issue":{"id":%q,"title":"Unknown theme \"Blue\"","culprit":"main.Render (poster.go)","level":"warning","count":"14","web_url":"https://sentry.example.invalid/issues/%s/"}}}`, action, id, id)
}

func (h *harness) report(source string, body []byte) (Ticket, string) {
	h.t.Helper()
	t, outcome, err := h.e.Report(context.Background(), source, http.Header{}, body)
	if err != nil {
		h.t.Fatal(err)
	}
	return t, outcome
}

// An error report becomes a ticket that goes through the whole flow like any
// other: risk-assessed merge, then dev, then the production gate.
func TestSentryReportBecomesATicket(t *testing.T) {
	h := newHarness(t)
	h.e.Config.Production.Approval = ProductionManual
	tk, outcome := h.report("sentry", sentryPayload("4501", "created"))
	if outcome != ReportCreated || !tk.Reactive || tk.ExternalID != "sentry:4501" || !strings.Contains(tk.Description, "Where: main.Render") {
		t.Fatalf("got %s %+v", outcome, tk)
	}
	tk = h.drain(tk.ID)
	expectWaiting(t, tk, WaitPromote)
	if tk.Risk != "low" {
		t.Fatalf("got %+v", tk)
	}
	h.act(tk.ID, ActionPromote, "")
	expectDeployed(t, h.drain(tk.ID))
}

func TestRepeatReportsJoinTheOpenTicket(t *testing.T) {
	h := newHarness(t)
	first, _ := h.report("sentry", sentryPayload("4501", "created"))
	again, outcome := h.report("sentry", sentryPayload("4501", "triggered"))
	if outcome != ReportDuplicate || again.ID != first.ID || again.Occurrences != 2 {
		t.Fatalf("got %s %+v", outcome, again)
	}
	if ts, _ := h.e.Store.List(context.Background()); len(ts) != 1 {
		t.Fatalf("want one ticket, got %d", len(ts))
	}
}

// Reports that arrive together must not each open a ticket.
func TestSimultaneousReportsOpenOneTicket(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, _, err := h.e.Report(context.Background(), "sentry", http.Header{}, sentryPayload("4501", "created")); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	ts, _ := h.e.Store.List(context.Background())
	if len(ts) != 1 || ts[0].Occurrences != 8 {
		t.Fatalf("got %d tickets, %+v", len(ts), ts)
	}
}

func TestAProblemThatComesBackPointsAtTheFixThatDidntHold(t *testing.T) {
	h := newHarness(t)
	first, _ := h.report("sentry", sentryPayload("4501", "created"))
	expectDeployed(t, h.drain(first.ID))
	back, outcome := h.report("sentry", sentryPayload("4501", "unresolved"))
	if outcome != ReportCreated || back.ID == first.ID || back.RegressionOf != first.ID || !strings.Contains(back.Description, "came back after "+first.ID) {
		t.Fatalf("got %s %+v", outcome, back)
	}
}

func TestReportLimitsAndIgnores(t *testing.T) {
	h := newHarness(t)
	if _, outcome := h.report("sentry", sentryPayload("1", "resolved")); outcome != ReportIgnored {
		t.Fatalf("resolved: got %s", outcome)
	}
	for i := range 3 {
		if _, outcome := h.report("sentry", sentryPayload(fmt.Sprint(i), "created")); outcome != ReportCreated {
			t.Fatalf("report %d: got %s", i, outcome)
		}
	}
	if _, outcome := h.report("sentry", sentryPayload("9", "created")); outcome != ReportLimited {
		t.Fatalf("over max_open: got %s", outcome)
	}
	// Another source has its own limit.
	if _, outcome := h.report("webhook", []byte(`{"id":"a","title":"Add a footer"}`)); outcome != ReportCreated {
		t.Fatalf("webhook: got %s", outcome)
	}
}

func TestReportErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		source string
		body   string
		want   error
	}{
		"unknown source":     {"jira", `{}`, ErrUnknownSource},
		"not json":           {"sentry", `nope`, ErrBadReport},
		"no issue":           {"sentry", `{"action":"created","data":{}}`, ErrBadReport},
		"issue with no id":   {"sentry", `{"action":"created","data":{"issue":{"title":"x"}}}`, ErrBadReport},
		"webhook with no id": {"webhook", `{"title":"x"}`, ErrBadReport},
		"webhook not json":   {"webhook", `[`, ErrBadReport},
	} {
		if _, _, err := h.e.Report(ctx, tc.source, http.Header{}, []byte(tc.body)); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestSignedSources(t *testing.T) {
	h := newHarness(t)
	t.Setenv("SENTRY_WEBHOOK_SECRET", "s3cret")
	body := sentryPayload("4501", "created")
	if _, _, err := h.e.Report(context.Background(), "sentry", http.Header{}, body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unsigned: got %v", err)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	hd := http.Header{"Sentry-Hook-Signature": {hex.EncodeToString(mac.Sum(nil))}}
	if _, outcome, err := h.e.Report(context.Background(), "sentry", hd, body); err != nil || outcome != ReportCreated {
		t.Fatalf("signed: got %s %v", outcome, err)
	}
	wf, _ := h.e.Workflow()
	for _, s := range wf.Sources {
		if s.Name == "sentry" && !s.Signed {
			t.Fatal("workflow should show sentry as signed")
		}
	}
}

func TestSourcesAPI(t *testing.T) {
	srv := Server(newHarness(t).e, nil)
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/api/sources/sentry", string(sentryPayload("1", "created")), http.StatusCreated},
		{"/api/sources/sentry", string(sentryPayload("1", "created")), http.StatusOK},
		{"/api/sources/sentry", string(sentryPayload("2", "resolved")), http.StatusOK},
		{"/api/sources/sentry", string(sentryPayload("3", "created")), http.StatusCreated},
		{"/api/sources/sentry", string(sentryPayload("4", "created")), http.StatusCreated},
		{"/api/sources/sentry", string(sentryPayload("5", "created")), http.StatusTooManyRequests},
		{"/api/sources/sentry", `nope`, http.StatusBadRequest},
		{"/api/sources/jira", `{}`, http.StatusNotFound},
	} {
		if code, out := call(t, srv, "POST", tc.path, tc.body); code != tc.want {
			t.Errorf("%s %s: got %d %v, want %d", tc.path, tc.body[:min(30, len(tc.body))], code, out, tc.want)
		}
	}
}

func TestLoadConfigRejectsBadSources(t *testing.T) {
	for name, body := range map[string]string{
		"no adapter": "sources:\n  jira:\n    kind: planned\n",
		"bad kind":   "sources:\n  sentry:\n    kind: urgent\n",
	} {
		p := filepath.Join(t.TempDir(), "p.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A repeat report that lands while a worker is mid-step on the ticket must
// still count: the worker's save mustn't overwrite it. (Found in a live run.)
func TestRepeatReportDuringAStepStillCounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, _ := h.report("sentry", sentryPayload("4501", "created"))
	claimed, ok, err := h.e.Store.Claim(ctx, h.clock)
	if err != nil || !ok || claimed.ID != first.ID {
		t.Fatalf("claim: %v %v", ok, err)
	}
	h.report("sentry", sentryPayload("4501", "created"))
	if err := h.e.process(ctx, claimed); err != nil { // the worker finishes its step and saves
		t.Fatal(err)
	}
	if tk, _ := h.e.Store.Get(ctx, first.ID); tk.Occurrences != 2 {
		t.Fatalf("occurrences %d, want 2", tk.Occurrences)
	}
}
