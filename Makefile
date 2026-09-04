GO_VERSION ?= 1.21

.DEFAULT_GOAL := help

.PHONY: help \
	pre-check \
	format vet build \
	test tests \
	integration-test integration-tests \
	run clean \
	all

help: ## Show this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-24s %s\n", $$1, $$2}'

pre-check: ## Verify Go toolchain and required shell tools are present
	@echo "-------------- Running pre-checks --------------"
	@command -v go >/dev/null 2>&1 || { \
		echo "ERROR: go not found. Install from https://go.dev/dl/ (need >= $(GO_VERSION))"; exit 1; }
	@go version | awk '{print "Go " $$3 " ✓"}'
	@go version | grep -qE 'go(1\.(2[1-9]|[3-9][0-9])|[2-9])' || \
		echo "WARNING: go.mod expects Go >= $(GO_VERSION); verify your installed version supports it."
	@command -v bash >/dev/null 2>&1 || { \
		echo "ERROR: bash not found — required by integrationTests.sh"; exit 1; }
	@echo "bash $$(bash --version | head -1 | awk '{print $$4}') ✓"
	@echo "No external Go modules or network access required to build ✓"

format: pre-check ## Format Go code
	go fmt ./...

vet: pre-check ## Run go vet across all packages
	go vet ./...

build: pre-check ## Build the xetd server and xet CLI binaries into bin/
	go build -o bin/xetd ./cmd/xetd
	go build -o bin/xet ./cmd/xet

tests: test

test: pre-check ## Run unit tests
	go test -v ./...

integration-tests: integration-test

integration-test: build ## Run integration tests (usage: make integration-test TEST=integration-tests/push_pull_roundtrip.sh)
	./integrationTests.sh ./bin/xetd ./bin/xet $(if $(TEST),$(TEST),integration-tests)

run: build ## Run xetd locally on :8420 with data in ./xet-data
	./bin/xetd -addr :8420 -data ./xet-data

clean: ## Clean build artifacts and local server data
	rm -rf bin
	rm -rf xet-data

all: format vet build test integration-test ## Run format, vet, build, unit tests, and integration tests
	@echo "All tasks completed successfully."
