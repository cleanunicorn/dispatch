# dispatch

Orchestrate coding agents from Slack. A single Go binary runs a coordinator
that turns Slack messages into tasks, runs each task as a Claude Code session
in a local folder, a Docker container or an SSH host, relays permission
prompts back as buttons, and keeps every event in SQLite so sessions survive
restarts — a restarted dispatch resumes the tasks it cut short by itself.

```
@dispatch add retries to the HTTP client and run the tests          ⏳
  ▶️ task `a1b2c3d4` started with agent *coder* (local)
  🤖 `claude-sonnet-4-5` · acceptEdits · claude 2.1.239 · subscription · local /srv/work/a1b2c3d4
  @you 🔐 Bash wants to run: go test ./...        [Allow] [Deny]
  Added exponential backoff in client.go; 12 tests pass.
  @you ✅ done · 2m13s · 7 tool calls
  📊 max plan
  ▰▰▱▱▱▱▱▱▱▱ 15% · 5h
  ▰▰▰▱▱▱▱▱▱▱ 28% · 7d
```

## Screenshots

Sanitized Slack screenshots of the main loop: task threads in a channel, a
permission prompt with buttons, and the finished turn with work summary and
usage.

![Slack channel with dispatch task threads](docs/images/slack-channel.svg)

![Slack thread with a permission prompt](docs/images/slack-permission.svg)

![Slack thread after a completed task](docs/images/slack-complete.svg)

While the agent works, the last message in the thread is a live status line
(`🔧 Bash \`go test ./...\` · 1m05s · 6 tool calls`) that is edited in place
every few seconds, and your message carries ⏳ — ✋ while the agent waits for
your answer. When the turn ends the status line goes and the mark becomes 📬:
the thread waits for your next message, or for `close`, which turns it ✅. A
task thread always carries one mark, so a channel shows which ones need you. The
lines that need you — a
permission or question prompt, the closing line, an error — mention whoever
wrote the message that turn answers (on a shared thread, the person who asked
last, not the one who started it), so you can mute the thread and still be
told when to look.
The closing line ends with the charge on an API key; on a Claude subscription
a meter follows it a moment later instead, one bar per plan window — the
5-hour and 7-day windows, and a model's own weekly window when it has one —
showing how much is used after the turn and, past 80%, when the window resets.

- **Transports**: Slack (Socket Mode, no public URL), a web UI, terminal. Telegram later.
  Conversations are shared: the web UI lists every thread, Slack's included, shows what
  was said there and answers its prompts; what you write in the web UI about a Slack thread
  is relayed into Slack, and a thread you start from the web UI in a Slack channel lives in
  Slack. The web UI keeps no history of its own — it reads the event log.
- **Surfaces**: `chat` (threads, commands, approvals), `feed` (ops channel mirror). Several per transport.
- **Agents**: Claude Code via `claude -p` stream-json. A definition's `kind` picks the driver
  (`claude` today; `codex` and `opencode` are accepted by config and provisioning, their drivers
  are on the way); every driver speaks one tool vocabulary, so `allowed_tools` and `auto_allow`
  mean the same whatever runs the task.
- **Environments**: local directory, Docker container, SSH host.
- **Definitions**: named agent configs (model, tools, permission mode, environment); many instances run concurrently, one per thread.

## Quick start

```sh
make build
bin/dispatch setup      # wizard: paths, claude check, Slack tokens, first agent, doctor
bin/dispatch run        # or: bin/dispatch run -terminal, or bin/dispatch run -web
make service          # systemd unit for the current user
```

### Transports

One dispatch can run several at once; conversations are shared between them.

- [Slack](docs/slack.md) — the app manifest, tokens, scopes, what a thread looks like.
- [Web UI](docs/web.md) — `transports = ["slack", "web"]`, accounts (`bin/dispatch user add`), the
  flight-strip board every thread is racked on, the React app.
- [Terminal](docs/terminal.md) — `bin/dispatch run -terminal`: one thread on stdin/stdout, no Slack needed; what `make e2e` drives.

Full instructions, Slack app manifest, docker/ssh notes: [SETUP.md](SETUP.md).
Architecture and conventions: [CLAUDE.md](CLAUDE.md).

## Commands

The same on every transport; on Slack, `@dispatch` is the mention, on the web UI and the terminal just type.

