# dispatch — agent orchestration over Slack
#
#   make help          list targets
#
# Variables you may override:  make service-install BIN=/opt/dispatch/dispatch USER_=bot

BIN     ?= /usr/local/bin/dispatch
CONFIG  ?= $(or $(DISPATCH_CONFIG),$(HOME)/.config/dispatch/config.toml)
USER_   ?= $(shell id -un)
GROUP_  ?= $(shell id -gn)
HOME_   ?= $(shell getent passwd $(USER_) | cut -d: -f6)
UNIT    ?= /etc/systemd/system/dispatch.service
GO      ?= go
# BIN and the unit live under root-owned prefixes. Set SUDO= (empty) to install
# into a prefix you already own, e.g. `make install BIN=$$HOME/bin/dispatch SUDO=`.
SUDO    ?= sudo

# auto-update: a root-owned deploy clone, polled by a systemd timer
REPO       ?= $(shell git remote get-url origin)
BRANCH     ?= main
SRC        ?= /opt/dispatch/src
INTERVAL   ?= 5min
UPDATER    ?= /usr/local/lib/dispatch/dispatch-update.sh
UPDATE_UNIT ?= /etc/systemd/system/dispatch-update.service
UPDATE_TIMER ?= /etc/systemd/system/dispatch-update.timer
GO_BIN     ?= $(shell command -v $(GO))
DEPLOY_ENV ?= /etc/dispatch/deploy.env
STATE      ?= /var/lib/dispatch/deployed.sha

.DEFAULT_GOAL := help
.PHONY: help build run run-terminal run-web ui ui-dev setup doctor test test-race test-live e2e restart-drill auto-resume-drill lint fmt tidy clean \
        install uninstall service-install service-uninstall service-restart service-status service-logs \
        update-install update-uninstall update-now update-status update-logs deploy-env \
        docker-build docker-run docker-stop docker-logs

## ---- build -----------------------------------------------------------------

build: ## Build bin/dispatch
	$(GO) build -o bin/dispatch ./cmd/dispatch

clean: ## Remove build output
	rm -rf bin

## ---- run locally -------------------------------------------------------------

run: build ## Run with the configured transports (Slack) — CONFIG=$(CONFIG)
	bin/dispatch run -config $(CONFIG)

run-terminal: build ## Run with the terminal transport instead of Slack
	bin/dispatch run -config $(CONFIG) -terminal

run-web: build ## Run with the web transport (browser UI) instead of Slack
	bin/dispatch run -config $(CONFIG) -web

UI := internal/transport/web/ui
ui: ## Build the web UI (React + HeroUI) into internal/transport/web/static — commit the result
	cd $(UI) && npm install --no-audit --no-fund && npm run build

ui-dev: ## Vite dev server for the web UI, proxying /api to a running `dispatch run -web`
	cd $(UI) && npm install --no-audit --no-fund && npm run dev

setup: build ## Interactive first-time wizard (writes CONFIG, then runs doctor)
	bin/dispatch setup -config $(CONFIG)

doctor: build ## Check config, claude login, docker, ssh hosts, Slack tokens
	bin/dispatch doctor -config $(CONFIG)

## ---- test -------------------------------------------------------------------

test: ## Unit tests (docker/ssh tests use a real daemon / throwaway sshd; skipped if absent)
	$(GO) test ./...

test-race: ## Unit tests with the race detector
	$(GO) test -race -count=1 ./...

test-live: ## Also drive the real claude CLI (haiku, a few cents)
	DISPATCH_LIVE=1 $(GO) test -count=1 ./...

restart-drill: build ## SIGTERM mid-tool-call: drain, notify, restart, resume (needs logged-in claude)
	python3 scripts/restart-drill.py bin/dispatch

auto-resume-drill: build ## SIGTERM mid-tool-call, then check the task finishes itself (needs logged-in claude)
	python3 scripts/auto-resume-drill.py bin/dispatch

e2e: build ## Whole binary through the terminal transport (needs logged-in claude)
	python3 scripts/e2e.py bin/dispatch

lint: ## gofmt check + go vet
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	$(GO) vet ./...

fmt: ## gofmt the tree
	gofmt -w .

tidy: ## go mod tidy
	$(GO) mod tidy

## ---- install as a service -----------------------------------------------------

install: build ## Copy the binary to BIN=$(BIN)   (SUDO= to skip sudo)
	$(SUDO) install -d $(dir $(BIN))
	$(SUDO) install -m 0755 bin/dispatch $(BIN)

uninstall: ## Remove the binary
	-$(SUDO) rm -f $(BIN)

service-install: install deploy-env ## Install + enable + start the systemd unit for USER_=$(USER_)
	sed -e 's|__USER__|$(USER_)|g' -e 's|__GROUP__|$(GROUP_)|g' -e 's|__HOME__|$(HOME_)|g' -e 's|__BIN__|$(BIN)|g' \
		deploy/dispatch.service | sudo tee $(UNIT) > /dev/null
	sudo systemctl daemon-reload
	sudo systemctl enable --now dispatch
	@$(MAKE) --no-print-directory service-status

service-uninstall: ## Stop + disable + remove the unit and the binary
	-sudo systemctl disable --now dispatch
	-sudo rm -f $(UNIT)
	sudo systemctl daemon-reload
	@$(MAKE) --no-print-directory uninstall

service-restart: install ## Rebuild, reinstall the binary, restart the service
	sudo systemctl restart dispatch
	@$(MAKE) --no-print-directory service-status

service-status: ## Show service status
	systemctl status dispatch --no-pager | head -8

service-logs: ## Follow service logs
	journalctl -u dispatch -f

## ---- auto-update from git ---------------------------------------------------

