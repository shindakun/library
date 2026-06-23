# Library root Makefile. Drives the Go build/test and the containerized stack.
#
# The container stack lives in docker/ (compose file + Dockerfiles). We use
# `docker compose` because on this host it talks to Podman through the
# Docker-compatible socket; `podman compose` delegates to Docker Desktop's
# plugin and fails. Override with COMPOSE=... if your setup differs.

COMPOSE      ?= docker compose
COMPOSE_FILE := docker/docker-compose.yml
DC           := $(COMPOSE) -f $(COMPOSE_FILE)

# Production stack (Proxmox: pulls prebuilt images from GHCR, no local build).
PROD_FILE    := docker/docker-compose.prod.yml
DC_PROD      := $(COMPOSE) -f $(PROD_FILE)

# LAN IP baked into OPDS links so the Xteink X4 can fetch them. Auto-detected;
# override with `make up LIBRARY_BASE_URL=http://host:8080`.
LANIP             ?= $(shell ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || echo 127.0.0.1)
LIBRARY_BASE_URL  ?= http://$(LANIP):8080

.DEFAULT_GOAL := help

## ---- Go ----

# Version stamped into the binary: the exact tag if HEAD is tagged, else the
# nearest tag + commits + short SHA (e.g. v0.1.0-3-gabc1234), else "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build
build: ## Build the Go binary to ./bin/library (version-stamped)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/library ./cmd/library

.PHONY: version
version: ## Print the version that would be stamped
	@echo $(VERSION)

.PHONY: test
test: ## Run Go tests
	go test ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: lint
lint: ## Run golangci-lint (needs: brew install golangci-lint)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found: brew install golangci-lint"; exit 1; }
	golangci-lint run ./...

.PHONY: lint-md
lint-md: ## Lint markdown docs (needs: npm i -g markdownlint-cli)
	@command -v markdownlint >/dev/null 2>&1 || { echo "markdownlint not found: npm install -g markdownlint-cli"; exit 1; }
	markdownlint '*.md' 'docs/**/*.md'

.PHONY: check
check: vet lint test ## Run all Go checks (vet + lint + test); mirrors CI

.PHONY: hooks
hooks: ## Install the repo git hooks (gofmt/vet/lint/markdownlint on commit)
	git config core.hooksPath .githooks
	@echo "git hooks installed (core.hooksPath = .githooks)"

.PHONY: run
run: ## Run the server locally on the host (needs a running sidecar for imports)
	go run ./cmd/library -data ./data -base-url $(LIBRARY_BASE_URL)

## ---- Podman VM clock ----
#
# The Podman machine VM's clock drifts when the Mac sleeps. Fedora CoreOS ships
# chrony with `makestep 1.0 3`, which steps the clock only for the first 3
# updates, so after a big sleep-induced jump it refuses to correct, leaving the
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
			echo "WARNING: Podman VM clock is $$SKEW s off; ADEPT fulfillment will fail."; \
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

## ---- Release ----
# Cut a release by pushing a vX.Y.Z tag; CI (.github/workflows/release.yml) then
# builds multi-arch images to GHCR and creates the GitHub Release. Requires the
# working tree to be clean and on main, and that main is pushed.

.PHONY: release
release: ## Tag and push a release: make release VERSION=v0.1.0
	@echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "VERSION must be vX.Y.Z, got '$(VERSION)'"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree not clean; commit first"; exit 1; }
	@git rev-parse "$(VERSION)" >/dev/null 2>&1 && { echo "tag $(VERSION) already exists"; exit 1; } || true
	@echo "Running checks before tagging..."
	@$(MAKE) --no-print-directory check
	git tag -a "$(VERSION)" -m "$(VERSION)"
	git push origin main
	git push origin "$(VERSION)"
	@echo "Pushed $(VERSION). CI will build images to GHCR and cut the GitHub Release."
	@echo "Watch: gh run watch  (or the Actions tab)"

## ---- Production (Proxmox: GHCR images, no local build) ----
# Run these ON the Proxmox guest. Set LIBRARY_BASE_URL to the host LAN address,
# and REGISTRY/TAG if not using the defaults baked into the prod compose file.

.PHONY: prod-pull
prod-pull: ## Pull the latest prod images from GHCR
	$(DC_PROD) pull

.PHONY: prod-up
prod-up: ## Start the prod stack (pulls images, runs detached)
	$(DC_PROD) up -d --pull always

.PHONY: prod-down
prod-down: ## Stop and remove the prod stack
	$(DC_PROD) down

.PHONY: prod-logs
prod-logs: ## Follow prod stack logs
	$(DC_PROD) logs -f

.PHONY: prod-ps
prod-ps: ## Show prod stack status
	$(DC_PROD) ps

.PHONY: prod-deploy
prod-deploy: ## Pull newest images and restart the prod stack in place
	$(DC_PROD) pull
	$(DC_PROD) up -d

## ---- DRM setup ----

.PHONY: drm-setup
drm-setup: ## One-time: authorize Adobe + export key into ./secrets (interactive)
	$(DC) build ebook-sidecar
	$(COMPOSE) -f $(COMPOSE_FILE) run --rm --userns=keep-id ebook-sidecar python /opt/setup.py

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