| message                      | effect                                      |
|------------------------------|---------------------------------------------|
| `@dispatch <prompt>`           | start a task with the channel's default agent (replies in a thread under your message) |
| `run <agent> <prompt>`       | start a task with a specific agent          |
| `run`                        | pick the agent from a menu, then type the prompt in the thread |
| `default <agent>`            | make `<agent>` the default for this channel (saved to config.toml); `default` shows it |
| reply in the thread          | follow-up to that task (resumes if idle)    |
| attach a file or image       | copied into the agent's environment, path added to the message (see [SETUP.md](SETUP.md#files-to-the-agent)) |
| button / reply to a question | answers the agent's `AskUserQuestion`       |
| `agent add`                  | define a new agent question by question; saved to config.toml, usable at once |
| `agent edit <name>`          | change an agent's model, environment, permissions, tools or system prompt; `agent edit` picks from a list |
| `agent delete <name>`        | remove an agent after a confirmation (refused while it is a default) |
| `review`                     | review this thread's pull request: dispatch reads it out of the thread's own log, opens a thread beside it in the same channel and starts the same agent there with a fresh session — a reader that has to get its opinion from the diff |
| `merge`                      | get this thread's pull request merged, then close the thread. dispatch asks the agent — commit and push what is outstanding, resolve a conflict with the base branch, run `gh pr merge --squash --delete-branch` — and runs none of it itself. When the turn ends it reads the log back: a `gh pr merge` of this thread's answered by gh's own confirmation closes the thread, and anything else leaves it open. A red check, a missing approval or a branch protection rule is somebody's decision, and the agent is told to report it rather than work around it. `merge rebase` / `merge merge` to not squash |
| `close`                      | stop the task and end the thread: dispatch goes quiet there and marks it ✅ (mention it in the thread to pick it up again) |
| `workflows`                  | list the workflows this dispatch knows: the `[[workflow]]` blocks in config.toml and any plan saved since it started, with what each one is for and how many steps it has |
| `workflow <name> <what you want>` | run that workflow on this thread, step by step. Each step is one real agent turn — here or in a fresh thread beside this one, with the step's own agent and model — and dispatch decides whether it happened by reading the records *that step itself* produced, never the agent's say-so: a pull request opened, a branch pushed, a merge gh confirmed, a report, or a command dispatch runs in the step's own environment. A step that did not happen takes its `on_fail` (ask you, retry, or stop), a `gate` step asks a human before the next one starts, and every transition is in the log — so a restart grades the step it cut short before resending it and work that already happened is not asked for twice. `cancel` stops the run |
| `plan <what you want, and how>` | compose a workflow for this one message instead of looking one up: dispatch hands the ask, the vocabulary and the agents that exist to `server.planner_agent`, then posts what each step *is* — who runs it, where, and what will make it count as done — and starts nothing until you say yes. What comes back goes through the same validation a `[[workflow]]` in config does and runs on the same runner, so a plan naming an agent nobody defined or an `expect` nobody implements is refused exactly the same way. Opt-in: with no `planner_agent` set the word is not dispatch's, and `plan …` is an ordinary prompt for the agent |
| `workflow save <name>`       | keep this thread's last plan as a workflow you can start by name: appended to config.toml (comments and formatting survive, and the file is put back if the result does not load) and added to the running dispatch at once. A plan nobody saves is a draft, and is gone on the next restart |
| `/model opus`, `/clear`, `/compact`, … | the agent's *own* commands, passed through to it as typed — every one its CLI, plugins and the project define, including ones added after this table. On Slack write `@dispatch /clear`: Slack keeps a message that *starts* with `/` for itself |
| `commands`                   | list the commands the agent on this thread accepts (its list, read from the session) |
| `status` / `cancel` / `agent list` / `help` | what they say                 |

## Layout

```
cmd/dispatch            run | setup | doctor
internal/transport    slack, web, terminal       — the wire
internal/surface      chat, feed                 — interaction on a transport
internal/coordinator                              — tasks, intents, event fan-out, decisions
internal/workflow                                 — what a step is and what makes one done (the runner is in coordinator)
internal/executor     local                      — one worker per task, idle timeout
internal/agent        claude                     — stream-json driver, permission handshake
internal/environment  local, docker, ssh         — where processes run
internal/store        sqlite                     — event log + projections
deploy/               systemd unit, Slack manifest, example config
```

## Tests

```sh
make test          # unit tests; docker and ssh tests use a real daemon / throwaway sshd, skipped if absent
make test-live     # also drives the real claude CLI (haiku, a few cents)
scripts/e2e.py bin/dispatch   # whole binary through the terminal transport (needs logged-in claude)
```
