// Package surface defines the interaction layer on top of a transport.
//
// A transport moves messages; a surface decides what they mean and what to
// show. Several surfaces can share one transport: on Slack, a "chat" surface
// turns mentions into task threads while a "feed" surface mirrors approvals
// and results into an ops channel.
//
// Inbound:  transport.Inbound --Surface.Handle--> []Intent --> Coordinator
// Outbound: Coordinator --> Event --Surface.Render--> []transport.Outbound
package surface

import (
	"context"
	"strings"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/executor"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/work"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// Surface interprets inbound traffic and renders coordinator events.
type Surface interface {
	// Name identifies the surface in config and prompt ids.
	Name() string
	// Transport is the name of the transport this surface is bound to.
	Transport() string
	// Handle interprets one inbound message. ok=false means "not mine",
	// and the coordinator offers it to the next surface on the transport.
	Handle(ctx context.Context, in transport.Inbound) (intents []Intent, ok bool)
	// Render converts a coordinator event into messages for the transport.
	// Returning nil posts nothing.
	Render(ev Event) []transport.Outbound
}

// Intent is something a human asked for. Concrete types below.
type Intent interface{ isIntent() }

// RunTask starts a new task on Thread. With neither Agent nor Prompt the
// coordinator asks for them on the thread (agent picker, then prompt).
type RunTask struct {
	Thread transport.ThreadID
	Agent  string // definition name; "" = the channel's or coordinator's default
	Prompt string
	User   string           // transport user id of who asked (transport.Inbound.UserID); the task's requester
	Files  []transport.File // attachments sent with the prompt; copied into the agent's environment
}

// SetDefault sets the default agent of the channel Thread belongs to, or
// with an empty Agent reports the current one.
type SetDefault struct {
	Thread transport.ThreadID
	Agent  string
}

// FollowUp sends Text (and Files) to the task on Thread, resuming it if
// needed. Text may be empty when Files are attached.
type FollowUp struct {
	Thread transport.ThreadID
	Text   string
	User   string           // transport user id of who wrote it; the requester if this starts a task
	Files  []transport.File // attachments sent with the message; copied into the agent's environment
}

// Cancel stops the task on Thread.
type Cancel struct{ Thread transport.ThreadID }

// CloseThread ends the conversation on Thread: the task running there is
// cancelled and the transport stops following the thread, so later chatter
// in it is ignored. Addressing the bot in the thread again reopens it.
type CloseThread struct{ Thread transport.ThreadID }

// Status asks for the state of the task on Thread.
type Status struct{ Thread transport.ThreadID }

// ReviewPR asks for a review of what the thread on Thread is working on.
// The coordinator reads the pull request out of the thread's own log
// (internal/work), opens a new thread beside it in the same channel and
// runs the same agent there, told to review that pull request — which is
// what a human does by hand at the end of a piece of work, in one word.
type ReviewPR struct {
	Thread transport.ThreadID
	User   string // transport user id of who asked; the review task's requester
}

// MergePR gets the thread's pull request merged and closes the thread.
//
// The agent on the thread does all of it: commit and push whatever is
// outstanding, merge the base branch in and resolve if GitHub says it
// conflicts, then run `gh pr merge`. dispatch runs none of those — they
// are commands, which is the agent's job — and instead reads the log back
// when the turn ends (work.State.Merged). The thread is closed on that
// sighting and on nothing else, so an agent that reports success the log
// cannot confirm closes nothing.
//
// It routes around nothing. A red check, a missing approval or a branch
// protection rule is a refusal to report, not to work past, and the
// prompt says so.
type MergePR struct {
	Thread transport.ThreadID
	Method string // as the human typed it; MergeMethod reads it
	User   string
}

// MergeMethod reads the word a human typed after `merge` and returns gh's
// own flag for it. The empty string is the default, and "it" is what
// people write and means nothing.
//
// It lives here rather than in either caller because both need the same
// answer for different reasons: the chat surface asks whether the rest of
// the message is a method at all — "merge main into this branch" is a
// prompt for the agent, not a command — and the coordinator asks which
// flag to put in front of the agent.
func MergeMethod(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "it", "squash":
		return "squash", true
	case "merge":
		return "merge", true
	case "rebase":
		return "rebase", true
	}
	return "", false
}

// RunWorkflow starts the workflow Name on Thread, with Ask as what the
// human wants done — the {{.Ask}} every step's prompt is rendered
// against. An empty Name asks for one from a list.
//
// A workflow is steps dispatch runs and then checks for itself
// (internal/workflow); it is not a longer prompt. The thread it starts on
// is its home: progress is posted there, and a step that did not ask for
// a thread of its own runs there.
type RunWorkflow struct {
	Thread transport.ThreadID
	Name   string
	Ask    string
	User   string
}

// PlanWorkflow writes a workflow for this one message instead of looking
// one up: the human says what they want and how they want it done, and
// dispatch composes the steps out of the agents that exist.
//
// The plan is the same struct a config workflow parses into and goes
// through the same workflow.Validate, so there is no second, looser path
// for a generated one — and it is shown and confirmed before it runs.
type PlanWorkflow struct {
	Thread transport.ThreadID
	Ask    string
	User   string
}

