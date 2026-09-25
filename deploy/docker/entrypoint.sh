#!/usr/bin/env bash
# entrypoint — the container's init: run dispatch, restart it on crash, keep it fresh.
#
# The Docker counterpart of deploy/dispatch.service + dispatch-update.timer on
# a systemd host. Three jobs:
#
#   1. Seed the data volume on the first start: install the binary the image
#      was built with (dispatch.seed) as DISPATCH_BIN, and record the sha it
#      was built from, so dispatch runs before the first update tick.
#   2. Run dispatch as the foreground child and restart it if it dies — the
#      unit's Restart=always, RestartSec=5. A SIGTERM to the container reaches
#      dispatch the way `systemctl stop` does: live threads are notified and
#      in-flight tool calls get drain_timeout to finish before the process
#      exits. Give `docker stop` a TimeoutStopSec-sized grace: 150s.
#   3. Run the update service: first tick after DISPATCH_UPDATE_FIRST seconds
#      (the timer's OnBootSec), then every DISPATCH_UPDATE_INTERVAL. Each tick
#      runs dispatch-update.sh, which rebuilds from origin/<branch>, installs
#      the new binary with an atomic rename and SIGTERMs dispatch — the wait
#      below then starts it again, onto the new binary.
#
# What `run` gets is the Dockerfile CMD: `dispatch:local` runs `run`,
# `dispatch:local run -web` adds flags, `dispatch:local doctor` runs doctor
# directly (no supervisor, exits when done). Runs as the `dispatch` user;
# SIGTERM/SIGINT exit 0 once the drain has finished.
set -Eeuo pipefail

log() { printf 'dispatch-entrypoint: %s\n' "$*"; }

# ---- configuration (docker run -e overrides) --------------------------------
CONFIG=${DISPATCH_CONFIG:-/home/dispatch/.config/dispatch/config.toml}
SRC=${DISPATCH_SRC:-/opt/dispatch/src}
BIN=${DISPATCH_BIN:-/opt/dispatch/bin/dispatch}
SEED=${DISPATCH_SEED:-/usr/local/lib/dispatch/dispatch.seed}
SEED_SHA=${DISPATCH_SEED_SHA:-/usr/local/lib/dispatch/dispatch.seed.sha}
STATE=${DISPATCH_UPDATE_STATE:-/opt/dispatch/state/deployed.sha}
PIDFILE=${DISPATCH_PIDFILE:-/opt/dispatch/state/dispatch.pid}
UPDATER=${DISPATCH_UPDATER:-/usr/local/lib/dispatch/dispatch-update.sh}
INTERVAL=${DISPATCH_UPDATE_INTERVAL:-300}
FIRST=${DISPATCH_UPDATE_FIRST:-120}
# 1 skips the update service entirely (0 leaves it on): for maintenance, or a
# pinned version you want to keep running while the branch moves.
DISABLE=${DISPATCH_UPDATE_DISABLE:-0}

mkdir -p "$SRC" "$(dirname "$BIN")" "$(dirname "$STATE")" "$(dirname "$PIDFILE")"

# ---- seed the data volume -----------------------------------------------------
if [ ! -x "$BIN" ]; then
	log "seeding $BIN from the image"
	install -m 0755 "$SEED" "$BIN.new"
	mv -f "$BIN.new" "$BIN"
fi
if [ ! -r "$STATE" ]; then
	install -m 0644 "$SEED_SHA" "$STATE"
fi

case "${1:-run}" in
run)
	shift
	;;
setup|doctor|user)
	exec "$BIN" "$@"
	;;
*)
	printf 'dispatch-entrypoint: unknown subcommand: %s\n' "$1" >&2
	exit 2
	;;
esac

# ---- the update service --------------------------------------------------------
# OnBootSec, then OnUnitActiveSec: first tick after FIRST seconds, then one
# every INTERVAL. The updater holds its own lock, so a tick that runs long
# simply makes the next one skip.
UPDATER_PID=""
if [ "$DISABLE" != 1 ]; then
	(
		while :; do
			sleep "$FIRST"
			FIRST=$INTERVAL
			if ! "$UPDATER"; then
				log "update tick failed — the next tick retries"
			fi
		done
	) &
	UPDATER_PID=$!
else
	log "update service disabled (DISPATCH_UPDATE_DISABLE=1) — dispatch runs as-is"
fi

# ---- run dispatch, restart it forever -------------------------------------------
STOPPING=0
child=0
term() {
	STOPPING=1
	kill "$UPDATER_PID" 2>/dev/null || true
	kill "$child" 2>/dev/null || true
}
trap term TERM INT
trap 'rm -f "$PIDFILE"' EXIT

start_dispatch() {
	"$BIN" run -config "$CONFIG" "$@" &
	child=$!
	printf '%s\n' "$child" > "$PIDFILE"
}

log "starting dispatch from $(cat "$STATE" 2>/dev/null || echo unknown)"
start_dispatch

while :; do
	rc=0
	wait "$child" || rc=$?
	if [ "$STOPPING" = 1 ]; then
		# The trap interrupted the wait; the process is still draining.
		# Reap its real exit status — the interrupted wait reported only
		# the signal that interrupted it.
		rc=0
		wait "$child" 2>/dev/null || rc=$?
		log "dispatch stopped (rc=$rc) — exiting"
		exit 0
	fi
	log "dispatch exited (rc=$rc) — restarting in 5s"
	sleep 5
	start_dispatch
done