deploy-env: ## Record install settings in DEPLOY_ENV=$(DEPLOY_ENV) (the updater re-renders units from it)
	@test -n "$(REPO)" || { echo "no git remote 'origin' here; pass REPO=https://github.com/you/dispatch"; exit 1; }
	@printf '%s\n' \
		"# Written by 'make deploy-env'. Plain values only — systemd reads this as an" \
		"# EnvironmentFile and the updater sources it. Re-run make to change it." \
		"DISPATCH_REPO=$(REPO)" \
		"DISPATCH_BRANCH=$(BRANCH)" \
		"DISPATCH_SRC=$(SRC)" \
		"DISPATCH_BIN=$(BIN)" \
		"DISPATCH_SERVICE=dispatch.service" \
		"DISPATCH_UPDATE_STATE=$(STATE)" \
		"GO=$(GO_BIN)" \
		"DISPATCH_USER=$(USER_)" \
		"DISPATCH_GROUP=$(GROUP_)" \
		"DISPATCH_HOME=$(HOME_)" \
		"DISPATCH_INTERVAL=$(INTERVAL)" \
		"DISPATCH_UPDATER=$(UPDATER)" \
		"DISPATCH_UNIT=$(UNIT)" \
		"DISPATCH_UPDATE_UNIT=$(UPDATE_UNIT)" \
		"DISPATCH_UPDATE_TIMER=$(UPDATE_TIMER)" \
		| $(SUDO) install -D -m 0644 /dev/stdin $(DEPLOY_ENV)
	@echo "wrote $(DEPLOY_ENV)"

update-install: deploy-env ## Poll REPO for new commits every INTERVAL=$(INTERVAL), rebuild, restart (SRC=$(SRC))
	@test -n "$(GO_BIN)" || { echo "go not found on PATH; pass GO_BIN=/path/to/go"; exit 1; }
	@test -n "$(REPO)" || { echo "no git remote 'origin' here; pass REPO=https://github.com/you/dispatch"; exit 1; }
	sudo install -d $(dir $(UPDATER))
	sudo install -m 0755 scripts/dispatch-update.sh $(UPDATER)
	sed -e 's|__REPO__|$(REPO)|g' -e 's|__BRANCH__|$(BRANCH)|g' -e 's|__SRC__|$(SRC)|g' \
		-e 's|__BIN__|$(BIN)|g' -e 's|__GO__|$(GO_BIN)|g' -e 's|__UPDATER__|$(UPDATER)|g' \
		deploy/dispatch-update.service | sudo tee $(UPDATE_UNIT) > /dev/null
	sed -e 's|__INTERVAL__|$(INTERVAL)|g' deploy/dispatch-update.timer | sudo tee $(UPDATE_TIMER) > /dev/null
	sudo systemctl daemon-reload
	sudo systemctl enable --now dispatch-update.timer
	@$(MAKE) --no-print-directory update-status

update-uninstall: ## Stop the timer and remove the updater (leaves SRC, state and the binary)
	-$(SUDO) systemctl disable --now dispatch-update.timer
	-$(SUDO) rm -f $(UPDATE_UNIT) $(UPDATE_TIMER) $(UPDATER) $(DEPLOY_ENV)
	$(SUDO) systemctl daemon-reload

update-now: ## Run one update immediately instead of waiting for the timer
	@sudo systemctl start dispatch-update.service; rc=$$?; \
		journalctl -u dispatch-update.service -n 30 --no-pager; \
		exit $$rc

update-status: ## Show when the update timer last ran and next fires
	systemctl list-timers dispatch-update.timer --no-pager
	@systemctl status dispatch-update.service --no-pager | head -6

update-logs: ## Follow updater logs
	journalctl -u dispatch-update.service -f

## ---- run it in docker -------------------------------------------------------

DOCKER_IMAGE ?= dispatch:local
# The container user reads the docker socket through this group.
DOCKER_GID ?= $(shell stat -c %g /var/run/docker.sock 2>/dev/null || echo 999)

docker-build: ## Build the dispatch container image (deploy/docker; context is the repo root)
	docker build -f deploy/docker/Dockerfile --build-arg GIT_SHA=$$(git rev-parse HEAD) -t $(DOCKER_IMAGE) .

docker-run: docker-build ## Run it: config + logins mounted from $HOME, docker socket lent in
	@test -f "$(CONFIG)" || { echo "no config at $(CONFIG) — run make setup first"; exit 1; }
	@docker rm -f dispatch >/dev/null 2>&1 || true
	docker run -d --name dispatch --init --restart unless-stopped --stop-timeout 150 \
		--group-add $(DOCKER_GID) \
		-v dispatch-data:/opt/dispatch \
		-v $(dir $(CONFIG)):/home/dispatch/.config/dispatch \
		-v $(HOME)/.claude:/home/dispatch/.claude \
		-v $(HOME)/.config/gh:/home/dispatch/.config/gh \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-p 127.0.0.1:8788:8788 \
		$(DOCKER_IMAGE)
	@docker logs -f dispatch

docker-stop: ## Stop and remove the container (SIGTERM: drain, persist, exit 0)
	docker stop -t 150 dispatch && docker rm dispatch

docker-logs: ## Follow the container's logs (dispatch and the updater both log here)
	docker logs -f dispatch

## ---- help -------------------------------------------------------------------

help: ## Show this help
	@awk 'BEGIN{FS=":.*## "} /^## ----/{sub(/^## ---- /,""); sub(/ -+$$/,""); printf "\n%s\n",$$0; next} /^[a-zA-Z_-]+:.*## /{printf "  %-20s %s\n",$$1,$$2}' $(MAKEFILE_LIST)
