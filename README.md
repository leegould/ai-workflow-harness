# ai-workflow-harness

The same ticket-to-deploy pipeline as [ai-workflow](https://github.com/leegould/ai-workflow),
packaged as **one program with a UI** instead of a Temporal workflow plus workers. Run it,
open the browser, create a ticket, and watch it go from refinement to a deployed change.
Anything that needs a person (questions, approval, a stuck loop) shows up as a card with
the buttons for what you can do.

```sh
go run ./cmd/harness          # then open http://localhost:8080
make check                    # lint, govulncheck, race tests, coverage
```

It needs Go and a logged-in Claude Code CLI. On first run it creates a local stand-in for
GitHub in `~/.cache/ai-workflow-harness-target`, seeded with `sample/poster`, a tiny Go app
that renders an event poster. Every merge has a visible result: the UI links to the
deployed poster.

## The flow

```
refinement ─ not ready ─▶ questions for a human ─▶ answers ─▶ refinement
    │ ready
    ▼
spec ─ risky plan ─▶ human approves, or asks for changes ─▶ spec
    │ low
    ▼
build ─▶ CI ─ fail ─▶ build (max 3 rounds, then escalate to a human)
           │ pass
           ▼
        review ─ changes ─▶ build
           │ approved
           ▼
        risk gate ─ medium/high ─▶ human approves
           │ low
           ▼
        merge ─▶ deploy to dev ─▶ smoke ─ fail ─▶ revert, redeploy dev, escalate (retry = fresh branch)
                                    │ pass
                                    ▼
                                 promote ─ manual ─▶ human ships it, or sends it back (revert, rebuild)
                                    │ shipped (or auto)
                                    ▼
                                production ─ smoke fails ─▶ roll production back, revert main and dev, escalate
                                    │ pass
                                    ▼
                                 deployed
```

The UI's start page, **Workflow**, shows this flow as it runs now: each step, its loops
back, the gate settings from `pipeline.yaml`, and the model each agent step uses. Pick a
different model there and it rewrites only that step file's `model:` line (a one-line git
diff), from the list `pipeline.yaml` allows. Steps read their file on every run, so
tickets in flight use the new model from their next run of that step.

Agent steps aren't tied to Claude Code. Each step file names a `runner`: `claude-cli`
(Claude Code), or `cursor-cli` (the Cursor CLI, with GPT, Composer and Grok models). Cursor
runs in its read-only ask mode, so it's offered only for steps that just read (refinement,
spec, retro), and the harness checks its JSON reply against the step's schema because
Cursor can't enforce one. Cursor reports tokens but not cost, so those runs count as $0 in
the stats. Every agent entry on the timeline says which runner and model ran it.

Human decisions: **answer**, **approve** (a spec or a merge), **revise** (a spec), **retry** (with guidance), **take over** (you
push to the branch, then **resume**, and your work goes through CI, review and the risk
gate too), **abandon**. An action that doesn't fit the current wait is rejected with a
reason.

## What's real

| Step | |
|---|---|
| Refinement | Called readiness in ai-workflow. Claude Code agent (`steps/refinement.md`), read-only (Read/Glob/Grep) in a checkout of main |
| Spec | Claude Code agent (`steps/spec.md`), read-only in a checkout of main. Returns the spec as structured output; the harness renders it and commits it with the change |
| Build | Claude Code agent (`steps/build.md`): edit tools plus `go test/vet/build/run` and `gofmt` only. The harness commits and pushes |
| CI | The harness runs gofmt, vet and tests on the pushed commit itself, whatever the agent says it ran |
| Risk gate | **Deterministic floor from `pipeline.yaml`:** paths (agent rules, dependencies, the CLI the smoke test uses) and diff size. Changing a file the spec didn't plan makes it at least medium |
| Merge, deploy, smoke, revert | Local git and go against the stand-in repo, with a deploy folder each for dev and production |
| Production gate | `production.approval` in `pipeline.yaml`: manual (the default) or auto. Production gets the exact commit that passed on dev, and a failure there rolls it back to what it ran before. The **Releases** page lists what's on dev and not in production, in merge order. Shipping one ships everything merged before it, so it's refused until all of that has passed on dev, and they're all marked released together |
| Review | Stub: approves unless the ticket says `never-approve` |

