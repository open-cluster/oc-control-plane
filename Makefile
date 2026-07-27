# The CI gates ARE this file: contributors reproduce CI locally with these targets.

STATICCHECK_VERSION := 2025.1.1
GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION := v1.1.4
GOLICENSES_VERSION := v1.6.0

.PHONY: tools lint build test test-short cover verify

tools:
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	go install github.com/google/go-licenses@$(GOLICENSES_VERSION)

# The end-to-end harness is a module of its own so it can depend on what running two real
# implementations takes without those dependencies reaching the shipping binary. A nested
# module is not reached by "./...", so every per-module gate below names it: a gate that
# stops covering part of the repository the moment that part moves still reports green,
# which is worse than not having it.
HARNESS_MODULE := test/e2e

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	cd $(HARNESS_MODULE) && go vet ./...
	staticcheck ./...
	cd $(HARNESS_MODULE) && staticcheck ./...
	golangci-lint run

# -buildvcs=false keeps the build reproducible, which also disables Go's own VCS stamping,
# so the version is supplied explicitly. Without this the binary cannot identify its
# artefact, and "which build is running?" has no answer during an incident.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" ./...

# The race detector requires cgo, so tests run with CGO_ENABLED=1. This is distinct from
# the shipped artefact: `build` stays CGO_ENABLED=0 for a static, reproducible binary.
# Integration tests need a reachable Docker daemon (Testcontainers).
test:
	CGO_ENABLED=1 go test -race -count=1 ./...
	cd $(HARNESS_MODULE) && CGO_ENABLED=1 go test -race -count=1 ./...

# Unit-only run for machines without Docker; the composition-root suite is skipped.
test-short:
	CGO_ENABLED=1 go test -race -short -count=1 ./...

cover:
	go test -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

vuln:
	govulncheck ./...

licenses:
	go-licenses check ./... \
		--allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,MIT,ISC,MPL-2.0 \
		--ignore github.com/open-cluster/oc-control-plane

# Every CI gate a contributor can run locally. Secret scanning is the one exception: it
# runs as a pinned GitHub Action over full history and has no local equivalent here.
verify: lint build test vuln licenses
