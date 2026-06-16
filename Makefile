# Library — root Makefile. Drives the Go build/test and the containerized stack.
#
# The container stack lives in docker/ (compose file + Dockerfiles). We use
# `docker compose` because on this host it talks to Podman through the
# Docker-compatible socket; `podman compose` delegates to Docker Desktop's
# plugin and fails. Override with COMPOSE=... if your setup differs.

COMPOSE      ?= docker compose
COMPOSE_FILE := docker/docker-compose.yml
DC           := $(COMPOSE) -f $(COMPOSE_FILE)

# LAN IP baked into OPDS links so the Xteink X4 can fetch them. Auto-detected;
# override with `make up LIBRARY_BASE_URL=http://host:8080`.
LANIP             ?= $(shell ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || echo 127.0.0.1)
LIBRARY_BASE_URL  ?= http://$(LANIP):8080

.DEFAULT_GOAL := help

## ---- Go ----

.PHONY: build
build: ## Build the Go binary to ./bin/library
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/library ./cmd/library

.PHONY: test
test: ## Run Go tests
	go test ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: lint-md
lint-md: ## Lint markdown docs (needs: npm i -g markdownlint-cli)
	@command -v markdownlint >/dev/null 2>&1 || { echo "markdownlint not found: npm install -g markdownlint-cli"; exit 1; }
	markdownlint *.md docs/*.md

.PHONY: run
run: ## Run the server locally on the host (needs a running sidecar for imports)
	go run ./cmd/library -data ./data -base-url $(LIBRARY_BASE_URL)

## ---- Podman VM clock ----
#
# The Podman machine VM's clock drifts when the Mac sleeps. Fedora CoreOS ships
# chrony with `makestep 1.0 3`, which steps the clock only for the first 3
# updates — so after a big sleep-induced jump it refuses to correct, leaving the
# container minutes behind real time. Adobe then rejects ADEPT fulfillment with
# E_ADEPT_REQUEST_EXPIRED (the signed request looks stale). This target installs
# a drop-in that lets chrony ALWAYS step on a large offset, then forces a sync.
# Idempotent; safe to re-run. Persists across `podman machine stop/start` (but
# not across `podman machine rm`, so it's wired into `up` as a guard).

.PHONY: time-sync
time-sync: ## Fix + resync the Podman VM clock (prevents ADEPT "request expired")
	@echo "makestep 1.0 -1" | podman machine ssh 'sudo tee /etc/chrony.d/99-always-step.conf >/dev/null'
	@podman machine ssh 'sudo systemctl restart chronyd && sleep 2 && sudo chronyc makestep >/dev/null && chronyc tracking | grep -E "Stratum|System time"'
	@HOST=$$(date -u +%s); VM=$$(podman machine ssh date -u +%s 2>/dev/null); \
		echo "host/VM clock skew: $$((HOST-VM))s (should be ~0)"

.PHONY: check-clock
check-clock: ## Warn if the Podman VM clock is skewed (used as an `up` guard)
	@HOST=$$(date -u +%s); VM=$$(podman machine ssh date -u +%s 2>/dev/null || echo $$HOST); \
		SKEW=$$((HOST-VM)); SKEW=$${SKEW#-}; \
		if [ $$SKEW -gt 5 ]; then \
			echo "WARNING: Podman VM clock is $$SKEW s off — ADEPT fulfillment will fail."; \
			echo "         Run 'make time-sync' to fix it."; \
		fi

## ---- Container stack ----

.PHONY: images
images: ## Build both container images
	$(DC) build

.PHONY: up
up: check-clock ## Build (if needed) and start the stack in the background
	LIBRARY_BASE_URL=$(LIBRARY_BASE_URL) $(DC) up -d --build

.PHONY: down
down: ## Stop and remove the stack
	$(DC) down

.PHONY: logs
logs: ## Follow stack logs
	$(DC) logs -f

.PHONY: ps
ps: ## Show stack status
	$(DC) ps

.PHONY: restart
restart: down up ## Restart the stack

## ---- DRM setup ----

.PHONY: drm-setup
drm-setup: ## One-time: authorize Adobe + export key into ./secrets (interactive)
	$(DC) build drm-sidecar
	$(COMPOSE) -f $(COMPOSE_FILE) run --rm --userns=keep-id drm-sidecar python /opt/setup.py

## ---- Housekeeping ----

.PHONY: clean
clean: ## Remove build artifacts (keeps data/, secrets/)
	rm -rf bin

.PHONY: clean-db
clean-db: ## Delete the catalog DB (forces a full rescan on next start)
	rm -f data/catalog.db data/catalog.db-wal data/catalog.db-shm

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