// SaveWorkflow writes the plan last made on Thread into config.toml
// under Name, turning "that worked" into a workflow that can be started
// by name tomorrow. A workflow made from chat that is not written back is
// lost on the next restart, which is why this exists at all.
type SaveWorkflow struct {
	Thread transport.ThreadID
	Name   string
}

// StopWorkflow ends the workflow running on Thread, leaving the thread
// and whatever the last step did exactly where they are.
type StopWorkflow struct{ Thread transport.ThreadID }

// ListWorkflows asks for the workflows that can be started.
type ListWorkflows struct{ Thread transport.ThreadID }

// ListAgents asks for the agent definitions.
type ListAgents struct{ Thread transport.ThreadID }

// ListCommands asks which of its own commands the agent on Thread
// accepts — "/model", "/clear", "/compact" and whatever else its CLI,
// its plugins and the project define. dispatch does not implement any of
// them: a message that is one is passed through to the agent verbatim
// (see agent.Run.Send), so this list is the agent's, read back from what
// it reported when it last started (agent.Event.Commands).
type ListCommands struct{ Thread transport.ThreadID }

// AddAgent starts the guided "agent add" flow on Thread: the coordinator
// asks for each setting in turn and saves the new definition.
type AddAgent struct{ Thread transport.ThreadID }

// EditAgent starts the guided "agent edit" flow on Thread for the
// definition Agent (picked from a list when empty).
type EditAgent struct {
	Thread transport.ThreadID
	Agent  string
}

// DeleteAgent asks to confirm and then removes the definition Agent
// (picked from a list when empty).
type DeleteAgent struct {
	Thread transport.ThreadID
	Agent  string
}

// Decide answers a permission prompt.
type Decide struct {
	PromptID string
	Choice   string
}

// Say posts Text on Thread without involving a task (help, usage errors).
type Say struct {
	Thread transport.ThreadID
	Text   string
}

func (RunTask) isIntent()       {}
func (FollowUp) isIntent()      {}
func (Cancel) isIntent()        {}
func (CloseThread) isIntent()   {}
func (Status) isIntent()        {}
func (ReviewPR) isIntent()      {}
func (MergePR) isIntent()       {}
func (RunWorkflow) isIntent()   {}
func (PlanWorkflow) isIntent()  {}
func (SaveWorkflow) isIntent()  {}
func (StopWorkflow) isIntent()  {}
func (ListWorkflows) isIntent() {}
func (ListAgents) isIntent()    {}
func (ListCommands) isIntent()  {}
func (AddAgent) isIntent()      {}
func (EditAgent) isIntent()     {}
func (DeleteAgent) isIntent()   {}
func (SetDefault) isIntent()    {}
func (Decide) isIntent()        {}
func (Say) isIntent()           {}

// EventKind classifies coordinator events.
type EventKind string

const (
	EventStarted    EventKind = "started"    // task created
	EventResumed    EventKind = "resumed"    // idle session picked up again
	EventAgent      EventKind = "agent"      // Agent carries the agent event
	EventPermission EventKind = "permission" // Agent is a needs_permission; PromptID set
	EventQuestion   EventKind = "question"   // Agent is a question; Question + PromptID set
	EventAllowed    EventKind = "allowed"    // Agent is a needs_permission a decider approved; Text says what ran and why
	EventFinished   EventKind = "finished"   // process exited; Task.Status is final
	EventHeartbeat  EventKind = "heartbeat"  // the task is still at it (or just stopped being); Task.Status says which
	EventClosed     EventKind = "closed"     // the conversation on Thread was closed
	EventReply      EventKind = "reply"      // Text answers a Status/ListAgents/Say
	EventNotice     EventKind = "notice"     // Text is dispatch's own word on Task that asks the human to act (a restart left it for them)
	EventError      EventKind = "error"      // Text explains a failure
	EventWorkflow   EventKind = "workflow"   // a workflow moved: Workflow carries the run, Text says what happened
)

// Event is what the coordinator tells surfaces about.
//
// EventHeartbeat is the coordinator's periodic word on a live task: it
// is sent every few seconds while an agent turn is running, once more
// when the turn leaves that state (a decision is pending, the turn ended,
// dispatch is shutting down), and right after a follow-up was handed to a
// live process. Surfaces use it to show that something is happening —
// the chat surface keeps a status line alive with it — and to take that
// display down when Task.Status is no longer running.
type Event struct {
	Kind     EventKind
	Thread   transport.ThreadID
	Task     *store.TaskState // nil for Reply/Error without a task; EventNotice always carries the task whose asker it addresses
	TaskID   executor.TaskID
	Agent    *agent.Event    // EventAgent, EventPermission
	PromptID string          // EventPermission/EventQuestion: id the Decide intent must echo
	Question *agent.Question // EventQuestion: the single question this event carries
	Text     string          // EventReply, EventNotice, EventAllowed, EventError
	// Work is what the thread is working on — the repository, branch,
	// pull request and issue read back out of the log (internal/work).
	// The coordinator fills it in on the moments a human decides whether
	// to go and look: the end of a turn, and an answered `status`. Nil
	// when the thread has touched no repository, which is most of them.
	Work *work.State
	// Workflow is the run this event is about, on EventWorkflow: which
	// step it is on, what each one did, and whether it is still going.
	// The chat surface renders it as a progress line on the workflow's
	// home thread; the feed renders every one.
	Workflow *workflow.State
}
