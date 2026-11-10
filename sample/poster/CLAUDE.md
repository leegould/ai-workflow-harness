# Poster

A tiny Go CLI: `go run . -in poster.json -out poster.html` renders an event
poster as one self-contained HTML page.

Rules for agents working here:
- Standard library only. No new dependencies.
- The page must stay self-contained: inline CSS, no external fonts, scripts or images.
- Keep `-in` and `-out` working as they are: the deploy smoke test runs
  `poster -in poster.json -out poster.html`.
- Add or update a test for any behaviour you change.
- Before you finish, run `gofmt -l .`, `go vet ./...` and `go test ./...` and
  fix anything they report. CI runs exactly these.
