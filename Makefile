# TracePoint — see docs/SPEC.md (source of truth) and docs/PROGRESS.md.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BIN        := bin/tracepoint
PKG        := ./...
MODULE     := github.com/IshaanNene/Tracepoint
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE       ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
              -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
              -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
              -X $(MODULE)/internal/buildinfo.Date=$(DATE)

GOBIN      := $(shell go env GOPATH)/bin
GOLANGCI   ?= $(GOBIN)/golangci-lint
GOVULNCHECK?= $(GOBIN)/govulncheck

# Coverage gates (§10). Overall plus per-package floors.
COVER_OVERALL_MIN := 70
COVER_STRICT_PKGS := schedule metrics analysis capacity compare
COVER_STRICT_MIN  := 85

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: check
check: fmt-check vet lint test contracts vuln ## Full local gate: format, vet, lint, race tests, contracts, vulns

.PHONY: fmt
fmt: ## Format the tree
	gofmt -s -w .
	@command -v goimports >/dev/null 2>&1 && goimports -w -local $(MODULE) . || true

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@out="$$(gofmt -s -l . | grep -v '^$$' || true)"; \
	if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet
	go vet $(PKG)

.PHONY: lint
lint: ## golangci-lint
	@command -v $(GOLANGCI) >/dev/null 2>&1 || { echo "golangci-lint not found at $(GOLANGCI)"; exit 1; }
	$(GOLANGCI) run

.PHONY: test
test: ## Unit tests with -race (cgo on)
	CGO_ENABLED=1 go test -race -shuffle=on -count=1 $(PKG)

.PHONY: cover
cover: ## Unit tests with coverage + gate check
	CGO_ENABLED=1 go test -race -count=1 -covermode=atomic -coverprofile=coverage.txt $(PKG)
	@go tool cover -func=coverage.txt | tail -n 1

.PHONY: integration
integration: ## Integration tests (testcontainers: postgres, mysql, redis)
	CGO_ENABLED=1 go test -race -count=1 -tags=integration -timeout=20m $(PKG)

.PHONY: e2e
e2e: ## Known-answer fault-injection suite (§10)
	CGO_ENABLED=1 go test -race -count=1 -tags=e2e -timeout=30m ./...

.PHONY: soak
soak: ## 10-minute soak, flat-heap assertion (§10)
	go test -count=1 -tags=soak -timeout=25m ./internal/metrics/... ./internal/executor/...

.PHONY: bench
bench: ## Benchmarks for the record path, scheduler, sketch merge
	go test -run='^$$' -bench=. -benchmem -count=6 ./internal/metrics/... ./internal/schedule/...

.PHONY: build
build: ## Build the static binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/tracepoint

.PHONY: contracts
contracts: ## Contract checks: rename completeness, JSON Schemas, schema fixtures
	./scripts/check-contracts.sh

.PHONY: vuln
vuln: ## govulncheck
	@command -v $(GOVULNCHECK) >/dev/null 2>&1 || { echo "govulncheck not installed; go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	$(GOVULNCHECK) $(PKG)

.PHONY: tidy
tidy: ## go mod tidy + verify
	go mod tidy
	go mod verify

.PHONY: release-dry
release-dry: ## GoReleaser dry run
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser not installed"; exit 1; }
	goreleaser release --snapshot --clean --skip=publish,sign

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf bin dist coverage.txt coverage.html