## Sources: where tickets come from

Besides the New ticket button, tickets can come from other systems through a webhook at
`POST /api/sources/<name>`, configured under `sources:` in `pipeline.yaml`. Two adapters
exist: **sentry** (Sentry's issue and alert webhooks) and **webhook** (anything that can
send `{"id", "title", "description", "url"}`).

Work comes in two kinds. **Planned** work is a person asking for something. **Reactive**
work is a system noticing something, like an error in production, and it closes a loop:
the source that raised the problem is the one that shows whether the fix held. A machine
can raise a lot of tickets, so the guardrails are in code:

- A repeat report of a problem with an open ticket is added to that ticket ("reported 14
  times") instead of opening another. That check and the create are one transaction, so
  reports arriving together can't both open a ticket.
- Each source has at most `max_open` tickets open. More reports are dropped, and the
  sender gets a 429.
- A problem that comes back after its fix was deployed opens a new ticket that names the
  fix that didn't hold, so the agents look at that change first.
- A fix goes through the same gates as any change: a risk-assessed merge, then dev, then
  the production gate. With production approval manual, an automatic fix reaches dev
  by itself and production only when someone ships it.
- With the source's signing secret set (`secret_env`), unsigned or wrongly signed reports
  are refused.

`scripts/fake-sentry.sh` sends a Sentry-shaped report about the poster, so you can watch
an error become a ticket, a spec, a fix and a deploy without a Sentry account.

## Specs

Once refinement says a ticket is ready, a spec agent writes a short brief: the goal, what's
in and out of scope, the decisions behind it (the refinement step's calls and your
answers included), the files it expects to change, and how to tell it's done. It exists
because decisions made on the way to a change were getting lost: answers appended to the
description, assumptions in the database, the build's reasoning nowhere.

- **It's committed with the change** as `specs/<ticket>.md`, one per change and never
  updated afterwards. A record of why, next to the what. The harness writes the file
  after the build runs, so the committed spec is the one that was written and approved.
- **The build works from it.** The spec is the brief and the ticket is reference, so the
  build needs to explore less.
- **You can check it before anything is built.** The planned files are run through the
  same risk rules as a real diff. If they hit a medium or high rule, the ticket waits for
  you to approve the spec or ask for changes. `spec.approval` in `pipeline.yaml` can make
  that `always` or `never`. Out of scope is where a misreading shows up: "Add a blue theme"
  would have said "doesn't switch the poster to it" before any code existed.
- **The build is checked against it.** The risk gate compares the real diff with the
  planned files, and anything extra needs a human. The spec file itself never counts
  towards risk.
- **Retros can tell the difference** between a wrong spec and a build that strayed from a
  right one, and can propose edits to the spec prompt too.

It takes the structure of spec-driven tools like Spec Kit (a spec per change, in the repo)
without their commands or long templates, and scales the ceremony by risk.

## Improving the process: retros

After each ticket ends (deployed or abandoned), a **retro** agent reads its timeline and
the current refinement, spec and build prompts, and may propose up to two edits, with the
evidence. Proposals wait under **Improvements** in the sidebar, with the diff and
**Apply** or **Dismiss**. Applying one edits the step file, so it shows up as a normal git
change to review and commit.

The guardrails are code, not instructions to the retro:
- Only the prompt text of `refinement`, `spec` and `build` can change. Never a step's frontmatter
  (model, tools, permissions, budget), never `pipeline.yaml`, and never the retro's own
  prompt.
- The text being replaced must appear exactly once, and the edited step must still load.
  Otherwise the proposal is discarded and the timeline says why.
- A proposal is re-checked when applied. If the file changed in between, it's marked
  stale rather than applied blindly.
- A failing retro is noted on the timeline and never changes the ticket's outcome.

**Did it do what you wanted?** A finished ticket asks, optionally, with Yes or No and a
comment. Your answer reruns the retro with it as the strongest evidence. Without it, the
loop can only learn from process signals (questions, rounds, CI). A ticket can pass every
gate and still miss the point: "Add a blue theme" added the theme but didn't switch the
poster to it, and the retro called it a smooth ticket.

**Decisions made for you.** Refinement and build list the calls they made where the ticket
left something open. These show in their own box on the ticket, not buried in a summary.
Refinement is told to ask, not assume, when a choice changes what the author will see or get.

The Improvements page shows questions asked per ticket, build rounds and agent spend, so
you can see whether an applied change helped. The retro is told that most tickets need no
change. On its first real run (a vague "make it orange" ticket), it judged three questions
reasonable and proposed nothing.

## Design

- **One program, state in SQLite.** Each ticket is a JSON document. The engine runs one
  step, then saves, so a crash loses at most the step in progress. On restart, tickets
  that were mid-step run that step again. That's lighter durability than Temporal's, which
  is fine for a team of 10–20 where runs take minutes and waits are visible in the UI.
- **Gates are code, rules are config.** `pipeline.yaml` holds the round cap and the risk
  floor. The agents never see it, so they can't argue with a gate or edit it.
- **The harness owns the side effects.** Agents edit files. The harness checks out,
  commits, pushes, runs CI, merges and deploys, so every action that matters goes through
  code that logs it.
- **Infrastructure failures aren't agent failures.** If git, the disk or the agent CLI
  fails, the step retries with backoff, then waits for a human as an error. It never uses
  a build round or goes to the builder as feedback.
- **Every step writes events**, which are the ticket's timeline in the UI and its audit
  trail: agent summaries, CI logs, risk reasons, who decided what, cost.

## Its own gates

The harness's code goes through the gates it puts on the code it builds. Every commit
runs `make check` on the staged files (`make hooks` installs the hook; GitHub Actions
runs the same check on push). Its rules, meaning prompts, `pipeline.yaml`, `CLAUDE.md`,
the checks and dependencies, can't be committed straight to main: they go on a branch, a
human approves the diff, and a `--no-ff` merge brings them in. Locally that's a guard
against accidents, since a hook can be skipped. On GitHub it would be branch protection
with a required review.

## Layout

```
cmd/harness        the program: flags, wiring, HTTP server
harness/           engine (steps and gates), store (SQLite), config (risk floor), API, ui/ (embedded)
agent/             step files → prompt → Runner (Claude Code CLI today)
target/            local stand-in for GitHub, CI and deploy
steps/             agent step definitions: model, tools, permissions, budget, schema, prompt
                   (refinement, spec, build, and retro, which proposes edits to the others)
pipeline.yaml      the rules: round cap, spec approval and risk floor
sample/poster      the toy app tickets change
```

## Compared with the Temporal version

| | ai-workflow (Temporal) | this harness |
|---|---|---|
| Moving parts | server, two workers, CLI | one binary |
| State | event history, replayed | a JSON document per ticket, saved per step |
| Long human waits | durable timers, free | a row in SQLite; nudges would be a poll |
| Changing the flow mid-ticket | versioning, replay tests | in-flight tickets just carry on at their step |
| Human interface | signals from the CLI | a UI with validated actions |
| Best for | many tickets, long waits, strict audit | a small team that wants to see and steer everything |

## Next

- A real review agent, with the target's CLAUDE.md as its rubric.
- Close the loop with Sentry itself: resolve the issue when its fix deploys, and link the
  ticket. A Linear source that writes progress back as comments.
- Record the model and prompt version on each agent run, so stats can compare models and
  a retro judges a ticket against the prompt it actually ran with.
- Retros across several tickets: only propose a change when a pattern repeats.
- Stats before and after each applied proposal, to show whether it helped.
- An LLM risk note that can raise the floor but never lower it.
- Gates per step in `pipeline.yaml`, e.g. skip review for docs-only changes, or call out
  to a service or a person before a step runs.
- A direct API harness for one step, to compare with Claude Code as the agent loop.
