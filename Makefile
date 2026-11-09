VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# built with the project's Go toolchain, so it understands the go version in go.mod
GOLANGCI := GOTOOLCHAIN=$(shell go env GOVERSION) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0

.PHONY: build test lint fmt e2e snapshot clean

build: ## build bin/sb2pb
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/sb2pb ./cmd/sb2pb

test: lint ## vet, lint and unit tests
	go vet ./...
	go test -race ./...

lint:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; echo "run: make fmt"; exit 1)
	$(GOLANGCI) run

fmt:
	gofmt -w cmd internal

e2e: ## end-to-end suite (needs Docker, Python 3)
	e2e/run.sh

snapshot: ## local release build into dist/ (no publishing)
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist e2e/.work
