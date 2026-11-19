# Common tasks. Run `make` or `make help` to list them.
.DEFAULT_GOAL := help

GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.9.0
TEST_DATABASE_URL     ?= postgres://chat:chat@localhost:5432/chat_test?sslmode=disable
FUZZTIME              ?= 30s

.PHONY: help run build test test-db cover fmt vet lint jpl vuln tidy check fuzz hooks tools db-up db-down clean

help: ## Show this list
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-9s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

run: ## Run the server (loads .env if it exists)
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; go run ./cmd/server

build: ## Build the server into bin/realtime-chat
	CGO_ENABLED=0 go build -trimpath -o bin/realtime-chat ./cmd/server

test: ## Run all tests with the race detector (database tests skip without TEST_DATABASE_URL)
	go test -race -count=1 ./...

test-db: ## Run all tests, including the database ones (needs `make db-up`)
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -race -count=1 ./...

cover: ## Measure test coverage like CI does (fails below 80%) and open the report
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' ./scripts/coverage.sh coverage.out
	go tool cover -html=coverage.out

fmt: ## Format all Go code (gofmt + goimports)
	golangci-lint fmt ./...

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint with the repository's configuration
	golangci-lint run ./...

jpl: ## Check NASA/JPL rules 1 and 2 (no recursion, no goto, documented loops)
	go run ./cmd/jplcheck ./...

vuln: ## Scan for known vulnerabilities in the code and its dependencies
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

tidy: ## Check that go.mod and go.sum are tidy
	go mod tidy -diff

check: tidy vet lint jpl test ## Run everything CI runs (do this before pushing)

fuzz: ## Run each fuzz test for 30 seconds (or FUZZTIME=5m)
	./scripts/fuzz.sh $(FUZZTIME)

hooks: ## Install the git hooks (needs: pip install pre-commit)
	pre-commit install --install-hooks
	pre-commit install --hook-type pre-push

tools: ## Install golangci-lint
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

db-up: ## Start PostgreSQL in Docker and wait until it's ready
	docker compose up -d --wait

db-down: ## Stop PostgreSQL (keeps the data; `docker compose down -v` deletes it)
	docker compose down

clean: ## Remove build and coverage output
	rm -rf bin dist coverage.out
