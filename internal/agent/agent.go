// Package agent defines the coding-agent abstraction.
//
// An Agent (Claude Code, Codex, OpenCode) is a process started inside an
// Environment that speaks a line-oriented JSON protocol. Each implementation
// translates its native protocol into the normalized Event type so the
// executor and coordinator never see vendor-specific messages.
//
// # What every driver owes the layers above
//
// Tool names. Event.Tool is what allowed_tools, the decider's auto_allow
// ceiling, the status line and the event log reason about, so every driver
// spells the tools dispatch knows in one vocabulary — Claude's, because that
// is what operators already write in config:
//
//	dispatch    Claude Code    Codex                 OpenCode
//	Bash        Bash           command_execution     bash
//	Read        Read           (file read)           read
//	Edit        Edit           file_change           edit
//	Write       Write          file_change (new)     write
//	WebFetch    WebFetch       (network access)      webfetch
//	mcp__*      mcp__<s>__<t>  mcp_tool_call         <server>_<tool>
//
// A tool with no row keeps its vendor name (Glob, Grep, Task, …); the raw
// vendor message is always in Event.Raw. The constants below are the names
// in the first column.
//
// Permission modes. PermissionMode is dispatch's vocabulary; each driver maps
// it onto its own flags:
//
//	dispatch            Claude Code            Codex app-server               OpenCode
//	manual              default                untrusted + read-only          every tool: ask
//	acceptEdits         acceptEdits            on-request + workspace-write   edit/write: allow, rest: ask
//	auto                auto                   on-request + workspace-write   allowed_tools: allow, rest: ask
//	bypassPermissions   bypassPermissions      never + danger-full-access     every tool: allow
//
// Codex app-server currently exposes untrusted, on-request and never
// approval policies; it has no on-failure counterpart. Its on-request policy
// is therefore the closest safe mapping for dispatch's auto mode.
//
// Three Definition keys are claude flags and nothing else: AllowedTools,
// MCPConfig and SubAgents become --allowedTools, --mcp-config and --agents.
// Codex and OpenCode have no equivalent, so a definition of those kinds is
// refused with them rather than silently dropping them (Kind.DropsClaudeFlags,
// enforced in config.validate and skipped by the wizard) — allowed_tools most
// of all, which would otherwise read as a pre-approval nothing honoured.
//
// Whatever the mode, a tool the driver is asked about reaches dispatch as
// EventNeedsPermission and is answered through Run.Decide: a driver must
// never run its vendor's non-interactive mode that auto-approves or
// auto-rejects, because then no prompt would ever reach a human.
package agent

import (
	"context"
	"time"

	"github.com/cleanunicorn/dispatch/internal/environment"
)

// Kind names an agent implementation.
type Kind string

const (
	KindClaude   Kind = "claude"
	KindCodex    Kind = "codex"
	KindOpenCode Kind = "opencode"
)

// Kinds lists every agent kind dispatch knows, in display order. A kind
// being listed does not mean a driver for it is built in; the executor's
// registry says that.
func Kinds() []Kind { return []Kind{KindClaude, KindCodex, KindOpenCode} }

// Valid reports whether k names a known agent kind.
func (k Kind) Valid() bool {
	for _, known := range Kinds() {
		if k == known {
			return true
		}
	}
	return false
}

// DropsClaudeFlags reports whether k's driver has nowhere to put the three
// definition keys that are claude CLI flags: AllowedTools, MCPConfig and
// SubAgents. Codex's app-server and opencode have no equivalent, so config
// refuses them on such a definition and the wizard never asks — accepting
// them would leave allowed_tools looking like it pre-approved tools it did
// not. A kind with no driver in the build answers false; the registry is
// what refuses that definition.
func (k Kind) DropsClaudeFlags() bool { return k == KindCodex || k == KindOpenCode }

// Canonical tool names: what Event.Tool says for the tools dispatch reasons
// about, whatever the vendor calls them (see the package doc).
const (
	ToolBash     = "Bash"
	ToolRead     = "Read"
	ToolEdit     = "Edit"
	ToolWrite    = "Write"
	ToolWebFetch = "WebFetch"
	// ToolMCPPrefix starts the name of an MCP tool: mcp__<server>__<tool>.
	ToolMCPPrefix = "mcp__"
)

// PermissionMode mirrors the agent's own permission levels.
type PermissionMode string

const (
	PermissionManual      PermissionMode = "manual"      // ask for every tool
	PermissionAcceptEdits PermissionMode = "acceptEdits" // auto-accept file edits, ask for the rest
	PermissionAuto        PermissionMode = "auto"
	PermissionBypass      PermissionMode = "bypassPermissions"
)

// Definition is a reusable agent configuration stored in the DB.
// An instance is Definition + Environment + SessionID.
type Definition struct {
	Name           string
	Kind           Kind
	Model          string
	SystemPrompt   string
	AllowedTools   []string
	PermissionMode PermissionMode
	SubAgents      map[string]any // passed through as --agents JSON
	MCPConfig      string         // path or inline JSON for --mcp-config
	Environment    environment.Spec
}

// EventType is the normalized event vocabulary.
type EventType string

