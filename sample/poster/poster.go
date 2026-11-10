// Package main renders an event poster as a single HTML page.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
)

type Poster struct {
	Title   string `json:"title"`
	Date    string `json:"date"`
	Tagline string `json:"tagline"`
}

func Load(path string) (Poster, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Poster{}, fmt.Errorf("read poster: %w", err)
	}
	var p Poster
	if err := json.Unmarshal(raw, &p); err != nil {
		return Poster{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if p.Title == "" {
		return Poster{}, fmt.Errorf("%s: title is required", path)
	}
	return p, nil
}

var page = template.Must(template.New("poster").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<style>
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font-family: system-ui, sans-serif; background: #f4f1ea; color: #1a1a1a; }
  .poster { padding: 4rem; border: 6px solid #1a1a1a; text-align: center; max-width: 40rem; }
  h1 { font-size: 3.5rem; margin: 0 0 1rem; }
  .date { font-size: 1.5rem; letter-spacing: 0.1em; }
  .tagline { font-style: italic; margin-top: 2rem; }
</style>
</head>
<body>
<div class="poster">
  <h1>{{.Title}}</h1>
  {{with .Date}}<div class="date">{{.}}</div>{{end}}
  {{with .Tagline}}<div class="tagline">{{.}}</div>{{end}}
</div>
</body>
</html>
`))

func Render(w io.Writer, p Poster) error {
	if err := page.Execute(w, p); err != nil {
		return fmt.Errorf("render poster: %w", err)
	}
	return nil
}
