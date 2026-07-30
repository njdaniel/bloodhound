.PHONY: build test test-integration vet fmt lint check clean

# Keep in sync with .github/workflows/ci.yml's lint job.
GOLANGCI_VERSION := 2.12.2

# golangci-lint's cache is keyed by import path, not by directory, so the
# default shared cache under ~/.cache serves one worktree's results to
# another — every agent worktree under .claude/worktrees/ has the same import
# paths (issue #25). Keeping the cache inside the checkout gives each worktree
# its own, so a run only ever reports files it can actually see. `?=` so
# `GOLANGCI_LINT_CACHE=... make lint` still wins, and CI (fresh checkout, cold
# cache) is unaffected either way.
GOLANGCI_LINT_CACHE ?= $(CURDIR)/.golangci-cache
export GOLANGCI_LINT_CACHE

build:
	go build ./...

# -race everywhere: the suite runs in seconds, and the concurrency that matters
# (the mcpclient goroutine that ties a server's lifetime to a context, the
# capture middleware's sequence counter) is exactly what a non-race run misses.
test:
	go test -race ./...

# Real-Prometheus integration tests: a Prometheus container scrapes a
# test-owned /metrics endpoint and the real mcp-prom binary is driven against
# it. Needs only Docker and Go — no API key, no paid calls. Without Docker the
# tests skip, which is why `check` does not depend on this target.
# `vet` runs with the tag too, since plain `make check` never sees these files.
test-integration:
	go vet -tags integration ./...
	go test -race -tags integration -count=1 -timeout 15m ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# golangci-lint is a CI tool, not a module dependency — it stays out of go.mod
# (CLAUDE.md's stdlib-first rule). Version and linter set live in
# .golangci.yml; CI installs the pinned version.
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed; skipping (CI will still run it)."; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_VERSION)"; \
		exit 0; \
	}; \
	golangci-lint run ./...

# lint is a soft dependency: it skips when the tool is absent so `make check`
# works on a clean checkout, but CI runs it unconditionally, so nothing lands
# unlinted either way.
check: fmt vet build test lint
	@echo "ok"

clean:
	rm -rf bin/ .golangci-cache/
