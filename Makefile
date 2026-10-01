# AI Cover Letter Generator — development tasks.
#
# Every target is safe to run repeatedly. Configuration comes from the
# environment; see .env.example for the full list.

BINARY      := bin/server
PKG         := ./...
GO          ?= go
# Go's own -envfile flag loads .env into the process environment. The server
# itself reads plain environment variables only.
RUN_ENV     := $(shell [ -f .env ] && echo "-envfile=.env")

.DEFAULT_GOAL := help

## help: list the available targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## run: start the server (loads .env if present)
.PHONY: run
run:
	$(GO) run $(RUN_ENV) ./cmd/server

## build: compile the server binary into bin/
.PHONY: build
build:
	$(GO) build -o $(BINARY) ./cmd/server

## test: run the full test suite
.PHONY: test
test:
	$(GO) test -race $(PKG)

## test-short: run the test suite without the race detector
.PHONY: test-short
test-short:
	$(GO) test $(PKG)

## cover: run the tests and report coverage for the internal packages
.PHONY: cover
cover:
	$(GO) test -coverprofile=coverage.out -coverpkg=./internal/... $(PKG)
	$(GO) tool cover -func=coverage.out | tail -1

## fmt: format all Go source
.PHONY: fmt
fmt:
	$(GO) fmt $(PKG)

## vet: run go vet
.PHONY: vet
vet:
	$(GO) vet $(PKG)

## check: format check plus vet, suitable for pre-commit
.PHONY: check
check: vet
	@unformatted=$$(gofmt -l cmd internal migrations tests); \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "vet and gofmt clean"

## tidy: prune and verify module requirements
.PHONY: tidy
tidy:
	$(GO) mod tidy

## clean: remove build output, test artifacts and local data
.PHONY: clean
clean:
	rm -rf bin coverage.out coverage.html
	rm -rf data
	@echo "cleaned build output and local data"

## check-tools: confirm cgo is enabled, which the SQLite driver requires
.PHONY: check-tools
check-tools:
	@$(GO) env CGO_ENABLED | grep -q '^1$$' \
		&& echo "CGO_ENABLED=1 (SQLite driver will build)" \
		|| { echo "CGO_ENABLED=0 — the SQLite driver needs cgo; install a C toolchain"; exit 1; }