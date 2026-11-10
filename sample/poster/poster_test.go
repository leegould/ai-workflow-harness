package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderIncludesContent(t *testing.T) {
	var b strings.Builder
	if err := Render(&b, Poster{Title: "Launch Night", Date: "2 Nov", Tagline: "Bring snacks"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h1>Launch Night</h1>", "2 Nov", "Bring snacks"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	var b strings.Builder
	if err := Render(&b, Poster{Title: "<script>x</script>"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "<script>") {
		t.Error("title was not escaped")
	}
}

func TestLoadRequiresTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(`{"date": "2 Nov"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for a missing title")
	}
}

func TestRunWritesPage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "poster.html")
	if err := run("poster.json", out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal(err)
	}
}
