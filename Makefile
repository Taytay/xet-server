.PHONY: help format build test tests clean integration-test integration-tests run all

# Default target
help: ## Show this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

format: ## Format Go code
	go fmt ./...

build: ## Build the xetd server and xet CLI binaries into bin/
	go build -o bin/xetd ./cmd/xetd
	go build -o bin/xet ./cmd/xet

tests: test

test: ## Run unit tests
	go test -v ./...

integration-tests: integration-test

integration-test: build ## Run integration tests (usage: make integration-test TEST=integration-tests/push_pull_roundtrip.sh)
	./integrationTests.sh ./bin/xetd ./bin/xet $(if $(TEST),$(TEST),integration-tests)

run: build ## Run xetd locally on :8420 with data in ./xet-data
	./bin/xetd -addr :8420 -data ./xet-data

clean: ## Clean build artifacts and local server data
	rm -rf bin
	rm -rf xet-data

all: format build test integration-test ## Run format, build, unit tests, and integration tests
	@echo "All tasks completed successfully."
