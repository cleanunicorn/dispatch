# dispatch — Agent guide

Guidance for AI agents (and humans) contributing to **dispatch**.

dispatch orchestrates coding agents from Slack, a web UI, and the terminal:
one Go binary turns conversations into tasks, runs each task as a coding-agent
session in a local folder, a Docker container, or an SSH host, relays
permission prompts back as buttons, and keeps every event in SQLite so
sessions survive restarts — a restarted dispatch resumes the tasks it cut
short by itself.

This project follows GitHub flow: `main` is always releasable — the deployed
instance auto-updates from it — all work happens on short-lived branches in
worktrees, and every change lands through a reviewed pull request.

Read [README.md](README.md) for the tour, [SETUP.md](SETUP.md) for install and
transports, and [DESIGN.md](DESIGN.md) for the full design. This file is the
operational checklist for *how to work* here.

## Prerequisites

- **Go ≥ 1.25** — the only toolchain `make build`/`make test` need.
- **Python 3** — `make e2e`, `make restart-drill`, and
  `make auto-resume-drill` drive `scripts/*.py`.
- **The `claude` CLI, logged in** — dispatch drives it; the live tests,
  `make e2e`, and the drills need it. `make doctor` checks the login.
- **Docker** (optional) — for the docker environment; its tests skip
  themselves without a live daemon.
- **Node + npm** (optional) — only for the web UI (`make ui`). The Vite build
  is committed under `internal/transport/web/static` and embedded, so
  `go build` never needs Node.
- **The `gh` CLI** — for pull requests (and dispatch itself lends it into
  containers; see the project map).
- **systemd** (Linux) — for `make service-install` / `make update-install`
  deploys.

Config lives at `$DISPATCH_CONFIG`, else `~/.config/dispatch/config.toml`;
[deploy/config.example.toml](deploy/config.example.toml) is the reference for
every key. It holds Slack tokens and is never committed.

## Commands

`make` owns the dev flow. Don't hand-roll the underlying commands.

- **Build:** `make build` — writes `bin/dispatch`
- **Run locally:** `make run` (Slack) · `make run-terminal` (terminal
  transport — the fastest way to try a chat change) · `make run-web` (browser
  UI on `web.listen`)
- **Lint:** `make lint` — gofmt `-l` check + `go vet`; run before finishing a
  change
- **Format:** `make fmt` (then `make tidy` for modules)
- **Test (all):** `make test` — `go test ./...`
- **Test (race):** `make test-race` — `go test -race -count=1 ./...`
- **Test (single):** `go test ./internal/coordinator -run TestName -v`
  (the coordinator is concurrent; use `-race` there)
- **Test (live):** `make test-live`, or the focused forms
  `DISPATCH_LIVE=1 go test -count=1 ./internal/agent/claude` (drives the real
  claude CLI, haiku, ~cents) and
  `DISPATCH_LIVE=1 go test -count=1 ./internal/decider ./internal/coordinator -run Live`
  (real decider verdicts)
- **Test (docker provisioning):**
  `DISPATCH_DOCKER_PROVISION=1 go test ./internal/environment/docker` — builds
  a real image from `ubuntu:24.04` (~60s, downloads packages); skipped
  otherwise
- **End-to-end:** `make e2e` — `scripts/e2e.py`: the whole binary through the
  terminal transport (needs a logged-in claude)
- **Restart drill:** `make restart-drill` — SIGTERM mid-tool-call → drain →
  restart → resume; `make auto-resume-drill` — SIGTERM, then check the task
  finishes itself
- **Web UI:** `make ui` — build into `internal/transport/web/static`
  (**commit the result**); `make ui-dev` — Vite dev server proxying `/api`
- **First-time setup / health:** `make setup` (wizard) · `make doctor`
  (config, claude login, docker, ssh hosts, Slack tokens)
- **Everything else:** `make help` — also lists the systemd install and
  auto-update targets

Always run `make lint` and the test suite before opening a PR.

## Golden rules

1. **Never commit directly to `main`.** Always branch, always PR. `main` is
   what the deployed instance tracks: `dispatch-update.timer` polls it every
   5 minutes, rebuilds, and restarts the service — a broken `main` is a
   broken production within minutes.
2. **Never force-push a shared branch.**
3. **Keep `main` green.** There is no CI on `main` — the local gate
   (`make lint`, `make test`, `make test-race` for anything concurrent) is
   the only gate. Run it before opening a PR.
4. **Use the Makefile.** It is the single source of the dev flow — don't
   hand-roll the underlying commands. If the flow needs to change, change the
   Makefile so everyone stays in sync.
5. **Never disable, skip, or delete a test to make a build pass.** If a test
   is wrong, say so and propose the fix.
