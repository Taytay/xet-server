GO_VERSION ?= 1.27
PYTHON_VERSION ?= 3.14

DOCS_PORT ?= 8000

# Optional local overrides (e.g. PIPENV_PYPI_MIRROR for a private index
# mirror); see .env.example. Never committed - .env is gitignored.
ifneq (,$(wildcard .env))
include .env
export
endif

.DEFAULT_GOAL := help

.PHONY: help \
	pre-check \
	install \
	format lint build \
	test tests test-race test-fuzz fuzz-seeds \
	integration-test integration-tests \
	docs docs-serve \
	run run-proxy clean \
	all

help: ## Show this help message
	@echo "Available targets:"
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-24s %s\n", $$1, $$2}'

pre-check: ## Verify Go toolchain and required shell tools are present
	@echo "-------------- Running pre-checks --------------"
	@command -v go >/dev/null 2>&1 || { \
		echo "ERROR: go not found. Install from https://go.dev/dl/ (need >= $(GO_VERSION))"; exit 1; }
	@go version | awk '{print "Go " $$3 " [OK]"}'
	@go version | grep -qE 'go(1\.(2[1-9]|[3-9][0-9])|[2-9])' || \
		echo "WARNING: go.mod expects Go >= $(GO_VERSION); verify your installed version supports it."
	@command -v bash >/dev/null 2>&1 || { \
		echo "ERROR: bash not found - required by integrationTests.sh"; exit 1; }
	@echo "bash $$(bash --version | head -1 | awk '{print $$4}') [OK]"
	@echo "No external Go modules or network access required to build [OK]"
	@cd scripts && go run build_docs.go -check-mermaid

install: ## Install pipenv Python deps (huggingface_hub/hf_xet, for the real hf CLI integration test)
	@command -v pipenv >/dev/null 2>&1 || { \
		echo "ERROR: pipenv not found. Install from https://pipenv.pypa.io/"; exit 1; }
	@pipenv --python $(PYTHON_VERSION)
	@pipenv install --dev

format: pre-check ## Run Formatter on Packages
	go fmt ./...

vet: pre-check
	go vet ./...

lint: vet ## Run Linter on Packages

# EXE is ".exe" on Windows and empty everywhere else. `go build -o bin/xetd`
# on Windows silently writes an extension-less `bin/xetd`, then the same
# `./bin/xetd` invocation used elsewhere (integrationTests.sh, `make run`,
# `make integration-test`) fails to find it because the OS only resolves
# `bin/xetd.exe`. Naming the output binaries with .exe explicitly on
# Windows keeps every downstream path resolving the same file. Detected
# from Go's own idea of the host OS so a cross-build (GOOS=... make build)
# still names its output correctly for that target.
EXE := $(shell go env GOEXE)

build: pre-check ## Build the xetd server and xet CLI binaries into bin/
	go build -o bin/xetd$(EXE) ./cmd/xetd
	go build -o bin/xet-proxyd$(EXE) ./cmd/xet-proxyd
	go build -o bin/xet$(EXE) ./cmd/xet

tests: test

test: pre-check ## Run unit tests
	go test -v ./...

test-race: pre-check ## Run unit tests under the race detector
	go test -race ./...

# How long each individual fuzz target runs under `make test-fuzz`. Kept
# short by default so the target is usable as a routine pre-push check;
# raise it (make test-fuzz FUZZTIME=5m) for a real soak. Note the race
# detector and the fuzzer are deliberately separate targets: -race slows
# execution by roughly an order of magnitude, which for a time-boxed fuzz
# run means far fewer inputs explored.
FUZZTIME ?= 30s

# fuzz-seeds first: replaying the committed corpus is fast and
# deterministic, and every previously-found crash lives there as a
# regression case. If one of those has come back there is no point
# spending FUZZTIME per target hunting for new inputs - fail immediately
# on the known one instead.
test-fuzz: fuzz-seeds ## Fuzz every target for $(FUZZTIME) each, after replaying the corpus (override: FUZZTIME=5m)
	@set -e; \
	total=0; \
	for pkg in $$(go list ./...); do \
	    targets=$$(go test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz' || true); \
	    for target in $$targets; do \
	        total=$$((total + 1)); \
	        echo "--- $$pkg :: $$target ($(FUZZTIME)) ---"; \
	        go test $$pkg -run "^$$target$$" -fuzz "^$$target$$" -fuzztime $(FUZZTIME); \
	    done; \
	done; \
	echo "fuzzed $$total target(s) for $(FUZZTIME) each"

fuzz-seeds: pre-check ## Replay every fuzz target's committed corpus (fast; includes past crash regressions)
	go test -run '^Fuzz' ./...

integration-tests: integration-test

integration-test: build ## Run integration tests (usage: make integration-test TEST=integration-tests/push_pull_roundtrip.sh)
	@PYTHON_VERSION=$(PYTHON_VERSION) ./integrationTests.sh ./bin/xetd$(EXE) ./bin/xet$(EXE) ./bin/xet-proxyd$(EXE) $(if $(TEST),$(TEST),integration-tests)

run: build ## Run xetd locally on :8420 with data in ./xet-data
	./bin/xetd -addr :8420 -data ./xet-data

run-proxy: build ## Run xet-proxyd locally on :8420 (CAS) / :8421 (Hub), relaying to real huggingface.co, with data in ./xet-proxy-data
	./bin/xet-proxyd -addr :8420 -hub-addr :8421 -data ./xet-proxy-data

docs: pre-check ## Regenerate docs/godoc/*.md and render all docs (README, CONTRIBUTING, CHANGELOG, docs/*.md) to browsable HTML in docs/build/
	cd scripts && go run build_docs.go

docs-serve: pre-check ## Build docs and serve docs/build/ locally for browsing
	@(command -v open >/dev/null 2>&1 && sleep 1 && open "http://localhost:$(DOCS_PORT)/") & \
	cd scripts && go run build_docs.go -serve ":$(DOCS_PORT)"

clean: ## Clean build artifacts, local server data, generated docs HTML, and the pipenv virtualenv
	rm -rf bin
	rm -rf xet-data
	rm -rf xet-proxy-data
	rm -rf docs/build
	@pipenv --venv >/dev/null 2>&1 && pipenv --rm || true

all: format vet build test integration-test ## Run format, vet, build, unit tests, and integration tests
	@echo "All tasks completed successfully."
