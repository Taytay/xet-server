GO_VERSION ?= 1.27
PYTHON_VERSION ?= 3.14

DOCS_PORT ?= 8000

# Optional local overrides (e.g. PIPENV_PYPI_MIRROR for a private index
# mirror); see .env.example. Never committed — .env is gitignored.
ifneq (,$(wildcard .env))
include .env
export
endif

.DEFAULT_GOAL := help

.PHONY: help \
	pre-check \
	install \
	format lint build \
	test tests \
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
	@go version | awk '{print "Go " $$3 " ✓"}'
	@go version | grep -qE 'go(1\.(2[1-9]|[3-9][0-9])|[2-9])' || \
		echo "WARNING: go.mod expects Go >= $(GO_VERSION); verify your installed version supports it."
	@command -v bash >/dev/null 2>&1 || { \
		echo "ERROR: bash not found — required by integrationTests.sh"; exit 1; }
	@echo "bash $$(bash --version | head -1 | awk '{print $$4}') ✓"
	@echo "No external Go modules or network access required to build ✓"
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

build: pre-check ## Build the xetd server and xet CLI binaries into bin/
	go build -o bin/xetd ./cmd/xetd
	go build -o bin/xet-proxyd ./cmd/xet-proxyd
	go build -o bin/xet ./cmd/xet

tests: test

test: pre-check ## Run unit tests
	go test -v ./...

integration-tests: integration-test

integration-test: build ## Run integration tests (usage: make integration-test TEST=integration-tests/push_pull_roundtrip.sh)
	@PYTHON_VERSION=$(PYTHON_VERSION) ./integrationTests.sh ./bin/xetd ./bin/xet ./bin/xet-proxyd $(if $(TEST),$(TEST),integration-tests)

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
