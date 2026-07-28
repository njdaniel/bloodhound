.PHONY: build test vet fmt check clean

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: fmt vet build test
	@echo "ok"

clean:
	rm -rf bin/