6. **Shut dispatch down; never kill it, and never find it by pattern.**
   SIGTERM is the contract: notify live threads, let in-flight tool calls
   finish for `drain_timeout` (default 2m), persist final state, exit 0 —
   interrupted tasks then resume themselves on the next start. `kill -9`
   cuts tool calls mid-write and leaves tasks to pick up by hand. And
   `pgrep -f` / `pkill -f` / `ps | grep` / `killall` match anywhere in the
   command line, so a pattern like `"bin/dispatch run"` also matches the
   deployed `/usr/local/bin/dispatch run` — this has taken the production
   instance down mid-task **twice**. Keep the pid of the process you started
   and signal only that; wait for it to exit:

   ```sh
   env DISPATCH_CONFIG=/tmp/dispatch-test/config.toml bin/dispatch run & pid=$!
   # ... test ...
   kill "$pid"                                   # SIGTERM: drain, persist, exit 0
   while kill -0 "$pid" 2>/dev/null; do sleep 1; done
   ```

   For the deployed service, let systemd do it (`sudo systemctl stop
   dispatch` — SIGTERM, waits `TimeoutStopSec=150`). If you truly have no
   pid, anchor a `pgrep -af "^/abs/path/dispatch run"` to the absolute path
   you launched, read what it matched, and confirm nothing under
   `/usr/local/bin` before signalling. A pattern that *could* match
   `/usr/local/bin/dispatch` is always a bug. See
   [Gotchas](#gotchas) for why an agent working in this repo is uniquely
   positioned to trip this.
7. **The event log is the source of truth.** New state belongs in a
   projection derived from appended records, not in a field that only lives
   in memory.
8. **No new runtime dependencies without a decision.** The only non-stdlib
   deps are `modernc.org/sqlite`, `slack-go/slack`, `BurntSushi/toml`;
   docker and ssh shell out to their CLIs deliberately (no SDKs). Adding one
   is a decision — justify it in the PR and in the package doc of the
   package that uses it.

## Communication

- Always explain the reasoning behind decisions and approaches.
- When claiming something works or is fixed, prove it — a passing test, a
  script that validates the behavior, or a clear explanation of why. Don't just
  assert.
- When uncertain, say so rather than presenting a guess as fact.
- End each response with a confidence indicator: 🟢 High | 🟡 Medium | 🔴 Low

## The GitHub flow, step by step

### 1. Start in a worktree, from an up-to-date `main`

**Always begin work in a git worktree, never directly on a checkout of
`main`.** Create one before the first edit, do the whole change there, and
open the PR from that branch. `.claude/worktrees/` is gitignored and is where
existing worktrees live.

```sh
git fetch origin
git worktree add .claude/worktrees/<short-topic> -b <type>/<short-description> origin/main
```

### 2. Create a branch

Branch names are short, lowercase, hyphenated, and prefixed by intent. Match
the Conventional Commits type you expect the PR to use:

```
feat/<short-description>      # new feature
fix/<short-description>       # bug fix
refactor/<short-description>  # internal change, no behavior change
perf/<short-description>      # performance work
docs/<short-description>      # documentation only
chore/<short-description>     # tooling, deps, housekeeping
```

### 3. Make focused changes

- One logical change per PR — and a whole feature *is* one logical change.
  Ship its code, tests, and docs together; don't split it across a chain of
  dependent PRs. Don't bundle an unrelated refactor into a fix either.
- Match the surrounding style: interfaces defined in package docs, plain
  `go test`, early returns, prose package docs that carry the *why*.
- Keep diffs focused: everything in the diff should serve that one change.
  Focused is about relevance, not size — don't ship half a feature to keep
  the diff short.

### 4. Commit

Commits follow [Conventional Commits](https://www.conventionalcommits.org):

```
type(optional-scope): short imperative description
```

Allowed types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
`build`, `ci`, `chore`, `revert`. Add `!` before the colon for a breaking
change.

```
fix: hold a result until sub-agents finish
feat(coordinator): carry /model switches across resumes
```

Write in the imperative mood ("add", not "added"). Keep the subject under
~72 characters and explain the *why* in the body when it isn't obvious.

### 5. Run the checks locally

There is no CI on `main` — these are the gate:

```sh
make lint           # gofmt check + go vet
make test           # full unit suite
make test-race      # anything concurrent (coordinator especially)
make ui             # if you touched internal/transport/web/ui — commit static/
```

`make e2e`, `make restart-drill`, and `make test-live` need a logged-in
claude; run them when you touched the agent handshake, the shutdown path, or
an end-to-end flow.

### 6. Push and open a PR

```sh
git push -u origin <type>/<short-description>
gh pr create --base main
```

Target **`main`**.

## PR titles

The PR title follows the same Conventional Commits format as commits:

```
type(optional scope)!: description
```

Nothing on `main` enforces this yet (no CI workflows) — the format is
expected of every PR, not machine-checked.

## PR description

Keep it short and useful, and follow this repo's own convention: **a PR
carries a feature's plan as a Progress checkbox list kept current as work
lands, plus the validation that was run, named in the body.**

- **What** changed and **why** (the motivation/problem).
- **Progress:** `- [x]` / `- [ ]` checkboxes for the change's milestones,
  updated as the PR evolves.
- **How to test** / what you ran (`make test-race`, `make e2e`,
  `make restart-drill`, …).
- **Linked issues**: `Closes #123` when it resolves one.

## After opening the PR

- There is no CI to wait for — the reviewer's evidence is the validation
  named in the PR body, so name what you actually ran.
- Remember `main` auto-deploys: don't ask to merge anything you wouldn't run
  on the production instance.
- Address review feedback by pushing more commits to the same branch.

## Project map (where things live)

One Go binary (`cmd/dispatch`: `run` | `setup` | `doctor` | `user`). Data
flows in one loop:

```
transports --Inbound--> surfaces --Intent--> Coordinator --Task--> Executor --> Agent --> Environment
transports <-Outbound-- surfaces <--Event--- Coordinator <-----agent.Event------------------┘
                                                 ↓
                                            Store (SQLite event log + projections)
```

Each layer is an interface defined in the package doc of
`internal/<pkg>/<pkg>.go`; read those files first — they carry the contract,
the concrete packages under them are implementations.

```
cmd/dispatch/            main: subcommands, agent-driver registry
internal/transport/      slack, web, terminal — dumb pipes (see below)
internal/surface/        chat, feed — how humans interact
internal/coordinator/    the only stateful brain
internal/executor/       one worker per task
internal/agent/          vendor protocol → agent.Event (claude driver)
internal/environment/    local, docker, ssh — exec + stream stdio
internal/store/          sqlite: append-only event log + projections
internal/decider/        static, claude, openai — policy verdicts
internal/work/           mines "what is this thread working on" from the log
internal/gh/             lends the host's GitHub login + git identity
internal/config/         config.toml load/validate/textual write-back
deploy/                  systemd units, config.example.toml, Slack manifest,
                         Dockerfile + entrypoint + updater for a containerized
                         instance (deploy/docker)
scripts/                 e2e.py, restart-drill.py, auto-resume-drill.py
docs/                    slack.md, web.md, terminal.md
```

The layers:

- **`transport`** (slack, web, terminal) — dumb on purpose: text,
  prompt-with-choices, files, `ThreadID` (Slack: `"<channel>/<thread_ts>"`;
  web: `"<channel>/<id>"`). It never interprets a message. Files go both
  ways: `Outbound.Files` are uploaded after the text; `Inbound.Files` are the
  attachments a human sent, downloaded by the transport (Slack: `files:read`),
  and the executor copies them into the environment under
  `/tmp/dispatch/inbox/<task>/` and appends the paths to the message.
  `File.Data` is never written to the event log, only the name — so the web
  UI shows a thread's past attachments by name only, and the bytes while the
  page is open. **A conversation belongs to dispatch, not to a transport.**
  The transport that minted the id *hosts* it (`TaskState.Transport`) and
  renders it natively; a `transport.Observer` (the web UI) is shown every
  thread of every transport, and anyone may write into any thread. The
  coordinator relays what humans write to the host and the observers as
  `Outbound.From` (`Decision` for answers), so each transport shows the whole
  exchange its own way — Slack posts "💬 *name* via web: …" and settles a
  prompt's buttons, the web shows a paper slip — and the log keeps one record
  (the inbound), never the relays. An inbound to `"<channel>/"` asks the
  channel's owner (`ChannelLister`) to open a thread (`ThreadOpener`), so a
  web user can start work in a Slack channel. The web transport has no
  memory: lists and history come from the coordinator through
  `transport.History` (`coordinator/threads.go`) — threads with what runs
  them (agent, model, environment, session), the agent list, and messages
  with the agent's tool calls as `Entry.Tool`, paired from the log's `agent`
  records; only the live status line and the open prompt per thread are kept
  in memory. Its users are accounts in the store (`dispatch user add`,
  `web/auth.go`: PBKDF2 hashes, sessions by token hash); the session's name
  is the `Inbound.UserID`. `Inbound.UserName` is the display name when a
  transport has one (Slack: users.info, cached). Keyed messages
  (`Outbound.Key`) are its one stateful feature: Slack edits/deletes the
  message it posted under the key and mirrors the text into the thread's
  assistant status; the terminal redraws the line. `Outbound.Mention`
  addresses one user (Slack: `<@U…>` in front of the text; terminal ignores
  it); the chat surface sets it to the task's `Addressee()` — the `Asker`,
  whoever wrote the message the turn answers, so on a shared thread the
  one waiting is tagged, not the `Requester` who started it — on the lines
  that need a human — prompts, the closing line, errors, the restart notices that
  ask someone to pick a task up — never on the agent's Markdown text, which
  the markdown block does not render mentions in.
- **`surface`** (chat, feed) — everything about *how* humans interact.
  `Handle` turns an inbound message into `[]Intent` (returning `ok=false`
  passes it to the next surface on that transport); `Render` turns a
  coordinator `Event` into outbound messages. Several surfaces share one
  transport, so a new interaction style on Slack is a **new surface, not a
  new Slack client**. The chat surface keeps one live status line per
  running turn (what tool, for how long, how many calls) as a *keyed*
  message (`Outbound.Key`): the transport edits it in place, and the surface
  moves it below every ordinary message and takes it down when the turn ends
  or a prompt is open. Task events reach every surface; `chat` renders only
  the tasks hosted on its own transport (the coordinator copies the result
  to observers), `feed` renders everything into its own thread.
- **`coordinator`** — the only stateful brain: intents → tasks, event
  fan-out to every surface, permission/question decision relay
  (`pending`/`askText` maps keyed by prompt id), guided wizards (`wizard.go`:
  add/edit/delete agent, the bare-`run` agent picker), the end-of-thread
  words (`finish.go`: `review`, `merge`), restart recovery. It is also the
  clock: every `Heartbeat` (10s) while a turn runs it broadcasts
  `EventHeartbeat`, and on a `transport.Reactor` it marks the thread's root
  message ⏳ (working) / ✋ (waiting for a decision) / 📬 (answered, waiting
  for the next message) / ❌ (failed) / ✅ (closed) — one mark, always
  (`mark`, `reactionFor`; `seedMarks` remembers the previous process's marks
  across a restart).
- **`executor`** (local) — one worker per task: provisions the environment,
  starts the agent, keeps a finished turn's process alive for `idle_timeout`
  so follow-ups are instant, then resumes the session with `--resume`.
- **`agent`** (claude) — normalizes vendor protocol into `agent.Event`.
  Nothing above this layer sees stream-json. A definition's `kind`
  (`agent.Kinds()`: claude, codex, opencode) picks the driver from the
  registry in `cmd/dispatch/main.go` (`drivers`); config accepts every kind,
  startup and `doctor` refuse a definition whose kind has no driver in the
  build. Every driver owes the layers above one tool vocabulary
  (`agent.ToolBash`, `ToolEdit`, …: Claude's names) and the dispatch
  permission modes — the mapping tables are in the `agent` package doc — so
  `allowed_tools`, `auto_allow`, the decider and the status line never learn
  a vendor's names.
- **`environment`** (local, docker, ssh) — "I can exec a command and stream
  its stdio", nothing more. Docker and SSH shell out to the `docker` and
  `ssh` CLIs deliberately (no SDKs; the user's ssh config/agent and docker
  context just work). Docker also *provisions*: `Spec.Provision` turns a
  plain base image into an agent-ready one (git, the GitHub CLI, Node, the
  agent CLI, a user with the host uid and a writable `$HOME`) and
  `docker commit`s it as `dispatch-env:<hash>`, built once per hash;
  `Spec.Reuse`/`ReuseKey` keep one container per thread or definition with
  `$HOME` on a volume.
- **`store`** (sqlite) — append-only `Record` log; `TaskState`/`Definition`/
  `FlowState` are projections over it. Crash recovery is a replay: live tasks
  become `interrupted`/`idle`, and the next message resumes the agent
  session.
- **`decider`** (static, claude, openai) — answers the coordinator's two
  policy questions, "resume this cut-short task?" and "allow this tool call
  without asking?", from facts read out of the log. A verdict can only
  narrow: the rules' answer is the floor and every failure falls back to it;
  `auto_allow` is the operator's ceiling and is matched before a decider is
  asked. Off by default. [DECIDER.md](DECIDER.md) is the design and the
  plan; `decider.ParseAllow` is the one parser of `auto_allow` patterns,
  shared by config validation and the coordinator's matcher.

### Invariants worth knowing before editing

- **The event log is the source of truth.** New state belongs in a
  projection derived from appended records, not in a field that only lives
  in memory.
- **Permission prompts are first-class and cross-surface.**
  `agent.EventNeedsPermission` → `surface.Event` (with `PromptID`) →
  `transport.Prompt` → `transport.Decision` → `surface.Decide` →
  `agent.PermissionDecision`. Any surface that rendered a prompt may answer
  it, so prompt ids are namespaced per surface and resolved on a base id in
  the coordinator; a decision is offered to every surface whatever transport
  it came from, so a prompt rendered by `chat-slack` can be answered from
  the web UI.
- **`AskUserQuestion` reuses the same path** as permissions, via
  `EventQuestion` + `Question.Answers`.
- **Output on a thread is ordered, and keyed messages depend on it.** `emit`
  renders and sends under a per-thread lock, because a heartbeat (ticker
  goroutine) and an agent event (executor goroutine) can both touch the
  status line; a surface that posts `[remove status, text, status]` needs
  those to reach the transport in that order. Heartbeat output is not
  written to the event log.
- **Agent text is Markdown, ours is transport markup.** `Outbound.Markdown`
  is set only on what the agent wrote; Slack renders it through a Block Kit
  `markdown` block (falling back to plain text) while dispatch's own lines
  stay mrkdwn (`*bold*`, backticks). Links in dispatch's own lines are
  `transport.Link` — `<url|label>`, Slack's own form, which the web UI's
  mrkdwn renderer reads and the terminal turns into an OSC 8 hyperlink
  (`transport.RenderLinks`), so a pull request reads as `#51` and is still
  one click away on every transport.
- **The Claude handshake is protocol-sensitive**
  (`internal/agent/claude`): spawn with `--permission-prompt-tool stdio`,
  send a `control_request`/`initialize` *first*, then answer each
  `can_use_tool` with a `control_response`. After each turn on a
  subscription login it also sends a `get_usage` control request and emits
  the answer as `agent.EventUsage` (claude 2.1.240+; older CLIs answer with
  an error and are not asked again). A `result` line is not always the
  turn's end: the CLI runs sub-agents behind the model's turn, emits a
  `result` when the model stops, and starts a *second* turn (new `init`,
  another `result`) when the sub-agent finishes. `background.go` tracks this
  session's sub-agents from the `system/task_*` lines and withholds a
  `result` until none is running or waiting to be delivered, so every layer
  above sees one `EventResult` per turn; a held result is delivered if the
  process exits on it. Only sub-agents (`task_type: local_agent`) are
  tracked: a `run_in_background` command may never end (a dev server), and a
  result held for it would never be released. Verified against claude
  2.1.240; `parse_test.go` fixtures pin the stream-json → `agent.Event`
  mapping, `testdata/session.jsonl` is a real captured session and
  `testdata/background.jsonl` a captured sub-agent one.
- **A container borrows the host's login**
  (`internal/agent/claude/login.go`). Before every turn in a docker
  environment the driver copies the host's `~/.claude/.credentials.json`
  into the container's `$HOME/.claude`, keeping the host mtime and leaving a
  copy the container refreshed more recently alone. It lends nothing when
  the definition's env carries a key (`ANTHROPIC_API_KEY`,
  `CLAUDE_CODE_OAUTH_TOKEN`, …), and never to local (same home) or ssh
  (someone else's machine). Without a host login the CLI's `Not logged in`
  result is annotated with what to do. `TestLiveDockerLogin`
  (`DISPATCH_LIVE=1`) proves it in a provisioned `ubuntu:24.04`.
- **A container borrows the host's GitHub login the same way**
  (`internal/gh`). Provisioning puts `gh` in every derived image, and before
  a task starts the executor writes the host's `~/.config/gh/hosts.yml` into
  the container's gh config dir and runs `gh auth setup-git`, so both
  `gh pr create` and `git push` speak for the operator's account. The token
  comes from that hosts.yml, else `gh auth token` (a login the host keeps in
  a keyring), else `GH_TOKEN`/`GITHUB_TOKEN` in dispatch's own environment;
  a hosts.yml in the container newer than the host's is left alone (only the
  first source carries an mtime — a bare token is stamped now and re-lent
  every task). Nothing is lent to local (same home) or ssh (someone else's
  machine), or when the definition's env carries `GH_TOKEN`/`GITHUB_TOKEN`.
  It lends the host's **git identity** with it (`internal/gh/identity.go`):
  `user.name`/`user.email` from the host's git config, else
  `GIT_AUTHOR_*`/`GIT_COMMITTER_*`, written as the container's global git
  config — a fresh container has no committer either, and `git commit` there
  stops before the token is ever used. The identity is lent even when the
  login is the definition's own, and never overwrites one already in the
  container (`GIT_AUTHOR_EMAIL`/`GIT_COMMITTER_EMAIL` in the definition's
  env opts out). Nothing here can fail a task: without a login the agent
  meets `gh`'s own "please run gh auth login", and `dispatch doctor` says
  what would be lent and who a container would commit as.
- **An agent's own commands are pass-through, not features.** `/model opus`,
  `/clear`, `/compact`, a plugin's or the project's — the CLI reads them out
  of the message text itself, so the chat surface implements none of them: a
  message that is not one of dispatch's bare words becomes a `FollowUp` and
  reaches the agent's stdin unchanged (`agent.Run.Send`). That is what makes
  *every* command work, including ones the CLI grows later; `commands` lists
  the session's own (`agent.Event.Commands`, from the init line). Do not add
  a case for one. Two things follow. Slack never delivers a message starting
  with `/` — it looks for a Slack command of that name — so there it is
  written `@dispatch /clear`. And what a command changes lives in the CLI
  *process*: `--resume` starts a new one, so a `/model` choice would be
  undone by the first idle timeout. That one is carried: the claude driver
  reads the name out of a `/model <name>` on its way to the CLI (`modelArg`
  — it does not run the command, the CLI does) and reports it on the turn's
  `EventResult.Model` — a *switch report*, and nothing else: an ordinary
  turn's result carries no model, so anything that wants to name the model a
  turn ran (the chat surface's closing line) reads the init and remembers it
  per thread. The coordinator keeps the switch as `store.TaskState.ModelPin`
  and asks for it again on every resume. The CLI announces the switch
  nowhere else a machine can read it — an English "Set model to Sonnet 5
  for this session only" is the whole of it, and the resolved name only
  appears on the *next* turn's init, which a thread left idle never has.
  Nothing else a command changes is carried.
- **What a thread is working on is mined from its own log**
  (`internal/work`). The closing line of a turn — well or badly ended — and
  an answered `status` carry the repository, branch, pull request and issue,
  and nothing asks GitHub for them. A thread that opened a PR already said
  so in the log: the agent ran `gh pr create` and the URL came back in the
  tool result, the human wrote "fix #47" in the message that started the
  task, the branch was born in a `git switch -c`. So the overview survives a
  restart and still works after a per-task container is gone; what changes
  without the thread saying so (the diff stat, the checks, a merge by
  someone else) is deliberately not here — the one outcome that is,
  `State.Merged`, is a merge *this thread performed*, which is the same kind
  of evidence as everything else in there. Every sighting is graded —
  created here > acted on > mentioned in passing — which is what picks *the*
  PR out of a thread that named a dozen numbers, and sightings of one number
  in one repository collapse into one reference however they were spelled.
  What it *refuses* to believe carries as much: the repository is the one a
  remote command named rather than every `github.com/owner/name` a go.mod
  scrolls past (falling back, when no command named a remote at all, to the
  repository most linked to by a pull request or issue URL), a branch is
  never a command's own flag, a branch is only linkable once the log saw it
  *pushed* (`git switch -c` creates nothing GitHub has heard of, and
  `gh pr list --head x` is a question rather than evidence), and a command
  that came back with a page of pull requests acted on none of them. Above
  all a thread is not working on what it merely *read*: only the output of a
  command that asked GitHub or a remote is evidence at all, so a file the
  agent opened, a `git log`, a grep over a repo full of PR numbers and a
  result whose command was never seen say nothing — and commands are
  recognised where a command *begins*, past any indentation, separator and
  wrapper, so `timeout 60 gh pr create` created a pull request while
  `grep -rn "gh pr create" .` is a grep. A here-document's body is a file
  being written, not commands being run (a fixture full of `git switch -c x`
  named no branch), though what a command hands GitHub under a body flag is
  still read for its links and its "Closes #47". The agent's own prose is
  read for a link and for "Closes #47" but never for a bare "#12", which in
  a report is a quotation — often of the overview line dispatch wrote under
  the last turn. A scan runs while a human waits for the closing line, so
  most records are ruled out on their bytes and never decoded (`maxScan`,
  `mayMatter`), and one record too dear to read is stepped over rather than
  ending the walk. Outbound records are never scanned: dispatch's own
  overview lines carry the references they were mined from and would keep
  re-confirming themselves. The coordinator attaches the answer to
  `surface.Event.Work`; `chat` and `feed` both render it, so the ops channel
  never falls behind the thread.
- **The end of a thread is two words, and neither of them asks for a
  number** (`internal/coordinator/finish.go`). `review` and `merge` read
  what the thread is working on out of its own log (`internal/work`, through
  `overview`), because a thread that opened a pull request already said so
  and pasting the URL back is exactly the friction they remove. `review`
  opens a thread *beside* this one in the same channel
  (`transport.ThreadOpener`, the same path `"<channel>/"` uses) and starts
  the same agent definition there: a new thread is a new session with no
  memory of why each choice was made, which is the only kind of reader
  worth having, and the same definition is the one whose environment can
  check the repository out. The prompt is posted as the new thread's root,
  so it reads as though a human typed it and the pull request's URL is in
  the new thread's log from its first record. `merge` asks the agent for
  all of it — commit what is outstanding and push, resolve a conflict with
  the base branch if `gh pr view --json mergeStateStatus` reports one, then
  run `gh pr merge` — and dispatch runs none of it. Every step is a command,
  which is the agent's job; a `gh` of dispatch's own here would be a second,
  worse GitHub client beside the one already in the container. What dispatch
  does is what it does everywhere else: it **reads the log back**.
  `work.State.Merged` is a `gh pr merge` this thread ran answered by gh's
  own "Merged pull request", and the thread is closed on that sighting and
  on nothing else — an agent that reports success the log cannot confirm
  closes nothing. That is the one *outcome* `internal/work` mines, and it
  belongs there because it is the same kind of evidence as the rest:
  something the thread did and said so in the log. A merge by someone else,
  a merge queue landing it later, the checks — still outside. The prompt
  tells the agent what not to work around: a conflict is a mechanical
  obstacle and gets fixed, while a red check, a missing approval or a branch
  protection rule is somebody's decision and is a thing to report.
  `Merged` is tied to `State.PR`, not to the thread — a thread that merged
  #51 and then opened #52 has merged nothing it is working on now — and the
  pull request it names is the one gh named in the answer, not the one the
  command was handed. The *repository* has to agree only when both sides
  name one: gh qualifies its confirmation ("cleanunicorn/dispatch#51")
  whatever the thread knows, and a thread whose every sighting was a bare
  number — `gh pr view 51`, no remote ever named — has none to compare it
  against, which is exactly the thread `prArg` writes a bare number for.
  There is one repository in play there; nobody wrote it down, and refusing
  the merge for that left the thread open on a pull request that is merged.
  `merge` runs off the inbox goroutine (one at a time per thread,
  `merging`) because the turn takes minutes, and waits for *its own* turn:
  `awaitTurnEnd`/`turnEnded` announce every ending turn by task id
  (`taskSink.OnEvent` on a result, and `drive` on the way out, for a turn
  that never reached the agent at all), and `waitForTurn` lets every other
  one go past — a thread's previous turn is torn down after its keep-alive
  expires, which can be long after this one was asked for. The id is not
  enough on the warm path, where the turn before the merge and the merge
  itself are the *same task*: so the waiter is registered before the "🚢
  asking…" line is posted, and emptied (`drainEnds`) after it, which drops
  an end the post gave time to arrive and cannot cost `merge` its own,
  unsent one. It refuses while a *turn* is running, not while a task is
  merely bound: a finished turn keeps its process warm for `idle_timeout`,
  and that is exactly the moment someone reads the closing line and says
  `merge`. It refuses too while a question is open, whose answer the prompt
  would otherwise become. Both reopen a closed thread themselves, once they
  know they are going to act and never on the way out of a refusal: a
  `merge` with nothing to merge leaves a closed thread closed, the way
  `status` always has. Both words are dispatch's *only when they are the
  whole message*: "review the auth code" and "merge main into this branch"
  are prompts, and stay prompts.
- **Definition vs instance.** `agent.Definition` is stored config; an
  instance is Definition + Environment + session id + thread. Definitions
  are seeded from config into the store on every start, so anything created
  from chat must *also* be written back to `config.toml` or it is lost on
  restart.
- **Config write-back preserves the file.** `config.AppendDefinition` /
  `ReplaceDefinition` / `RemoveDefinition` / `AppendChannel` edit
  `config.toml` textually so comments and formatting survive, validate the
  whole result, and restore the original if it fails to load. Use those
  helpers rather than `config.Save` for chat-driven changes.
- **Closing a thread is thread state, not task state.** `close` writes the
  `closed_threads` table (`store.SetThreadClosed`), which the coordinator
  mirrors in memory. A closed thread is skipped by `seedThreads` and
  `recover`, and `transport.ThreadCloser.Forget` tombstones it on Slack so
  a late "cancelled" notice cannot resurrect it. Any message a human
  addresses to the bot there reopens it. Nothing is ever deleted.
- **Graceful restart is a tested contract.** On SIGTERM: notify live
  threads, drain in-flight tool calls for `drain_timeout`, persist final
  state with a *non-cancelled* context, post "back" notices in `recover()`.
  `cancel` stays immediate. `make restart-drill` guards this.

## Conventions

- **Dependencies:** only non-stdlib deps are `modernc.org/sqlite`,
  `slack-go/slack`, `BurntSushi/toml`. Adding a dependency is a decision —
  justify it in the PR and in the package doc of the package that uses it.
  The web UI (`internal/transport/web/ui`: React, HeroUI, Tailwind,
  react-markdown) is the one JavaScript toolchain; its Vite build is
  committed in `internal/transport/web/static` and embedded, so `go build`
  never needs Node. Run `make ui` after touching `ui/` and commit `static/`.
- **Package docs carry the design rationale** — the layer interfaces live in
  them. Keep them accurate when the contract changes.
- **A feature's plan lives in its PR:** a Progress checkbox list kept
  current as work lands, and the validation that was run (`make test-race`,
  `make e2e`, `make restart-drill`) named in the body.
- **`.claude/worktrees/` holds other worktrees' full copies of the tree**
  (gitignored). Scope greps and `find` to `cmd/ internal/` or exclude it, or
  you will read another branch's files as if they were yours.
- **Naming:** standard Go — files `snake_case.go`, tests `*_test.go` beside
  the code, no testify; plain `go test` with table tests.

## Testing

- **Runner:** plain `go test` (no testify), tests beside the code as
  `*_test.go`.
- **What to cover:** happy path + error paths; the concurrent coordinator
  gets `-race` (`make test-race`).
- **Integration tests skip themselves rather than fail** when the thing they
  need is absent: docker tests need a live daemon, ssh tests spin up a
  throwaway `sshd`, live claude tests need `DISPATCH_LIVE=1` plus a
  logged-in `claude`. Keep that pattern for new integration tests.
- **Fixtures:** `internal/agent/claude/testdata/session.jsonl` is a real
  captured session and `background.jsonl` a captured sub-agent one;
  `parse_test.go` fixtures pin the stream-json → `agent.Event` mapping.
- **Whole-system checks** (need a logged-in claude): `make e2e` (binary
  through the terminal transport), `make restart-drill` and
  `make auto-resume-drill` (SIGTERM mid-tool-call → drain → resume — the
  graceful-restart contract).

## Security

- Never commit secrets, API keys, credentials, or sensitive data. Slack
  tokens and agent credentials live in `config.toml` under
  `~/.config/dispatch/` — outside the repo; `.gitignore` also excludes
  `.env`, `*.db`, and build output under `bin/`.
- Always validate and sanitize user and external input.
- **Credentials lending is deliberately narrow** (see the invariants): a
  container borrows the host's claude and GitHub logins and git identity,
  never over a definition's own key, never to local or ssh. Preserve those
  boundaries when touching `internal/gh` or `internal/agent/claude/login.go`.
- Web UI users are accounts in the store — PBKDF2 password hashes, sessions
  kept by token hash (`internal/transport/web/auth.go`). Never store or log
  a session token in plaintext beyond the initial handshake.

## Gotchas

- **dispatch runs agents that work on *this repo*** — so your cleanup
  command can stop the dispatch that is running you. This is why the
  SIGTERM/pattern rules in [Golden rules](#golden-rules) exist. The unit is
  `Restart=always` precisely because a stray `pkill -f "dispatch run"`
  drains and exits 0 like a clean stop, which `on-failure` would treat as
  one and leave the box down; an explicit `systemctl stop` is still honoured
  (see `deploy/dispatch.service`). The restart does not undo the damage:
  in-flight tool calls were cut mid-write and tasks were left interrupted.
- **`CLAUDE.md` is a symlink to `AGENTS.md`.** Edit `AGENTS.md`; never let
  the two drift.
- **No CI on `main` yet.** The local gate (`make lint`, `make test`,
  `make test-race`) is the only gate, and `main` auto-deploys to the
  production instance — treat a red local run as a broken deploy.
- **`make lint` fails on formatting:** run `make fmt` (and `make tidy` after
  dependency changes) before wondering why.
- **Touching `internal/transport/web/ui/` is not done until `make ui` ran
  and `internal/transport/web/static/` is committed** — the embedded build
  is what ships, and a stale one is invisible in review.
- **Heartbeat output is not written to the event log** — don't "fix" its
  absence, and don't rely on it after a restart.

## Start here

The fastest path to understanding this codebase:

[README.md](README.md) → [DESIGN.md](DESIGN.md) → `cmd/dispatch/main.go` →
the package docs `internal/transport/transport.go`,
`internal/surface/surface.go`, `internal/coordinator/coordinator.go`,
`internal/executor/executor.go`, `internal/agent/agent.go`,
`internal/environment/environment.go`, `internal/store/store.go`,
`internal/decider/decider.go` → [DECIDER.md](DECIDER.md)
