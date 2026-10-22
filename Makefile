# brief — read-only MCP server for Kubernetes diagnostics.
#
# No target here mutates a cluster, with one deliberate exception: the eval
# targets, whose whole job is to break a kind cluster in a known way. They are
# named eval-* so the read-only guard in .claude/ can tell them apart from an
# accident.

GO      ?= go
PKG     := ./...
BIN     := bin
CLUSTER ?= brief-eval

.DEFAULT_GOAL := check

## check: vet, test and build — what CI runs
.PHONY: check
check: vet test build

## build: both binaries into bin/
.PHONY: build
build:
	$(GO) build -o $(BIN)/brief-server ./cmd/brief-server
	$(GO) build -o $(BIN)/kubectl-brief ./cmd/kubectl-brief

## test: the whole suite
.PHONY: test
test:
	$(GO) test -race $(PKG)

## test-one: a single test, e.g. make test-one T=TestMarshalTruncates P=./internal/envelope
.PHONY: test-one
test-one:
	@test -n "$(T)" || { echo "usage: make test-one T=TestName [P=./internal/pkg]"; exit 2; }
	$(GO) test -race -run '$(T)' -v $(or $(P),$(PKG))

## cover: coverage report at coverage.html
.PHONY: cover
cover:
	$(GO) test -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "coverage.html written"

## vet: go vet
.PHONY: vet
vet:
	$(GO) vet $(PKG)

## fmt: gofmt in place
.PHONY: fmt
fmt:
	$(GO) fmt $(PKG)

## tidy: sync go.mod and go.sum
.PHONY: tidy
tidy:
	$(GO) mod tidy

## bench-tokens: the v0.1 gate — tokens spent against kubectl, in three scenarios
.PHONY: bench-tokens
bench-tokens:
	$(GO) test -run TestNothing -bench BenchmarkTokens -benchmem ./internal/...

## eval-up: create the kind cluster and apply a fixture, e.g. make eval-up F=oom
.PHONY: eval-up
eval-up:
	hack/eval/up.sh $(or $(F),oom)

## eval-down: destroy the kind cluster
.PHONY: eval-down
eval-down:
	hack/eval/down.sh

## help: list targets
.PHONY: help
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
