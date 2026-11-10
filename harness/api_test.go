package harness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") && !strings.HasPrefix(rec.Body.String(), "[") {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestAPI(t *testing.T) {
	hn := newHarness(t)
	page := filepath.Join(t.TempDir(), "poster.html")
	srv := Server(hn.e, page)

	if code, _ := call(t, srv, "GET", "/", ""); code != http.StatusOK {
		t.Fatalf("UI: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/tickets", `{"title":"  "}`); code != http.StatusBadRequest {
		t.Fatalf("blank title: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/tickets", `nope`); code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
	code, created := call(t, srv, "POST", "/api/tickets", `{"title":"Dark mode","description":"make it THIN"}`)
	if code != http.StatusCreated || created["id"] != "T-1" {
		t.Fatalf("create: %d %v", code, created)
	}
	if code, _ := call(t, srv, "GET", "/api/tickets", ""); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	hn.drain("T-1")

	code, got := call(t, srv, "GET", "/api/tickets/T-1", "")
	if code != http.StatusOK || got["actions"] == nil {
		t.Fatalf("get: %d %v", code, got)
	}
	if code, _ := call(t, srv, "GET", "/api/tickets/T-99", ""); code != http.StatusNotFound {
		t.Fatalf("missing: %d", code)
	}
	if code, out := call(t, srv, "POST", "/api/tickets/T-1/actions", `{"action":"approve"}`); code != http.StatusConflict {
		t.Fatalf("approve while asking questions: %d %v", code, out)
	}
	if code, _ := call(t, srv, "POST", "/api/tickets/T-1/actions", `{"action":"answer","comment":"blue"}`); code != http.StatusOK {
		t.Fatalf("answer: %d", code)
	}

	for _, path := range []string{"/api/proposals", "/api/stats"} {
		if code, _ := call(t, srv, "GET", path, ""); code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
	}
	if code, _ := call(t, srv, "POST", "/api/proposals/P-1", `{"action":"maybe"}`); code != http.StatusBadRequest {
		t.Fatalf("bad proposal action: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/proposals/P-9", `{"action":"apply"}`); code != http.StatusNotFound {
		t.Fatalf("missing proposal: %d", code)
	}

	if code, _ := call(t, srv, "GET", "/poster", ""); code != http.StatusNotFound {
		t.Fatalf("poster before deploy: %d", code)
	}
	if err := os.WriteFile(page, []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, srv, "GET", "/poster", ""); code != http.StatusOK {
		t.Fatalf("poster: %d", code)
	}
}
