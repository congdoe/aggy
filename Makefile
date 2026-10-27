VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/congdoe/aggy/internal/cli.Version=$(VERSION)

.PHONY: build test vet

build:
	go build -ldflags "$(LDFLAGS)" -o bin/aggy ./cmd/aggy

vet:
	go vet ./...

test: vet
	go test -race ./...
