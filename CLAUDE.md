# ai-workflow-harness: rules for agents

Read README.md first.

- Commits run `make check` on the staged files (`make hooks` installs the hook). Never
  skip it with `--no-verify`: if it fails, fix the cause.
- Run `make check` before you finish and make it pass (lint, govulncheck, race tests,
  coverage). `make fmt` fixes formatting. Never weaken a gate: don't edit `.golangci.yml`,
  `Makefile`, `tools/go.mod` or `pipeline.yaml` unless that's the task.
- Every side effect (git, CI, deploy, agent runs) goes through `harness.Repo` or
  `agent.Runner`, so it can be faked in tests and logged as an event.
- A step must be safe to run twice: after a crash, the step in progress runs again.
- Infrastructure errors are returned as errors (retried, then a human). Failures in the
  code being built are results that go back to the builder. Don't mix them up.
- Human actions are validated against the ticket's current wait; add new ones to the
  `actions` table in `harness/ticket.go`.
- Wrap errors with context; compare with `errors.Is`. Tests use the real step files and
  `pipeline.yaml` with fakes for the agent and repo.
