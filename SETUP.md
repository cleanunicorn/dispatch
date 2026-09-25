# Setting up dispatch on a Linux machine

About 20 minutes. You need: a Linux box (or VM) you can run a service on,
Go 1.25+ to build, Claude Code installed and logged in, and admin rights on a
Slack workspace.

## 1. Build

```sh
git clone https://github.com/cleanunicorn/dispatch && cd dispatch
make build          # -> bin/dispatch
```

## 2. Install and log in to Claude Code (as the user that will run dispatch)

```sh
curl -fsSL https://claude.ai/install.sh | bash   # or see https://code.claude.com/docs/en/setup
claude                                           # then type /login and follow the browser flow
claude -p --model haiku "Reply with OK"          # prints OK when authenticated
```

dispatch drives `claude -p` as a subprocess; it uses whatever login that user has.

## 3. Create the Slack app

1. Open https://api.slack.com/apps → **Create New App** → **From a manifest**.
2. Pick your workspace, paste `deploy/slack-manifest.yaml`, create.
3. **Basic Information → App-Level Tokens → Generate Token and Scopes**:
   name it `socket`, add scope `connections:write`, generate. Copy the `xapp-…` token.
4. **Install App → Install to Workspace**. Copy the **Bot User OAuth Token** (`xoxb-…`).
5. In Slack, invite the bot to a channel: `/invite @dispatch`.

The manifest enables Socket Mode, so the machine needs outbound HTTPS only —
no public URL, no inbound ports.

Scopes, what each one is for, and the thread UI are in [docs/slack.md](docs/slack.md);
the browser UI, which can run next to Slack, is in [docs/web.md](docs/web.md).

To find your Slack user ID (for `allowed_users`): click your profile → **⋯** →
**Copy member ID**.

## 4. Run the wizard

```sh
bin/dispatch setup
```

It asks for the database/workdir paths, checks `claude`, takes the two Slack
tokens, defines your first agent, writes `~/.config/dispatch/config.toml`, then
runs `dispatch doctor`. Fix anything marked ✘ and re-run `bin/dispatch doctor`.

You can edit the file by hand afterwards — `deploy/config.example.toml` shows
every option, including docker and ssh environments and a second surface.

