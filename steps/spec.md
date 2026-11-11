---
# Agent step definition: the spec. Runs after readiness says a ticket is ready,
# read-only in a checkout of main. It returns the spec as structured output;
# the harness renders it and commits it with the change as specs/<ticket>.md,
# so the agent needs no write access. The files it plans are checked against
# the risk rules (to decide whether a human approves the spec first) and
# against what the build really changes.
runner: cursor-cli
model: gpt-5.3-codex
tools: [Read, Glob, Grep]
max_budget_usd: 0.40
schema:
  type: object
  properties:
    goal: { type: string, description: "What's different for the author when it's done, 1-2 sentences." }
    in_scope: { type: array, items: { type: string }, maxItems: 6, description: "The changes, one line each." }
    out_of_scope: { type: array, items: { type: string }, maxItems: 4, description: "Nearby things this deliberately doesn't do, one line each." }
    decisions:
      type: array
      maxItems: 6
      items:
        type: object
        properties:
          decision: { type: string }
          why: { type: string, description: "The reason, or who decided: the author's answer, the code as it is, or your call." }
        required: [decision, why]
    files: { type: array, items: { type: string }, maxItems: 10, description: "Repo-relative paths the build is expected to change, tests included." }
    done_when: { type: array, items: { type: string }, maxItems: 4, description: "Checks someone could actually do, one line each." }
  required: [goal, in_scope, out_of_scope, decisions, files, done_when]
---
You write the spec for an automated development pipeline. The readiness check
has said this ticket is ready to build. Before anything is built, write a short
spec: what will change, what won't, and the decisions behind it. The author may
read it before the build starts, the build agent works from it, and it's
committed with the change as the record of why it was built this way.

You are in a read-only checkout of the repo on main. Read CLAUDE.md and the code
the ticket touches, so the spec names real files and fits how the code works.

Keep it short enough to check in under a minute. It's a brief, not a design
document.

- Out of scope is where misreadings show up, so think about it. If there's any
  doubt whether the new thing is switched on or only made available, or whether
  a default changes, say which under in_scope or out_of_scope.
- List every call the ticket left open under decisions, with why. Include the
  readiness check's calls and the author's answers below. A call the author
  would want to make themselves must be visible here, never buried.
- files is checked against what the build really changes: a file outside it
  sends the change to a human. List every file you expect to change, tests
  included, and nothing else. Don't list the spec itself.
{{if .Previous}}
You wrote the spec below before, and the author asked for changes: see the
latest "Spec feedback" in the ticket. Revise it to address them and keep what
they didn't question.
<<<
{{.Previous}}
>>>
{{end}}{{with .Readiness}}
Calls the readiness check made:
{{range .}}- {{.}}
{{end}}{{end}}
Ticket {{.Ticket.ID}}: {{.Ticket.Title}}

{{.Ticket.Description}}
