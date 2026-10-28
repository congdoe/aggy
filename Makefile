VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/congdoe/aggy/internal/cli.Version=$(VERSION)

# Keep in step with the lint job in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT ?= golangci-lint

# Keep in step with the goreleaser jobs in .github/workflows.
GORELEASER_VERSION := v2.19.0
GORELEASER ?= goreleaser

.PHONY: build test vet lint nocgo noasm lint-install snapshot release-check smoke

build:
	go build -ldflags "$(LDFLAGS)" -o bin/aggy ./cmd/aggy

vet:
	go vet ./...

test: vet
	go test -race ./...

# Keep the targets in step with the lint matrix in .github/workflows/ci.yml.
lint: nocgo noasm
	GOOS=darwin GOARCH=arm64 $(GOLANGCI_LINT) run ./...
	GOOS=darwin GOARCH=amd64 $(GOLANGCI_LINT) run ./...
	GOOS=linux GOARCH=amd64 $(GOLANGCI_LINT) run ./...
	GOOS=linux GOARCH=arm64 $(GOLANGCI_LINT) run ./...
	GOOS=windows GOARCH=amd64 $(GOLANGCI_LINT) run ./...

# cgo reaches the disk and processes around every lint ban, and lint
# cannot see a cgo file when it cross-compiles. go list, with cgo on,
# names every package's cgo files for each OS; any is a failure. Test
# files are not part of CgoFiles.
nocgo:
	@for os in darwin linux windows; do \
		out=$$(CGO_ENABLED=1 GOOS=$$os go list -f '{{if .CgoFiles}}{{.ImportPath}} {{.CgoFiles}}{{end}}' ./...) || exit 1; \
		if [ -n "$$out" ]; then echo "cgo is banned ($$os): $$out"; exit 1; fi; \
	done

# Go assembly and prebuilt objects make raw system calls past every lint
# ban. aggy has none and vendors no code, so any is a failure.
noasm:
	@out=$$(find . -path ./.git -prune -o -type f \( -name '*.s' -o -name '*.S' -o -name '*.syso' \) -print); \
	if [ -n "$$out" ]; then echo "assembly and .syso files are banned:"; echo "$$out"; exit 1; fi

lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

snapshot:
	$(GORELEASER) release --snapshot --clean

release-check:
	$(GORELEASER) check

# Opt-in live checks against the real vendor CLIs; they spend quota. The
# script refuses to run unless AGGY_SMOKE=1. Never part of test or CI.
smoke:
	test/smoke/smoke.sh
