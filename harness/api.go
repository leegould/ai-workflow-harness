package harness

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strings"
)

//go:embed ui
var uiFiles embed.FS

// Server is the HTTP API plus the UI. PosterPage is the deployed app's output,
// so the UI can show what each ticket actually shipped.
func Server(e *Engine, posterPage string) http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))

	mux.HandleFunc("GET /api/tickets", func(w http.ResponseWriter, r *http.Request) {
		ts, err := e.Store.List(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if ts == nil {
			ts = []Ticket{}
		}
		reply(w, http.StatusOK, ts)
	})

	mux.HandleFunc("POST /api/tickets", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Title, Description string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		in.Title = strings.TrimSpace(in.Title)
		if in.Title == "" {
			fail(w, http.StatusBadRequest, errors.New("a ticket needs a title"))
			return
		}
		t, err := e.Create(r.Context(), in.Title, strings.TrimSpace(in.Description))
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		reply(w, http.StatusCreated, t)
	})

	mux.HandleFunc("GET /api/tickets/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, err := e.Store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, status(err), err)
			return
		}
		evs, err := e.Store.Events(r.Context(), t.ID)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		reply(w, http.StatusOK, map[string]any{"ticket": t, "events": evs, "actions": t.Actions()})
	})

	mux.HandleFunc("POST /api/tickets/{id}/actions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Action  Action
			Comment string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		t, err := e.Act(r.Context(), r.PathValue("id"), in.Action, in.Comment)
		if err != nil {
			fail(w, status(err), err)
			return
		}
		reply(w, http.StatusOK, t)
	})

	mux.HandleFunc("GET /api/proposals", func(w http.ResponseWriter, r *http.Request) {
		ps, err := e.Store.Proposals(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		reply(w, http.StatusOK, ps)
	})

	mux.HandleFunc("POST /api/proposals/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Action string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Action != "apply" && in.Action != "dismiss") {
			fail(w, http.StatusBadRequest, errors.New(`action must be "apply" or "dismiss"`))
			return
		}
		p, err := e.Decide(r.Context(), r.PathValue("id"), in.Action == "apply")
		if err != nil {
			fail(w, status(err), err)
			return
		}
		reply(w, http.StatusOK, p)
	})

	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		s, err := e.Stats(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		reply(w, http.StatusOK, s)
	})

	mux.HandleFunc("GET /poster", func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(posterPage); err != nil {
			http.Error(w, "Nothing deployed yet.", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, posterPage)
	})
	return mux
}

func status(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalidAction), errors.Is(err, ErrProposalClosed):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	reply(w, code, map[string]string{"error": err.Error()})
}
