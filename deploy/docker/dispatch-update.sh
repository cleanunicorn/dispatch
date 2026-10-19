#!/usr/bin/env bash
# dispatch-update — the in-container updater: pull origin/<branch>, rebuild, restart dispatch.
#
# The Docker counterpart of scripts/dispatch-update.sh: fired every
# DISPATCH_UPDATE_INTERVAL seconds by the loop in deploy/docker/entrypoint.sh,
# or by hand:
#
#   docker exec dispatch /usr/local/lib/dispatch/dispatch-update.sh
#   docker exec -e DISPATCH_UPDATE_FORCE=1 dispatch /usr/local/lib/dispatch/dispatch-update.sh
#
# Same contract as the host updater: never leave dispatch down. The new binary
# is built and smoke-tested in a scratch directory first; the live binary is
# only replaced once that passes, and the replacement is an atomic rename. The
# running process is then SIGTERMed — the same drain path as `docker stop` —
# and the entrypoint restarts it onto the new binary. A binary that will not
# stay up is rolled back to $BIN.prev and its sha recorded in
# $DISPATCH_UPDATE_STATE.failed, skipped until the branch moves.
#
# It also deploys the glue — this script and the entrypoint — because a
# release that changes deploy/docker/ is as much "the new version" as one that
# changes the Go code. A new entrypoint takes effect the next time the
# container starts; the updater itself is re-executed on the spot, like on the
# host. There are no unit files to re-render: the container's systemd is the
# entrypoint, and its "unit" is the ENV block baked into the image.
set -Eeuo pipefail

log() { printf 'dispatch-update: %s\n' "$*"; }
fail() { printf 'dispatch-update: ERROR: %s\n' "$*" >&2; exit 1; }

