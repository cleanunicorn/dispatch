# dispatch — the whole orchestrator in one container, self-updating from git.
#
# Build from the repo root (the build context is the source tree):
#   docker build -f deploy/docker/Dockerfile -t dispatch:local .
# or just: make docker-build
#
# The image carries:
#   - a dispatch binary built from this checkout, kept as the seed the
#     entrypoint installs on the first start (the in-container updater
#     replaces it with builds from origin/<branch> from the first tick on)
#   - the Go toolchain, so the container can rebuild dispatch itself
#   - the claude CLI (installed the way SETUP.md does it, under $HOME, where
#     the CLI's own auto-updater can write), the gh CLI, and the docker CLI
#
# What to mount in — see SETUP.md, "Running it in Docker":
#   - the config at $DISPATCH_CONFIG (default /home/dispatch/.config/dispatch/config.toml),
#     which also carries the SQLite event log and the per-task workdirs
#   - /opt/dispatch on a volume: the deploy checkout, the live binary and the
#     updater's state survive container restarts there
#   - the host's Claude and GitHub logins, and the docker socket if agent
#     definitions use docker environments
#
# Entrypoint: deploy/docker/entrypoint.sh — runs dispatch, restarts it on
# crash, and polls origin/<branch> every DISPATCH_UPDATE_INTERVAL seconds via
# deploy/docker/dispatch-update.sh.

FROM golang:1.25-bookworm AS build
ARG GIT_SHA
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -o /out/dispatch ./cmd/dispatch \
 && printf '%s\n' "${GIT_SHA:-unknown}" > /out/git.sha

FROM debian:bookworm-slim

# The docker CLI only (docker environments shell out to it; no daemon here —
# the socket is mounted). Pin with --build-arg DOCKER_CLI_VERSION=…
ARG DOCKER_CLI_VERSION=28.3.3

RUN case "$(dpkg --print-architecture)" in \
	amd64) dlarch=x86_64 ;; \
	arm64) dlarch=aarch64 ;; \
	*) printf 'unsupported architecture\n' >&2; exit 1 ;; \
	esac \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
	ca-certificates curl git gnupg util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL "https://download.docker.com/linux/static/stable/${dlarch}/docker-${DOCKER_CLI_VERSION}.tgz" \
	| tar -xz -C /usr/local/bin --strip-components=1 docker/docker

# gh: provisioning puts it in every agent image, and the GitHub login lending
# (`gh auth token`) reads it here too.
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
	-o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
	> /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y --no-install-recommends gh && rm -rf /var/lib/apt/lists/*

# The Go toolchain the updater builds with. GOTOOLCHAIN=auto (the default)
# downloads a newer toolchain into $GOPATH if a future go.mod asks for one,
# and $GOPATH sits on the data volume, so the download happens once.
COPY --from=build /usr/local/go /usr/local/go

RUN useradd --create-home --uid 1000 dispatch \
 && install -d -o dispatch -g dispatch \
	/home/dispatch/.config/dispatch \
	/opt/dispatch/src /opt/dispatch/bin /opt/dispatch/state

COPY --chown=dispatch:dispatch deploy/docker/entrypoint.sh deploy/docker/dispatch-update.sh /usr/local/lib/dispatch/
COPY --from=build --chown=dispatch:dispatch /out/dispatch /usr/local/lib/dispatch/dispatch.seed
COPY --from=build --chown=dispatch:dispatch /out/git.sha /usr/local/lib/dispatch/dispatch.seed.sha

ENV HOME=/home/dispatch \
	PATH="/home/dispatch/.local/bin:/usr/local/go/bin:${PATH}" \
	GOPATH=/opt/dispatch/state/go \
	GOCACHE=/opt/dispatch/state/gocache \
	DISPATCH_CONFIG=/home/dispatch/.config/dispatch/config.toml \
	DISPATCH_SRC=/opt/dispatch/src \
	DISPATCH_BIN=/opt/dispatch/bin/dispatch \
	DISPATCH_UPDATE_STATE=/opt/dispatch/state/deployed.sha \
	DISPATCH_PIDFILE=/opt/dispatch/state/dispatch.pid \
	DISPATCH_UPDATE_INTERVAL=300 \
	DISPATCH_UPDATE_FIRST=120

# The claude CLI, installed as the container user the same way SETUP.md
# installs it on the host — under $HOME, where its auto-updater can write.
USER dispatch
RUN curl -fsSL https://claude.ai/install.sh | bash

USER dispatch
WORKDIR /home/dispatch
# `dispatch:local` runs `run`; `dispatch:local doctor` runs doctor; extra args
# after `run` (-web, -terminal) pass through to dispatch.
ENTRYPOINT ["/usr/local/lib/dispatch/entrypoint.sh"]
CMD ["run"]