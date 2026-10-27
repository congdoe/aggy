VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/congdoe/aggy/internal/cli.Version=$(VERSION)

# Keep in step with the lint job in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT ?= golangci-lint

.PHONY: build test vet lint lint-install

build:
	go build -ldflags "$(LDFLAGS)" -o bin/aggy ./cmd/aggy

vet:
	go vet ./...

test: vet
	go test -race ./...

lint:
	$(GOLANGCI_LINT) run ./...

lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
