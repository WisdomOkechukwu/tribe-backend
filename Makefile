# Tribe backend tasks (like `composer run` scripts). Run `make help` to list them.
#
# The MAMP MySQL server runs out of file handles when several test packages
# each create a ~100-table database at once, so tests run one package at a
# time (TEST_P=1). On a dedicated MySQL you can raise it: make test TEST_P=4

TEST_P ?= 1
GO_ENV := GOFLAGS=-mod=mod

.PHONY: help build migrate rollback fresh seed dev server worker scheduler test test-unit vet fmt try verify snapshot routes loadtest-seed loadtest

help: ## Show this list
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build the four programs into bin/
	$(GO_ENV) go build -o bin/ ./cmd/...

migrate: ## Create the tribe_core database if needed and run new migrations
	$(GO_ENV) go run ./cmd/artisan migrate

rollback: ## Undo the last migration
	$(GO_ENV) go run ./cmd/artisan migrate:rollback

fresh: ## Drop every table, migrate again and seed (local only)
	$(GO_ENV) go run ./cmd/artisan migrate:fresh
	$(GO_ENV) go run ./cmd/artisan db:seed

seed: ## Fill lookup tables, demo customers and admins
	$(GO_ENV) go run ./cmd/artisan db:seed

dev: build ## Run server + worker + scheduler together (Ctrl+C stops all three)
	@trap 'kill 0' INT TERM; ./bin/server & ./bin/worker & ./bin/scheduler & wait

server: build ## Run only the HTTP API
	./bin/server

worker: build ## Run only the queue worker
	./bin/worker

scheduler: build ## Run only cron + the rate collector + the push outbox
	./bin/scheduler

test: ## Run every test (needs MySQL and Redis)
	$(GO_ENV) go test -p $(TEST_P) -count=1 -timeout 20m ./...

test-unit: ## Run only the tests that need no database
	SKIP_DATABASE_TESTS=1 $(GO_ENV) go test -count=1 ./...

vet: ## Static checks
	$(GO_ENV) go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "These files need gofmt:"; gofmt -l .; exit 1)

fmt: ## Format every Go file
	gofmt -w .

try: ## Guided curl tour of the API (server, worker and scheduler must be running)
	./scripts/try-the-api.sh

verify: ## Check every journal entry balances and every snapshot matches its legs
	$(GO_ENV) go run ./cmd/artisan ledger:verify

snapshot: ## Write daily snapshots (make snapshot DATE=2026-10-23)
	$(GO_ENV) go run ./cmd/artisan ledger:snapshot $(DATE)

routes: ## List every route
	$(GO_ENV) go run ./cmd/artisan route:list

loadtest-seed: ## DISPOSABLE servers only: funded load-test customers (make loadtest-seed USERS=200)
	$(GO_ENV) go run ./cmd/artisan loadtest:seed $(or $(USERS),200)

loadtest: ## Load-test a DISPOSABLE deployment (make loadtest URL=http://IP:8080 DURATION=2m); see docs/LOAD_TESTING.md
	$(GO_ENV) go run ./cmd/loadtest -url $(or $(URL),http://localhost:8080) -users $(or $(USERS),200) -concurrency $(or $(CONCURRENCY),50) -duration $(or $(DURATION),1m)
