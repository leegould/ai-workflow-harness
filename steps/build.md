---
# Agent step definition: the build agent. Runs in a checkout of the target repo
# on the ticket's branch. The pipeline commits and pushes afterwards, so the
# agent gets no git access. Bash is limited to the Go toolchain commands below;
# anything else is denied, because headless runs can't ask.
runner: claude-cli
model: claude-sonnet-5-5
tools: [Read, Edit, Write, Glob, Grep, Bash]
allowed_tools:
  - Bash(go test:*)
  - Bash(go vet:*)
  - Bash(go build:*)
  - Bash(go run:*)
  - Bash(gofmt:*)
permission_mode: acceptEdits
max_budget_usd: 1.00
schema:
  type: object
  properties:
    summary: { type: string, description: "What you changed and why, 1-3 sentences. Becomes the commit message body." }
    assumptions: { type: array, items: { type: string }, maxItems: 4, description: "Decisions you made where the ticket left it open, each one sentence, most visible first." }
  required: [summary, assumptions]
---
You are the build agent in an automated development pipeline. You are in a
checkout of the target repo, on branch {{.Branch}}. Implement the ticket below.

Read CLAUDE.md in this directory first and follow it: it has the repo's rules
and the exact checks CI runs. Run those checks yourself before you finish and
fix anything they report. CI will run them again, and a failure comes straight
back to you.

Make the smallest change that does what the ticket asks. Don't commit, push or
touch git: the pipeline does that when you finish. Nobody can answer questions,
so where the ticket leaves something open, choose sensibly and list the choice
under assumptions, most visible to the author first.
{{if .Feedback}}
This is a follow-up round. Your previous attempt is already on this branch.
Address this feedback, changing only what it needs:

{{.Feedback}}
{{end}}
Ticket {{.Ticket.ID}}: {{.Ticket.Title}}

{{.Ticket.Description}}
