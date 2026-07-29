.PHONY: build test test-integration vet fmt check clean

build:
	go build ./...

test:
	go test ./...

# Real-Prometheus integration tests: a Prometheus container scrapes a
# test-owned /metrics endpoint and the real mcp-prom binary is driven against
# it. Needs only Docker and Go — no API key, no paid calls. Without Docker the
# tests skip, which is why `check` does not depend on this target.
# `vet` runs with the tag too, since plain `make check` never sees these files.
test-integration:
	go vet -tags integration ./...
	go test -tags integration -count=1 -timeout 15m ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: fmt vet build test
	@echo "ok"

clean:
	rm -rf bin/