# ---- configuration ----------------------------------------------------------
REPO=${DISPATCH_REPO:-https://github.com/cleanunicorn/dispatch}
BRANCH=${DISPATCH_BRANCH:-main}
SRC=${DISPATCH_SRC:-/opt/dispatch/src}
BIN=${DISPATCH_BIN:-/opt/dispatch/bin/dispatch}
STATE=${DISPATCH_UPDATE_STATE:-/opt/dispatch/state/deployed.sha}
LOCK=${DISPATCH_UPDATE_LOCK:-/opt/dispatch/state/update.lock}
UPDATER=${DISPATCH_UPDATER:-/usr/local/lib/dispatch/dispatch-update.sh}
ENTRYPOINT=${DISPATCH_ENTRYPOINT:-/usr/local/lib/dispatch/entrypoint.sh}
PIDFILE=${DISPATCH_PIDFILE:-/opt/dispatch/state/dispatch.pid}
# How long the restarted dispatch must stay up before the deploy counts as good.
GRACE=${DISPATCH_UPDATE_GRACE:-10}
FORCE=${DISPATCH_UPDATE_FORCE:-}
# How long to wait for the old process to exit after SIGTERM: drain_timeout
# (default 2m) plus margin.
DRAIN_WAIT=${DISPATCH_UPDATE_DRAIN_WAIT:-300}
# How long to wait for the entrypoint to start the new process (RestartSec=5
# plus margin).
START_WAIT=${DISPATCH_UPDATE_START_WAIT:-60}
# 0 leaves this script and the entrypoint alone (binary-only deploys).
SYNC_GLUE=${DISPATCH_UPDATE_SYNC_GLUE:-1}

# ---- one updater at a time ----------------------------------------------------
# A slow build must not overlap the next tick.
if [ -z "${DISPATCH_UPDATE_LOCKED:-}" ]; then
	export DISPATCH_UPDATE_LOCKED=1
	# -E 75: distinguish "lock is held" from the child's own exit code.
	flock -n -E 75 "$LOCK" "$0" "$@" && exit 0
	rc=$?
	[ "$rc" = 75 ] && { log "another update is already running; skipping this tick"; exit 0; }
	exit "$rc"
fi

command -v git >/dev/null || fail "git not found"
GO=${GO:-go}
command -v "$GO" >/dev/null || fail "go not found"

if [ ! -d "$SRC/.git" ]; then
	log "cloning $REPO into $SRC"
	mkdir -p "$SRC"
	git clone --branch "$BRANCH" "$REPO" "$SRC"
fi

git -C "$SRC" remote set-url origin "$REPO"
git -C "$SRC" fetch --prune --quiet origin "$BRANCH"
remote_sha=$(git -C "$SRC" rev-parse "origin/$BRANCH")

# Reset every tick, not only when deploying: the glue below is installed from
# this checkout, so it has to match the branch even on a tick that builds
# nothing. Discard anything local — this checkout is the deploy's, not a place
# to edit.
git -C "$SRC" reset --hard --quiet "origin/$BRANCH"
git -C "$SRC" clean -ffdq

# ---- glue: the updater itself, then the entrypoint -----------------------------

# Replace this script and hand over to the new copy, so a release that changes
# the updater takes effect on the tick that brings it in rather than the one
# after.
sync_self() {
	[ "$SYNC_GLUE" = 1 ] || return 0
	local new="$SRC/deploy/docker/dispatch-update.sh"
	[ -f "$new" ] || return 0
	cmp -s "$new" "$UPDATER" && return 0
	[ -z "${DISPATCH_UPDATE_REEXEC:-}" ] || {
		log "WARNING: updater still differs from the checkout after re-exec; continuing with the old one"
		return 0
	}
	log "updater changed on $BRANCH — installing and handing over"
	install -m 0755 "$new" "$UPDATER.new"
	mv -f "$UPDATER.new" "$UPDATER"
	export DISPATCH_UPDATE_REEXEC=1
	# The flock fd is inherited across exec, so the lock is still held.
	exec "$UPDATER" "$@"
}

# The entrypoint is already running and cannot be handed over from underneath
# itself the way the updater can; a new one takes effect on the next container
# start (docker compose restart dispatch).
sync_entrypoint() {
	[ "$SYNC_GLUE" = 1 ] || return 0
	local new="$SRC/deploy/docker/entrypoint.sh"
	[ -f "$new" ] || return 0
	cmp -s "$new" "$ENTRYPOINT" && return 0
	log "entrypoint changed on $BRANCH — installing (takes effect on the next container start)"
	install -m 0755 "$new" "$ENTRYPOINT.new"
	mv -f "$ENTRYPOINT.new" "$ENTRYPOINT"
}

sync_self "$@"
sync_entrypoint

# ---- binary -------------------------------------------------------------------
# The sha the running binary was built from. Not the checkout's HEAD: the
# checkout is reset before the build, so a failed build would otherwise look
# "up to date" forever while an older binary keeps running.
deployed_sha=$(cat "$STATE" 2>/dev/null || echo none)
POISON="$STATE.failed"

if [ "$deployed_sha" = "$remote_sha" ] && [ -x "$BIN" ] && [ -z "$FORCE" ]; then
	log "up to date at ${remote_sha:0:12}"
	exit 0
fi

if [ "$(cat "$POISON" 2>/dev/null || true)" = "$remote_sha" ] && [ -z "$FORCE" ]; then
	log "${remote_sha:0:12} already failed to stay up; waiting for a new commit on $BRANCH"
	log "(DISPATCH_UPDATE_FORCE=1 retries it anyway)"
	exit 0
fi

log "updating ${deployed_sha:0:12} -> ${remote_sha:0:12} on $BRANCH"

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

log "building"
(cd "$SRC" && "$GO" build -o "$stage/dispatch" ./cmd/dispatch) || fail "build failed, keeping ${deployed_sha:0:12}"

# Smoke test: a binary that cannot even print its usage must not replace a working one.
"$stage/dispatch" -h >/dev/null 2>&1 || fail "new binary failed its smoke test, keeping ${deployed_sha:0:12}"

mkdir -p "$(dirname "$BIN")" "$(dirname "$STATE")"
# Keep the binary we are replacing: it is the only thing known to actually run.
if [ -x "$BIN" ]; then cp -f "$BIN" "$BIN.prev"; fi
install -m 0755 "$stage/dispatch" "$BIN.new"
mv -f "$BIN.new" "$BIN"    # atomic: a concurrent exec sees old or new, never a partial file
log "installed $BIN at ${remote_sha:0:12}"

commit_state() { printf '%s\n' "$remote_sha" > "$STATE"; rm -f "$POISON"; }

# Nothing running (no pidfile): the entrypoint — or the next container start —
# picks the new binary up itself.
old_pid=$(cat "$PIDFILE" 2>/dev/null || true)
if [ -z "$old_pid" ] || ! kill -0 "$old_pid" 2>/dev/null; then
	commit_state
	log "no running dispatch found — binary updated, nothing restarted"
	exit 0
fi

# SIGTERM the running process: it drains in-flight tool calls (drain_timeout,
# default 2m) and exits. If it never exits, nothing is committed and the old
# binary keeps running — the next tick retries.
log "stopping dispatch (pid $old_pid) — waiting for the drain"
kill -TERM "$old_pid"
stopped=0
for _ in $(seq "$DRAIN_WAIT"); do
	kill -0 "$old_pid" 2>/dev/null || { stopped=1; break; }
	sleep 1
done
[ "$stopped" = 1 ] || fail "dispatch (pid $old_pid) did not exit within ${DRAIN_WAIT}s — it is still running the old binary; nothing committed, the next tick retries"

# The entrypoint's restart delay is 5s; wait for the new pid it writes.
new_pid=""
for _ in $(seq "$START_WAIT"); do
	cand=$(cat "$PIDFILE" 2>/dev/null || true)
	if [ -n "$cand" ] && [ "$cand" != "$old_pid" ]; then new_pid=$cand; break; fi
	sleep 1
done
[ -n "$new_pid" ] || fail "no new dispatch pid appeared within ${START_WAIT}s — nothing committed, the next tick retries"

# The deployed sha is written only after the new process has proven it can
# stay up: a deploy that never came up is not a deploy.
sleep "$GRACE"
if ! kill -0 "$new_pid" 2>/dev/null || [ "$(cat "$PIDFILE" 2>/dev/null || true)" != "$new_pid" ]; then
	printf '%s\n' "$remote_sha" > "$POISON"
	if [ ! -x "$BIN.prev" ]; then
		fail "dispatch did not stay up on ${remote_sha:0:12} and there is no previous binary to restore"
	fi
	log "dispatch did not stay up on ${remote_sha:0:12} — rolling back to the previous binary"
	cp -f "$BIN.prev" "$BIN.rollback"
	mv -f "$BIN.rollback" "$BIN"
	# Whatever the entrypoint started meanwhile may still be the bad binary;
	# stop it so the restart delay puts the restored one in its place.
	cur=$(cat "$PIDFILE" 2>/dev/null || true)
	if [ -n "$cur" ] && kill -0 "$cur" 2>/dev/null; then
		kill -TERM "$cur"
		for _ in $(seq "$DRAIN_WAIT"); do
			kill -0 "$cur" 2>/dev/null || break
			sleep 1
		done
	fi
	fail "rolled back to ${deployed_sha:0:12}; ${remote_sha:0:12} is skipped until $BRANCH moves"
fi

commit_state
log "dispatch is up on ${remote_sha:0:12}"