More agents can be added later from Slack (or the terminal) without a
restart: see [Managing agents from chat](#managing-agents-from-chat).

## 5. Try it

Terminal first (no Slack needed; details in [docs/terminal.md](docs/terminal.md)):

```sh
bin/dispatch run -terminal
> agents
> run coder create hello.py that prints hi, then run it
```

Then with Slack:

```sh
bin/dispatch run
```

In the channel you invited the bot to:

```
@dispatch create hello.py that prints hi, then run it
```

dispatch answers **in a thread under your message** (look for "1 reply"), using the
channel's default agent; `run <agent> <prompt>` picks a specific one. Slack
has no autocomplete for text after a mention, so a bare `@dispatch run` posts
the agent list as buttons (a searchable menu from five agents up) and then
asks for the prompt in the thread; `run <agent>` alone skips straight to the
prompt question.

Each channel can have its own default agent: `@dispatch default reviewer` in
`#code-review` makes plain messages there run *reviewer*. That appends a
`[[channels]]` block to `config.toml` (you can also write one by hand with
the channel id from the channel's details pane); `default` shows the
current one and `agents` marks it. Without a channel default,
`server.default_agent` applies.

When the agent asks a question (Claude Code's `AskUserQuestion`), the thread
shows the options as buttons; click one, or reply in the thread with your own
answer. Multi-select questions are answered one option at a time for now. When the agent wants to run
something not pre-approved you get **Allow / Deny** buttons. Reply in the
thread to continue the conversation; `status`, `cancel`, `close`, `agent list`
(or `agents`), `help` work anywhere. DMs to the bot work the same way without
the mention.

## While the agent works

The last message in a task thread is a live status line — `⏳ thinking · 4s`,
`🔧 Bash \`go test ./...\` · 1m05s · 6 tool calls` — edited in place every
few seconds and moved below each new message, and your message carries ⏳
(✋ while the agent waits for your answer). When the turn ends the status line
goes, the closing line says how long it took, how many tools it used and what
it cost, and the mark becomes 📬: the agent has answered and the thread waits
for your next message — or for `close`. A failed task leaves ❌ instead. A
task thread is never without a mark, so a channel reads at a glance: ⏳ and ✋
are in progress, 📬 and ❌ are yours to answer or close, ✅ is done with.

The lines that need you — a permission or question prompt, the closing line,
an error, a "dispatch is back" notice that asks you to pick the task up —
mention whoever wrote the message the turn answers (`@you ✅ done · …`), so
you can mute the thread and still be told when to look. Only that one person
is tagged: on a thread two people share, each is tagged on the answers to
their own messages — a message sent while a turn is still going joins that
turn, so its writer is the one tagged when it ends, and whoever clicks a
prompt's button does not take the tag from the person who asked; `⏹️
cancelled` is not tagged; the
status line and the agent's own text never are. This needs no extra scope.

The same text also shows above the composer ("dispatch ⏳ thinking · 4s") when
the app has Slack's *Agents & AI Apps* feature: the manifest enables it
(`assistant_view`, the `assistant:write` scope, the `assistant_thread_*`
events). It also turns the app's DM into Slack's assistant split view. If you
created the app before it was added, either add those to the app and
reinstall, or leave it out — dispatch notices the first failure, logs one line
and keeps the in-thread status line.

The ⏳/✋/📬/❌/✅ marks need the `reactions:write` bot scope.

## Closing a thread

`close` in a thread ends the conversation there: the task running on it is
cancelled, the thread's first message gets a ✅, and dispatch stops following
the thread — later replies in it are ignored instead of resuming the agent,
and a restart leaves it alone. Mention the bot in the thread again (or type
in it on the terminal transport) to reopen it and continue where you left
off; the agent session is still there.

The ✅ needs the `reactions:write` bot scope (in the manifest; if you created
the app before it was added, add the scope and reinstall — without it closing
still works, dispatch just logs a warning instead of reacting). The only
message dispatch ever deletes is its own live status line: closing tidies the
channel by ending threads, Slack's own history stays intact.

## Managing agents from chat

```
@dispatch agent add
```

dispatch asks, one question per message in the thread: name, model, where it
runs (local / docker / ssh and the image, host or directory that goes with
it), permission mode, pre-approved tools (presets or a comma-separated list)
and an optional system prompt, then shows a summary with **Save / Cancel**.
Click a button or type the answer; `cancel` at any point abandons the flow.
Type paths in backticks (`` `/home/me/app` ``): Slack refuses to send a
message that starts with `/` because it looks like a slash command.
Answers are saved as you go, so a dispatch restart mid-way re-asks the next
question when it is back.

Saving appends a `[[definitions]]` block to `config.toml` (the rest of the
file is left untouched) and registers the agent immediately — `agents` lists
it and `run <name> <prompt>` works without a restart. Settings the flow does
not ask for (`sub_agents`, `mcp_config`, `key_path`, container `env`) can be
added to that block by hand afterwards.

`@dispatch agent edit <name>` shows the agent's settings with a menu — model,
environment, permissions, tools, system prompt — re-asks the field you pick,
and on **Save** rewrites that agent's `[[definitions]]` block (tasks already
running keep their old settings). `@dispatch agent delete <name>` asks for a
confirmation and removes the block; an agent that is the global
`default_agent` or a channel default is refused until the default points
elsewhere. Both pick the agent from a list when the name is left out.

## 6. Install as a service

```sh
make service-install   # installs /usr/local/bin/dispatch and a systemd unit for the current user
make service-logs
```

`make service-install` substitutes your user, group and home into
`deploy/dispatch.service`, enables it and starts it. The unit runs as *your*
user so it finds your Claude Code login and ssh/docker config. Edit
`/etc/systemd/system/dispatch.service` if the binary or config live elsewhere.

Remove with `make service-uninstall`. Rebuild and restart after a code change with `make service-restart`; logs with `make service-logs`.

## 7. Keep it up to date automatically

```sh
make update-install    # poll origin/main every 5 minutes; rebuild and restart on a new commit
make update-status     # when it last ran, when it fires next
make update-logs
```

This installs two more system units and a copy of `scripts/dispatch-update.sh`:

- `dispatch-update.timer` — fires 2 minutes after boot, then every `INTERVAL`
  (default `5min`). The `OnBootSec` tick is what covers downtime: the machine
  comes back and picks up whatever landed on the branch meanwhile.
- `dispatch-update.service` — a oneshot that does the work, as root.

Each run: `git fetch` into a **deploy checkout** at `SRC` (default
`/opt/dispatch/src`, cloned on the first run) and hard-reset it to `origin/main`.
Then two things are brought into line with the branch.

**The glue** — this script and the three unit files. A release that changes
`deploy/` is as much "the new version" as one that changes the Go code, and
deploying only the binary silently leaves the box on the old units. Each unit is
re-rendered from the checkout using the settings recorded at install time in
`/etc/dispatch/deploy.env`, compared with what is installed, and written only if it
differs, followed by `daemon-reload`. If the updater script itself changed, it is
installed and re-executed on the spot, so a release lands on the tick that brings
it in rather than the one after.

**The binary** — if the deployed sha already matches `origin/main` there is
nothing to build. Otherwise: build into a scratch directory, smoke-test the new
binary, replace `/usr/local/bin/dispatch` with an atomic rename, and
`systemctl restart dispatch`.

The deploy checkout is root-owned and separate from any checkout you edit in —
it is reset on every run, so never work in it.

Three things can go wrong, and each has a defined outcome:

| failure | what happens |
|---|---|
| `main` does not compile | old binary keeps running, nothing restarts, exit 1 in the journal; retried every tick and deploys itself once `main` compiles again |
| new binary fails `dispatch -h` | same — it never reaches `/usr/local/bin/dispatch` |
| new binary installs but the service will not stay up | the previous binary is restored from `$BIN.prev`, the service is restarted, and that sha is recorded in `deployed.sha.failed` and skipped until the branch moves |
| a unit file systemd cannot parse | detected via its `LoadState` after `daemon-reload`, restored from `$UNIT.prev`, reloaded again; the binary deploy carries on |
| a unit that parses but the service will not start on it | restored from `$UNIT.prev` — and if the same tick also deployed a binary, both are put back, since either could be the reason |

That last case is why the deployed sha lives in `/var/lib/dispatch/deployed.sha`
and is written *after* the restart is confirmed healthy, not before: a deploy
that never came up is not a deploy. `DISPATCH_UPDATE_GRACE` (default 10s) is how
long the service must stay up to count, and `DISPATCH_UPDATE_FORCE=1` retries a
sha that was skipped, and `DISPATCH_UPDATE_SYNC_GLUE=0` goes back to binary-only
deploys.

`/etc/dispatch/deploy.env` is what makes unit re-rendering possible — it records the
`USER_`/`GROUP_`/`HOME_`/`BIN`/`INTERVAL` values the installers were given. It is
written by `make service-install` and `make update-install`; change a setting by
re-running those with the new value rather than editing the file. Without it the
updater logs a warning and leaves the units alone.

**Upgrading an existing box to this**: the updater already on the box does not know
how to deploy glue, so it cannot install the version that does. Run
`make service-install && make update-install` by hand once; every release after
that lands on its own.

Restarts go through the same drain path as everything else (see *Restarting
dispatch* below): live threads are notified, in-flight tool calls get
`drain_timeout` to finish, and interrupted tasks resume themselves after the
new binary starts. A deploy that lands mid-task is not a lost task.

Overridable on install:

| variable   | default                    | what it is                          |
|------------|----------------------------|-------------------------------------|
| `REPO`     | this clone's `origin` URL  | what to pull from                   |
| `BRANCH`   | `main`                     | branch to track                     |
| `INTERVAL` | `5min`                     | poll period                         |
| `SRC`      | `/opt/dispatch/src`          | the deploy checkout                 |
| `BIN`      | `/usr/local/bin/dispatch`    | where the binary is installed       |

```sh
make update-install BRANCH=release INTERVAL=15min SRC=/opt/dispatch/src
make update-now          # deploy right now instead of waiting for the tick
make update-uninstall    # stop and remove the timer; the binary stays
```

A private repo needs credentials root can use non-interactively — a deploy key
plus `REPO=git@github.com:you/dispatch.git` and a `/root/.ssh/config` entry, or a
token in the URL.

## 8. Or run it in Docker

Same thing as §6 and §7 — dispatch as a service that keeps itself up to date —
inside one container. `deploy/docker/Dockerfile` builds an image that carries
the dispatch binary from the checkout it was built from, the Go toolchain, the
claude CLI (installed as the container user, so its own auto-updater works),
the gh CLI and the docker CLI; `deploy/docker/entrypoint.sh` runs dispatch,
restarts it on crash, and polls `origin/<branch>` every
`DISPATCH_UPDATE_INTERVAL` seconds (default 300, first tick after 120) with
`deploy/docker/dispatch-update.sh` — the `dispatch-update.timer` analog, built
in.

```sh
make docker-run        # builds the image, runs the container with the mounts below
make docker-logs       # dispatch and the updater both log here
make docker-stop       # SIGTERM: drain, persist, exit 0
```

or, from `deploy/docker/`, `docker compose up -d --build` — `compose.yaml` is
the same set of mounts as the Makefile target, with comments.

A container borrows the host's logins the same way dispatch lends them to agent
environments, by *mounting* them rather than copying:

| mount | what it carries |
|---|---|
| `dispatch-data` → `/opt/dispatch` | the deploy checkout, the live binary, the updater's state |
| `~/.config/dispatch` → `/home/dispatch/.config/dispatch` | the config, the SQLite event log and the per-task workdirs — point `db` and `workdir_root` in the config at paths under here, and `[web] listen = "0.0.0.0:8788"` so the port mapping reaches the browser UI |
| `~/.claude` → `/home/dispatch/.claude` | the host's Claude Code login, used by dispatch itself and lent into agent environments |
| `~/.config/gh` → `/home/dispatch/.config/gh` | the host's GitHub login, same story |
| `/var/run/docker.sock` | only if agent definitions use docker environments; `group_add` the socket's gid (`stat -c %g /var/run/docker.sock`) |

Because `~/.claude` and `~/.config/gh` *are* the host's login files, the
borrowing rules work unchanged: a docker-environment agent container gets the
same credentials it would get from a host-installed dispatch. The agent
container's uid is the dispatch user's uid inside the container (1000), so
files it writes into mounted workdirs stay owned by whoever owns that path on
the host — if your user's uid is not 1000, either chown the config directory to
the container's uid or run the container with `user: "<your uid>:<your gid>"`.

The updater runs as a loop inside the container and follows the same contract
as the host one: clone once into `/opt/dispatch/src`, hard-reset to
`origin/<branch>` every tick, build into a scratch directory, smoke-test
(`dispatch -h`), install with an atomic rename, SIGTERM dispatch and wait for
the drain, then wait for the entrypoint to start the new process and prove it
stays up (10s) before writing the deployed sha. A binary that will not stay up
is rolled back from `$BIN.prev` and its sha recorded as `deployed.sha.failed`,
skipped until the branch moves. It also deploys the glue: a changed updater is
re-executed on the spot, a changed entrypoint is installed and takes effect on
the next container start (`DISPATCH_UPDATE_SYNC_GLUE=0` turns glue off).

| failure | what happens |
|---|---|
| `main` does not compile | old binary keeps running, exit 1 in `docker logs`, retried every tick |
| new binary fails `dispatch -h` | same — it never reaches `/opt/dispatch/bin/dispatch` |
| new binary installs but will not stay up | the previous binary is restored, the entrypoint restarts dispatch on it, the sha is skipped until the branch moves |
| the old process ignores SIGTERM past `DISPATCH_UPDATE_DRAIN_WAIT` (300s) | nothing is committed, the old binary keeps running, the next tick retries |

Run a tick by hand (or re-deploy a skipped sha): `docker exec dispatch
/usr/local/lib/dispatch/dispatch-update.sh`, with `-e DISPATCH_UPDATE_FORCE=1`
to retry.
To run dispatch as-is while the branch moves — a pinned version, maintenance —
`docker compose down && DISPATCH_UPDATE_DISABLE=1 docker compose up -d`.

A private repo needs credentials the container user can use non-interactively,
the same two ways the host updater does: a token in `DISPATCH_REPO`'s URL.

`docker compose stop` (or `docker stop -t 150`) is the safe restart: SIGTERM
notifies live threads, in-flight tool calls get `drain_timeout` to finish, the
container exits 0, and interrupted tasks resume themselves on the next start —
the same contract the systemd unit carries.

## Files from the agent

When the agent mentions a file path in its reply (`/tmp/settings-top.png`,
`out/report.pdf`) and the file exists in its environment, dispatch uploads it
into the thread — images and PDFs show inline. Agents are told this in their
system prompt, so "send me a screenshot" works. Up to 10 files per message,
20 MiB each. Requires the `files:write` bot scope (in the manifest; if you
created the app before it was added, add the scope and reinstall the app).

## Files to the agent

Attach files or images to any message that reaches the agent — the mention
that starts a task, a DM, a reply in the task's thread — and dispatch copies
them into the agent's environment (local folder, container or ssh host alike)
under `/tmp/dispatch/inbox/<task id>/` and appends their paths to the message.
The agent reads them from disk: a pasted screenshot, a log, a PDF, a CSV. A
message with only an attachment works too. Names are made path-safe
(`Screenshot 2026.png` → `Screenshot_2026.png`); a second file of the same
name in one session becomes `image-2.png`. 20 MiB per file; one that cannot be
fetched is reported in the thread and skipped, the rest of the message still
goes through. Requires the `files:read` bot scope (in the manifest; if you
created the app before it was added, add the scope and reinstall the app —
without it every attachment is reported as skipped). Files sent with a bare
`run` are dropped, since the prompt is typed later; send them with the prompt.

## Restarting dispatch

`make service-restart` (or Ctrl-C / `systemctl restart dispatch`) is safe while
agents run:

1. Every live thread gets "⏸️ dispatch is restarting".
2. Agents that are in the middle of a tool call (a test run, a build) are given
   `drain_timeout` (default 2m) to finish that call; then their processes stop.
   Files in the workdir stay.
3. On start, every task that was *mid-execution* is resumed (`--resume`) on its
   own and told to carry on — you do not have to type anything in the threads. A
   task that never got as far as a session is simply run again from its original
   message.
4. A task whose agent had already answered is left alone: its process was only
   being kept alive for a follow-up (`idle_timeout`), so the stop cut nothing
   short and the thread is still waiting for you, not for dispatch.

`status` shows such tasks as *interrupted* until they are picked up. `cancel` is
still immediate. The systemd unit's `TimeoutStopSec` is set above `drain_timeout`.

Auto-resume is on by default and tunable in `[server]`:

| key                  | default | what it does                                         |
|----------------------|---------|------------------------------------------------------|
| `auto_resume`        | `true`  | `false` goes back to "▶️ dispatch is back — reply in this thread to continue" |
| `auto_resume_within` | `12h`   | tasks last touched longer ago than this wait for a reply instead |
| `max_auto_resumes`   | `3`     | consecutive automatic resumes of one task before it waits for a human — a task that keeps taking dispatch down cannot restart-loop |
| `resume_prompt`      | built-in | the message an auto-resumed session is given          |

The counter behind `max_auto_resumes` is cleared as soon as a resumed agent
finishes a turn.

## The decider (optional)

Dispatch's restart rules are blunt on purpose: everything cut mid-execution is
picked up with the same "carry on" sentence. Switching on a decider hands those
judgement calls to a small model — it can leave a task for you with a reason, or
word the resume in the task's own terms. See [DECIDER.md](DECIDER.md).

```toml
[decider]
kind = "claude"       # off (default) | claude | openai
model = "haiku"
uses = ["resume", "permission"]   # question kinds it may answer; [] = never asked
timeout = "15s"
max_per_task = 20
auto_allow = ["Read", "Glob", "Grep", "Bash(go test:*)"]   # see "permission" below
```

`kind = "claude"` runs the `claude` CLI you already have. `kind = "openai"`
talks to any OpenAI-compatible endpoint instead — OpenAI, DeepSeek, Groq,
Mistral, OpenRouter, or a local Ollama/vLLM — and then `model` is the
endpoint's own model name:

```toml
[decider]
kind = "openai"
model = "gpt-4o-mini"                      # or "deepseek-chat", "llama3.2", …
[decider.openai]
base_url = "https://api.openai.com/v1"     # "https://api.deepseek.com/v1", "http://localhost:11434/v1"
api_key = "sk-…"                           # leave out for a local server that needs none
```

The key lives in `config.toml` next to your Slack tokens (the file is
`0600`); nothing is read from the environment. `dispatch doctor` calls the
endpoint's `/models` with that key and fails the check if it is unreachable
or the key is rejected — the one misconfiguration that would otherwise fall
back to the rules silently on every question. No `temperature` or
`response_format` is sent, so reasoning models and minimal endpoints work
as-is.

On a restart it judges each cut-short task from the tail of its own thread —
the last thing you asked, the agent's last words, its recent tool calls, the
files it changed, the tool call that was in flight — and picks one of four:

| verdict | what you see |
|---------|--------------|
| continue | `⏯️ resuming session`, with a prompt naming what the agent was in the middle of |
| ask | a question with **continue** / **drop** buttons; replying with your own words resumes it instead |
| wait | `▶️ dispatch is back — <reason>; reply in this thread to continue` |
| abandon | `⏹️ leaving this task: <reason>` — no restart offers it again, a reply still can |

With `"permission"` in `uses` it also triages approval prompts, so routine
tool calls stop waking you. A prompt means the call is outside what the agent
definition pre-approved, so the decider needs permission you wrote down
yourself: `auto_allow` (same syntax as `allowed_tools` — `Read`,
`Read(/repo/*)`, `Bash(go test:*)`, `Bash(*)`). A command is matched the way a
shell reads it, so `Bash(go test:*)` covers `go test ./... && go test ./cmd`
and `go test ./... 2>&1`, but not `go test ./... && rm -rf .git`, not
`go test ./... > somefile`, and not `go test $(something)`. Paths are cleaned
first, so `Read(/repo/*)` does not cover `/repo/../etc/shadow`. A pattern that
does not parse (`Bash(`, `Bash()`) is a config error, never "every call". A
call outside that list goes straight to the buttons without asking the decider
anything; a call inside it may be approved, and the thread is told:

```
🔓 allowed automatically: `Bash go test ./...` — Run all tests; explicitly requested.
_say `cancel` to stop this task_
```

`auto_allow` is empty by default, so nothing is approved without you until you
list something.

The decider can only ever narrow what the rules already allow:
`auto_resume_within`, `max_auto_resumes` and `auto_allow` are applied before it
is asked, and an answer outside the offered options is discarded. If it fails,
times out or is not configured, dispatch behaves exactly as it does without one.
Every verdict, with its reason, is in the event log and in `status`.

## Environments

**local** — the agent runs on the dispatch host in `workdir` (or a fresh
directory under `workdir_root` per task).

**docker** — the agent runs in a container from `image`, with `workdir`
mounted at `/work` and the process running as your uid so files stay yours.

Name any base image you like. Dispatch makes it agent-ready the first time it
is used and caches the result, so you do not have to build and maintain an
image yourself:

```toml
[definitions.environment]
kind  = "docker"
image = "ubuntu:24.04"
reuse = "thread"
```

The container runs with your login. A container has no agent login of its own,
so before every turn dispatch copies the host's Claude
`~/.claude/.credentials.json` or Codex `~/.codex/auth.json` (the login of the
user running dispatch) into its matching home directory. The CLI inside
refreshes the token itself, and a copy the container refreshed more recently
than the host is left alone. To give a container its own identity instead, put
a key in its env — dispatch then lends nothing:

```toml
[definitions.environment.env]
CLAUDE_CODE_OAUTH_TOKEN = "…"     # from `claude setup-token` on a logged-in machine, or use ANTHROPIC_API_KEY
# CODEX_API_KEY = "…"              # or OPENAI_API_KEY; otherwise run `codex login` on the dispatch host
```

When neither exists the turn ends with `Not logged in`, and dispatch says so
in the thread along with what to do about it.

### GitHub

The container gets your GitHub login the same way. Every provisioned image
carries the `gh` CLI, and before a task starts dispatch writes the host's
`~/.config/gh/hosts.yml` into the container's gh config dir and runs
`gh auth setup-git` there — so `gh pr create`, `gh issue list` and
`git push` all work as you, with no token in the config file.

The token is taken from the first of these that has one:

1. the host's `hosts.yml` (`$GH_CONFIG_DIR`, else `~/.config/gh`), copied
   as it is, enterprise hosts included
2. `gh auth token` on the host, for a login `gh` keeps in the system keyring
3. `GH_TOKEN` / `GITHUB_TOKEN` in dispatch's own environment

A `hosts.yml` inside the container that is newer than the host's is left
alone, so a login made in there survives — when what dispatch lends is the
host's own `hosts.yml` (source 1), whose mtime says when the host last
logged in. A token from sources 2 and 3 has no such file behind it and
counts as current every time, so it is re-lent at every task and a
`gh auth login` made inside the container does not survive.

To give a container an account of its own — a bot, a fine-grained token —
put one in its env and dispatch lends none:

```toml
[definitions.environment.env]
GH_TOKEN = "github_pat_…"
```

The commits get your name too. A fresh container has no `user.name` or
`user.email`, so `git commit` in it stops with "Please tell me who you
are"; dispatch writes the host's identity — your `git config user.name` and
`user.email`, or `GIT_AUTHOR_NAME`/`GIT_AUTHOR_EMAIL` from dispatch's own
environment — into the container's global git config. It is written once
and never overwritten, so an identity a `setup` command or the agent set
stays, and it is lent even to a container with its own token. To choose one
yourself:

```toml
[definitions.environment.env]
GIT_AUTHOR_NAME  = "dispatch"
GIT_AUTHOR_EMAIL = "bot@example.com"     # dispatch then lends no identity
```

With no login anywhere the task still runs; only GitHub is out of reach,
and `gh` says so itself. `dispatch doctor` prints what would be lent and who
a container would commit as.

### Provisioning

With `provision = "auto"` (the default) dispatch installs, as root, into a
throwaway container started from `image`:

- `ca-certificates`, `curl`, `git`, `tar`, and `ripgrep` if the distro has it
- the GitHub CLI (`gh`), from the distro if it packages it, else the
  official release tarball
- Node 18+ if the image has none
- the agent CLI for the definition's `kind` — `claude` or `codex`
- a user with your uid/gid and a writable `$HOME` at `/home/dispatch`, with
  passwordless `sudo`, so the agent can install whatever it turns out to need
  mid-task (what it may actually run is still gated by its permission mode)
- `git config --system safe.directory '*'`, so git will touch the mounted
  workdir even though it belongs to a different uid

The result is committed as `dispatch-env:<hash>` and reused from then on. The
hash covers the base image, the agent, `packages`, `setup` and your uid, so
changing any of them rebuilds and changing none of them costs nothing.
apt, apk, dnf, yum, pacman and zypper images are all handled.

Two escape hatches:

```toml
packages = ["postgresql-client", "jq"]   # extra OS packages
setup    = ["pip install --break-system-packages ruff"]   # extra root commands, run last
```

An image that already carries the agent CLI and git is used **exactly as it
is** — dispatch checks before it builds anything, so a purpose-built image
never gets rewritten (put `gh` in it yourself if you want the GitHub login
lent into it). `provision = "none"` turns the whole thing off.

The first task on a cold image spends about a minute building; every later
task starts instantly. Watch for `docker: provisioning image` in the log.

### Reusing a container

`reuse` decides how long a container lives and who shares it:

| value                 | behaviour                                                              |
|-----------------------|------------------------------------------------------------------------|
| `task` (default)      | a fresh container per task, removed when the task ends                  |
| `thread`              | one container per conversation, kept warm between messages             |
| `definition`          | one container shared by every conversation running that agent          |

A reused container keeps `$HOME` on a named volume, so anything the agent
installed mid-task, its login and its session history survive between messages
— which is what makes Claude and Codex resumes work in Docker at all. Reused
containers and their workdirs are keyed to the scope, so two
threads never share a filesystem.

Containers nobody has touched for `docker.reuse_ttl` (default 24h) are
removed at startup and hourly. Home volumes are kept: the login inside them
is worth more than the disk.

```toml
[docker]
binary    = "docker"
run_args  = ["--network=host"]   # appended to every `docker run`
reuse_ttl = "24h"
```

**ssh** — the agent runs on `host` in `workdir`; `claude` must be installed and
logged in there for the ssh user. `dispatch doctor` checks both. Uses your
`~/.ssh/config`, agent and known_hosts; set `key_path` to pin a key.

## Permission modes

| mode                | behaviour                                                        |
|---------------------|------------------------------------------------------------------|
| `manual` (default)  | every tool not in `allowed_tools` asks in Slack                  |
| `acceptEdits`       | file edits auto-approved, commands still ask                     |
| `auto`              | Claude Code's own heuristics                                     |
| `bypassPermissions` | never asks — only for throwaway containers                       |

`allowed_tools` takes Claude Code rule syntax: `Read`, `Edit`, `Bash(git:*)`, `Bash(npm test)`.

## Surfaces

A *transport* is the wire (Slack, terminal). A *surface* is an interface on it.
Default: one `chat` surface per transport. Add a `feed` to mirror every task's
start, approvals and results into an ops channel — and approve from there:

```toml
[[surfaces]]
name = "chat"
kind = "chat"
transport = "slack"

[[surfaces]]
name = "ops"
kind = "feed"
transport = "slack"
thread = "C0123456789/"     # channel id + "/" posts at top level
approvals = true
```

## Troubleshooting

- **`claude` check fails in doctor** — run `claude` interactively as the service user and `/login`. If dispatch runs under systemd, the unit's `HOME=` must point at that user's home.
- **Bot does not react to mentions** — re-install the app after changing the manifest; make sure the bot was invited to the channel; check `journalctl -u dispatch` for `slack connected`.
- **Buttons do nothing** — Interactivity must be on (the manifest enables it) and Socket Mode must be enabled.
- **Permission prompt never appears, task fails with `permission_denials`** — the definition's `permission_mode` is not `manual`/`acceptEdits`, or `claude` is older than 2.1; dispatch needs the `--permission-prompt-tool stdio` handshake.
- **Files in the docker workdir owned by root** — containers run as your uid:gid by default; this only happens if the docker factory `User` was overridden to `root`.
- **dispatch stopped and never came back** — check `systemctl is-active dispatch` and look for a
  `shutdown: notifying live task` line in `journalctl -u dispatch` with no `Stopping
  dispatch.service` job line above it. That combination means something sent SIGTERM directly
  rather than going through systemd; the usual culprit is an agent cleaning up a test instance
  with a `pgrep -f` / `pkill -f` pattern like `bin/dispatch run`, which also matches
  `/usr/local/bin/dispatch run`. If it dies again seconds after each start, the task that did it
  is auto-resuming and repeating the kill: cancel that thread, or start once with
  `auto_resume = false` to break the loop. `Restart=always`
  and the updater's watchdog both bring it back now; `DISPATCH_UPDATE_WATCHDOG=0` turns the latter
  off if you want to keep it stopped for maintenance.
- Everything dispatch saw is in the SQLite `log` table: `sqlite3 ~/.config/dispatch/dispatch.db 'select seq,kind,substr(payload,1,120) from log order by seq desc limit 20'`.
