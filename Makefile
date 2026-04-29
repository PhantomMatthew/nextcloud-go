# nextcloud-go — Makefile
#
# Common developer entry points. CI mirrors a subset of these targets.
# All targets are .PHONY; this is not a build artifact tracker.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

GO          ?= go
GOFLAGS     ?=
PKG         := ./...
BIN_DIR     := bin
LDFLAGS     := -s -w \
	-X 'github.com/PhantomMatthew/nextcloud-go/internal/observability.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)' \
	-X 'github.com/PhantomMatthew/nextcloud-go/internal/observability.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo none)' \
	-X 'github.com/PhantomMatthew/nextcloud-go/internal/observability.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)'

.PHONY: all
all: fmt vet lint test build

.PHONY: build
build: ## Build all binaries into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ncgo         ./cmd/ncgo
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ncgo-cli     ./cmd/ncgo-cli
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ncgo-captest ./cmd/ncgo-captest

.PHONY: build-all
build-all: ## Build for linux/amd64 and linux/arm64
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ncgo-linux-amd64 ./cmd/ncgo
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ncgo-linux-arm64 ./cmd/ncgo

.PHONY: run
run: ## Run ncgo locally
	$(GO) run ./cmd/ncgo

.PHONY: test
test: ## Run all unit tests
	$(GO) test $(GOFLAGS) -race -count=1 $(PKG)

.PHONY: test-short
test-short:
	$(GO) test $(GOFLAGS) -short -count=1 $(PKG)

.PHONY: cover
cover: ## Generate coverage report (HTML at coverage.html)
	$(GO) test $(GOFLAGS) -race -covermode=atomic -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: bench
bench:
	$(GO) test $(GOFLAGS) -run=^$$ -bench=. -benchmem $(PKG)

.PHONY: fmt
fmt: ## gofmt -s -w
	$(GO) fmt $(PKG)
	@which goimports >/dev/null 2>&1 && goimports -w -local github.com/PhantomMatthew/nextcloud-go . || true

.PHONY: vet
vet:
	$(GO) vet $(PKG)

.PHONY: lint
lint: ## Run golangci-lint
	@which golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed (https://golangci-lint.run/usage/install/)"; exit 1; }
	golangci-lint run --timeout=5m

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: verify
verify: ## go mod verify + tidy diff check
	$(GO) mod verify
	$(GO) mod tidy
	@git diff --exit-code -- go.mod go.sum || { echo "go.mod / go.sum out of sync; run 'make tidy'"; exit 1; }

.PHONY: docker
docker: ## Build production OCI image
	docker build -t ncgo:dev .

.PHONY: dev-up
dev-up: ## Start dev stack (postgres + redis + ncgo)
	docker compose -f deploy/docker/docker-compose.dev.yml up --build

.PHONY: dev-down
dev-down:
	docker compose -f deploy/docker/docker-compose.dev.yml down -v

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html dist/

.PHONY: help
help:
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