const (
	EventInit            EventType = "init"             // the CLI started a turn: the session, or a turn of its own mid-session (claude: after a sub-agent finishes); Session, Model, Mode, Version, Commands, Workdir set
	EventText            EventType = "text"             // assistant text (full or delta)
	EventToolUse         EventType = "tool_use"         // agent invoked a tool
	EventToolResult      EventType = "tool_result"      // tool finished
	EventToolDenied      EventType = "tool_denied"      // the agent's own policy refused a tool call; Tool, ToolID, Text (reason) set
	EventNeedsPermission EventType = "needs_permission" // agent blocked on approval
	EventQuestion        EventType = "question"         // agent asks the human (AskUserQuestion); Questions set
	EventResult          EventType = "result"           // turn finished
	EventError           EventType = "error"
	EventUsage           EventType = "usage" // the plan's usage after a turn; Usage set. Follows a result on subscription sessions; not a state change
)

// Event is one normalized message from an agent.
//
// An Event is written to the log as JSON with no field tags, so its field
// names are the wire format. internal/work decodes logged events into a
// narrow struct of its own — Type, Text, ToolInput, ToolID — to avoid
// paying for Raw on every record of a thread, and renaming or tagging one
// of those four here would compile everywhere and quietly stop the work
// overview finding anything. TestNarrowEventMatchesAgentEvent pins the
// two together.
type Event struct {
	Type      EventType
	At        time.Time
	Session   string // agent-native session id, used for Resume
	Text      string // EventText, EventError, EventResult summary
	Tool      string // EventToolUse / EventToolResult / EventToolDenied / EventNeedsPermission
	ToolInput map[string]any
	ToolID    string     // correlates ToolUse, ToolResult, NeedsPermission
	ParentID  string     // non-empty when emitted by a sub-agent
	Partial   bool       // EventText: true for streaming deltas
	Questions []Question // EventQuestion
	Files     []File     // files the agent referred to, fetched from its environment
	Cost      float64    // EventResult: USD at API list prices (see Billing)
	Usage     *Usage     // EventUsage
	Commands  []string   // EventInit: the agent's own commands this session accepts (see Run.Send)
	// Model is the model the session runs on: what it resolved to
	// (EventInit), and on EventResult the one this turn switched it to,
	// set only when a human asked the agent for the switch in its own
	// words. The switch itself lives in the agent process, so the layer
	// that keeps the session (store.TaskState.ModelPin) asks for this
	// model again every time it resumes.
	Model   string
	Mode    PermissionMode // EventInit: permission mode the agent runs with
	Version string         // EventInit: agent CLI version
	Workdir string         // EventInit: working directory the agent reports
	Billing Billing        // EventInit, EventResult: how this session is paid for
	Raw     []byte         // vendor message, kept for the event log
}

// Billing says whether Cost is a real charge or an API-equivalent estimate.
// On a subscription the estimate is not what the human pays; what they
// want to know is how much of the plan is left, which is Usage.
type Billing string

const (
	BillingUnknown      Billing = ""
	BillingAPIKey       Billing = "api_key"      // metered; Cost is what was charged
	BillingSubscription Billing = "subscription" // flat plan; Cost is what it would have cost at API rates
)

// Usage is how much of a subscription plan its rate-limit windows have
// used, read after a turn. It arrives as its own EventUsage, after the
// result, so the turn never waits for the lookup; a metered (API-key)
// session never sends one, and neither does an agent that could not say
// (an older CLI, a failed lookup) — then Cost is all there is.
type Usage struct {
	Plan    string        // as the vendor names it: claude "pro", "max", "team", "enterprise"; codex "plus", "pro", "prolite", "business", …; "" when unknown
	Windows []UsageWindow // display order: the short window, the weekly one, then per-model weekly windows
}

// UsageWindow is one rate-limit window of a plan.
type UsageWindow struct {
	Name     string    // "5h", "7d", or a model's name for its own weekly window
	Used     float64   // percent of the window used, 0–100
	ResetsAt time.Time // when the window resets; zero when unknown
}

// File is an attachment pulled out of the agent's environment.
type File struct {
	Name string // base name
	Path string // path as the agent wrote it
	Data []byte `json:"-"` // contents; not written to the event log
}

// Question is one question an agent asks the human.
type Question struct {
	Header      string
	Text        string
	Options     []Option
	MultiSelect bool
}

// Option is one selectable answer.
type Option struct {
	Label       string
	Description string
}

// PermissionDecision answers an EventNeedsPermission or EventQuestion.
type PermissionDecision struct {
	ToolID string
	Allow  bool
	Reason string
	// Answers maps Question.Text to the chosen label (or free text) for
	// EventQuestion; nil for plain permissions.
	Answers map[string]string
}

// Run is a live agent turn.
type Run interface {
	// Events streams normalized events until the turn ends. Closed on exit.
	Events() <-chan Event
	// Send delivers a follow-up user message into the running session,
	// verbatim. An agent CLI reads its own commands out of that text —
	// "/model opus", "/clear", "/compact", anything the vendor or a
	// plugin defines — and runs them itself instead of prompting the
	// model, so dispatch supports all of them by passing the text through
	// and none of them by name. What they change is the CLI process's own
	// state, which usually lasts only as long as the process: see
	// store.TaskState.ModelPin for the one dispatch carries across a
	// resume. EventInit.Commands lists what the session accepts.
	Send(ctx context.Context, text string) error
	// Decide answers a pending permission request.
	Decide(ctx context.Context, d PermissionDecision) error
	// Stop terminates the agent process.
	Stop() error
}

// Agent is the interface every agent implementation provides.
type Agent interface {
	Kind() Kind
	// Start begins a new session in env with the given prompt.
	Start(ctx context.Context, env environment.Environment, def Definition, prompt string) (Run, error)
	// Resume continues an existing session.
	Resume(ctx context.Context, env environment.Environment, def Definition, session, prompt string) (Run, error)
}
