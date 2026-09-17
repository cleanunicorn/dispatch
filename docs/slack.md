# Slack

dispatch's first transport: a Slack app over Socket Mode. Outbound HTTPS is all the
machine needs — no public URL, no inbound ports.

Other transports: [Web UI](web.md), [Terminal](terminal.md). Every conversation is shared between them
(see [One conversation, every transport](#one-conversation-every-transport)).

## Create the app

1. Open https://api.slack.com/apps → **Create New App** → **From a manifest**.
2. Pick your workspace, paste `deploy/slack-manifest.yaml`, create.
3. **Basic Information → App-Level Tokens → Generate Token and Scopes**: name it
   `socket`, add scope `connections:write`, generate. Copy the `xapp-…` token.
4. **Install App → Install to Workspace**. Copy the **Bot User OAuth Token** (`xoxb-…`).
5. In Slack, invite the bot to a channel: `/invite @dispatch`.

`bin/dispatch setup` asks for both tokens and writes them to the config; or add them by hand.

## Configure

```toml
[server]
transports = ["slack"]          # add "web" to run the browser UI next to it

[slack]
app_token = "xapp-..."          # app-level token with connections:write
bot_token = "xoxb-..."          # Bot User OAuth token
allowed_users = []              # Slack user ids allowed to command dispatch; empty = everyone

[[channels]]                    # optional: a default agent per channel
id = "C0123ABCDEF"              # channel details → "Channel ID"
agent = "sandbox"
```

`transports` defaults to `["slack"]` when the tokens are set. The config file holds
secrets, keep it `0600`. To find a user id for `allowed_users`: profile → **⋯** →
**Copy member ID**. `default <agent>` in a channel appends a `[[channels]]` block for
you; when a channel appears more than once the last block wins.

Run `bin/dispatch doctor` after editing: it checks both tokens against Slack.

## Use it

| message                      | effect                                      |
|------------------------------|---------------------------------------------|
| `@dispatch <prompt>`           | start a task with the channel's default agent; dispatch replies in a thread under your message |
| DM the bot                   | same, without the mention                   |
| reply in the thread          | follow-up to that task (resumes if idle)    |
| attach a file or image       | copied into the agent's environment, path added to the message ([SETUP.md](../SETUP.md#files-to-the-agent)) |
| button / reply to a question | answers a permission or `AskUserQuestion` prompt |
| `@dispatch /model opus`        | the agent's own commands — `/model`, `/clear`, `/compact`, a plugin's — passed to it as typed; `commands` lists what this agent accepts |

**Slack keeps a leading `/` for itself.** A message that *starts* with `/` is read by Slack
as one of its own commands and never leaves the client, so an agent command is written
after the mention — `@dispatch /clear` — which dispatch strips back to `/clear` before the
agent sees it. In a DM or a thread the mention is otherwise optional; for these it is not.
The web UI and the terminal have no such rule: there you type `/clear`.

The full command list (`run`, `default`, `agent add/edit/delete`, `close`, `status`,
`cancel`, `help`) is in the [README](../README.md#commands) and is the same on every
transport.

### What you see in a thread

- **Live status line** — the last message in the thread: `⏳ thinking · 4s`,
  `🔧 Bash \`go test ./...\` · 1m05s · 6 tool calls`, edited in place and moved below
  each new message. It is the only message dispatch ever deletes.
- **Reactions on your message** — one at a time, and never none: ⏳ while the agent
  works, ✋ while it waits for your decision, 📬 once it has answered and the thread
  waits for your next message, ❌ when the task failed, ✅ once you close the thread
  (`reactions:write`). Every task thread is either being worked on, waiting on you, or
  closed — scan a channel for 📬, ❌ and ✋ to find the ones that need you.
- **Mentions** — the lines that need a human (prompts, the closing line, errors,
  "dispatch is back" notices) tag whoever wrote the message the turn answers — on a
  shared thread, each person on the answers to their own messages — so you can mute the
  thread and still be told when to look. The agent's own text never tags anyone.
- **Agent text is Markdown** — rendered through a Block Kit `markdown` block, so
  headings, `**bold**` and fenced code look right. dispatch's own lines are Slack mrkdwn.
- **Composer status** — "dispatch ⏳ thinking · 4s" above the message box, when the app
  has the *Agents & AI Apps* feature (`assistant_view`, `assistant:write`, the
  `assistant_thread_*` events — all in the manifest). Optional: without it dispatch
  logs one line and keeps the in-thread status line.

More on the thread lifecycle: [While the agent works](../SETUP.md#while-the-agent-works),
[Closing a thread](../SETUP.md#closing-a-thread).

## One conversation, every transport

A thread started in Slack belongs to dispatch, not to Slack. The [web UI](web.md) lists
it, shows its history, and anyone signed in there can write into it or answer its
prompts:

- A message from the web arrives in the thread as `💬 *name* via web: …`.
- A decision made on the web settles the prompt's buttons here, as a click would.
- A thread a web user starts in one of the bot's channels is posted at top level by
  the bot and lives in Slack from then on.

Channel names in the web sidebar need `channels:read` and `groups:read`, DMs `im:read`
(all in the manifest); without them the id stands in.

## Scopes

Bot scopes, all in `deploy/slack-manifest.yaml`. If you created the app before a scope
was added, add it under **OAuth & Permissions** and reinstall the app. A missing scope
never stops dispatch: the call fails, dispatch logs which scope it suspects, and the feature
that needed it is skipped.

| scope | used for | without it |
|-------|----------|------------|
| `app_mentions:read`, `channels:history`, `groups:history`, `im:history` | receiving mentions, thread replies and DMs (the `app_mention`, `message.*` events) | dispatch hears nothing |
| `chat:write` | posting, editing and deleting dispatch's own messages, in channels and DMs | nothing is posted |
| `im:write` | opening a DM with a user | not called today; in the manifest for when it is |
| `reactions:write` | ⏳ / ✋ / 📬 / ❌ / ✅ on the root message | one warning per mark, no reactions |
| `files:read` | attachments you send: dispatch downloads them from Slack and copies them into the agent's environment ([Files to the agent](../SETUP.md#files-to-the-agent)) | every attachment is reported as "could not be fetched" and skipped; the text still reaches the agent |
| `files:write` | files the agent mentions, uploaded into the thread ([Files from the agent](../SETUP.md#files-from-the-agent)) | a "could not upload" line instead of the file |
| `users:read` | the display name next to what someone wrote, on other transports | the user id stands in |
| `assistant:write` | the composer status line (optional, with `assistant_view` and the `assistant_thread_*` events) | one log line, the in-thread status line still works |
| `channels:read`, `groups:read`, `im:read` | channel names in the web UI (optional; `im:read` for the bot's DMs, shown as "DM") | the channel id stands in |

The app-level token (`xapp-…`) needs one scope of its own, `connections:write`, for
Socket Mode; it is not in the manifest, you add it when generating the token (step 3
above).

`bin/dispatch doctor` reads the scopes each token carries (Slack returns them in the
`X-OAuth-Scopes` header of every API call) and lists what is missing: ✘ for a scope
from the first rows of the table, ℹ for an optional one, and a separate line for the
app-level token's `connections:write`.

```
  ✔ slack                  bot @dispatch in acme
  ✘ slack scopes           missing: files:read, im:read — add under OAuth & Permissions and reinstall the app (docs/slack.md#scopes)
  ✔ slack app_token        connections:write
```

## A feed surface

A second surface on the same transport mirrors every task's start, approvals and
results into an ops channel, and lets you approve from there:

```toml
[[surfaces]]
name = "ops"
kind = "feed"
transport = "slack"
thread = "C0123456789/"     # channel id + "/" posts at top level
approvals = true
```

See [Surfaces](../SETUP.md#surfaces).

## Troubleshooting

- **Bot does not react to mentions** — reinstall the app after changing the manifest;
  make sure the bot was invited to the channel; check `journalctl -u dispatch` for
  `slack connected`.
- **Buttons do nothing** — Interactivity and Socket Mode must both be on (the manifest
  enables them).
- **`doctor` marks a token ✘** — the `xapp-` token needs `connections:write`; the `xoxb-`
  token is the *Bot* User OAuth Token, not the user one.
- **Attachment "could not be fetched"** — the app lacks `files:read`, or the file is
  larger than dispatch accepts.
- **`/clear` did nothing / "not a valid command"** — Slack swallowed it. Write
  `@dispatch /clear`; see *Use it* above.

More in [SETUP.md → Troubleshooting](../SETUP.md#troubleshooting).
