# `make check` is the single gate: lint, vulnerabilities, race tests and a
# coverage minimum. Tool versions are pinned in tools/go.mod.

TOOL := go tool -modfile=tools/go.mod
COVERAGE_MIN := 70
# Domain code only: cmd/ is a thin main that wires things up.
COVERAGE_PKGS := ./harness/...,./agent/...,./target/...

.PHONY: check fmt lint vuln test coverage run hooks

check: lint vuln test coverage

fmt:
	$(TOOL) golangci-lint fmt ./...

lint:
	$(TOOL) golangci-lint run ./...

vuln:
	$(TOOL) govulncheck ./...

test:
	go test -race -coverpkg=$(COVERAGE_PKGS) -coverprofile=coverage.out ./...

coverage: test
	@go tool cover -func=coverage.out | awk -v min=$(COVERAGE_MIN) \
		'/^total:/ { sub("%","",$$3); printf "coverage %s%% (min %s%%)\n", $$3, min; if ($$3+0 < min) { print "FAIL: coverage below minimum"; exit 1 } }'

run:
	go run ./cmd/harness

# Runs `make check` on the staged tree before every commit (.githooks/pre-commit).
hooks:
	git config core.hooksPath .githooks
