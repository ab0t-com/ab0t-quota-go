# Convenience targets. Most contributors invoke `go ...` directly; this
# file is for local muscle memory + CI parity.

.PHONY: all test race vet fmt tidy build install dist release clean cover examples lint scan scan-staged scan-history hooks

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.1.0-dev)

all: vet test build

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

build:
	go build ./...

examples:
	go build ./examples/...

# Build + install quotactl into $GOBIN.
install:
	go install ./cmd/quotactl

cover:
	go test -coverprofile=cover.out ./...
	go tool cover -html=cover.out -o cover.html
	@echo "wrote cover.html"

# Cross-compile binaries for distribution.
dist:
	scripts/build.sh $(VERSION)

# Pre-release flow — does NOT push or tag.
release:
	scripts/release.sh $(VERSION)

clean:
	rm -rf release/ cover.out cover.html gitleaks-report.json

# --- Secret scanning + git hooks --------------------------------------

# Install pre-commit hook (idempotent — points git at .githooks/).
hooks:
	scripts/install-hooks.sh

# Full working-tree scan. Safe to run anytime.
scan:
	scripts/scan.sh

# Stage-only scan — what the pre-commit hook runs.
scan-staged:
	scripts/scan.sh --staged

# Full history scan — for own-repo mode (after split from mono-repo).
scan-history:
	scripts/scan.sh --history
