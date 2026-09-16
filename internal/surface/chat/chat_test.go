package chat

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

func TestFormatCost(t *testing.T) {
	cases := map[agent.Billing]string{
		agent.BillingSubscription: "", // nobody's bill; the usage meter says what it cost
		agent.BillingAPIKey:       "$2.269",
		agent.BillingUnknown:      "$2.269",
	}
	for b, want := range cases {
		if got := FormatCost(&agent.Event{Cost: 2.269, Billing: b}); got != want {
			t.Errorf("%q: got %q want %q", b, got, want)
		}
	}
}

// A usage event renders as a meter per window for the chat, bar first so
// the bars line up on a phone, and a short phrase for the feed. A nearly spent window says when it resets.
func TestUsageMeter(t *testing.T) {
	at := time.Date(2026, 8, 23, 6, 0, 0, 0, time.UTC)
	usage := &agent.Usage{Plan: "max", Windows: []agent.UsageWindow{
		{Name: "5h", Used: 3, ResetsAt: at.Add(80 * time.Minute)},
		{Name: "7d", Used: 26, ResetsAt: at.Add(5 * 24 * time.Hour)},
		{Name: "Fable", Used: 37.4},
	}}
	spent := &agent.Usage{Windows: []agent.UsageWindow{
		{Name: "5h", Used: 92, ResetsAt: at.Add(80 * time.Minute)},
		{Name: "7d", Used: 85, ResetsAt: at.Add(5*24*time.Hour + 3*time.Hour)},
		{Name: "Fable", Used: 100, ResetsAt: at.Add(-time.Minute)}, // already reset: nothing to promise
	}}
	cases := []struct {
		name        string
		ev          agent.Event
		meter, text string
	}{
		{"windows", agent.Event{At: at, Usage: usage},
			"📊 max plan\n▱▱▱▱▱▱▱▱▱▱ 3% · 5h\n▰▰▰▱▱▱▱▱▱▱ 26% · 7d\n▰▰▰▰▱▱▱▱▱▱ 37% · Fable", "5h 3% · 7d 26% · Fable 37%"},
		{"nearly spent", agent.Event{At: at, Usage: spent},
			"📊 plan usage\n▰▰▰▰▰▰▰▰▰▱ 92% · 5h · resets in 1h20m\n▰▰▰▰▰▰▰▰▰▱ 85% · 7d · resets in 5d 3h\n▰▰▰▰▰▰▰▰▰▰ 100% · Fable",
			"5h 92% (resets in 1h20m) · 7d 85% (resets in 5d 3h) · Fable 100%"},
		{"empty", agent.Event{At: at, Usage: &agent.Usage{Plan: "max"}}, "", ""},
		{"none", agent.Event{At: at}, "", ""},
	}
	for _, c := range cases {
		if got := UsageMeter(&c.ev); got != c.meter {
			t.Errorf("meter %s: got %q want %q", c.name, got, c.meter)
		}
		if got := FormatUsage(&c.ev); got != c.text {
			t.Errorf("text %s: got %q want %q", c.name, got, c.text)
		}
	}
}

func TestHelpListsClose(t *testing.T) {
	if !strings.Contains(help, "`close`") {
		t.Fatal("help does not mention close")
	}
}

func TestHandleCommands(t *testing.T) {
	s := New("chat", "slack", false)
	th := transport.ThreadID("C1/1.0")
	cases := []struct {
		text string
		want surface.Intent
	}{
		{"run coder fix the build", surface.RunTask{Thread: th, Agent: "coder", Prompt: "fix the build"}},
		{"run coder", surface.RunTask{Thread: th, Agent: "coder"}},
		{"run", surface.RunTask{Thread: th}},
		{"default", surface.SetDefault{Thread: th}},
		{"default coder", surface.SetDefault{Thread: th, Agent: "coder"}},
		{"fix the build", surface.FollowUp{Thread: th, Text: "fix the build"}},
		{"close", surface.CloseThread{Thread: th}},
		{"Close", surface.CloseThread{Thread: th}},
		{"cancel", surface.Cancel{Thread: th}},
		{"agent", surface.ListAgents{Thread: th}},
		{"commands", surface.ListCommands{Thread: th}},
		{"cmds", surface.ListCommands{Thread: th}},
		// The agent's own commands are not dispatch's: they pass through.
		{"/clear", surface.FollowUp{Thread: th, Text: "/clear"}},
		{"/model opus", surface.FollowUp{Thread: th, Text: "/model opus"}},
		{"/compact keep the plan", surface.FollowUp{Thread: th, Text: "/compact keep the plan"}},
		{"/a-plugin-command --flag", surface.FollowUp{Thread: th, Text: "/a-plugin-command --flag"}},
		{"agent list", surface.ListAgents{Thread: th}},
		{"agent add", surface.AddAgent{Thread: th}},
		{"agent edit", surface.EditAgent{Thread: th}},
		{"agent edit coder", surface.EditAgent{Thread: th, Agent: "coder"}},
		{"Agent Update coder", surface.EditAgent{Thread: th, Agent: "coder"}},
		{"agent delete coder", surface.DeleteAgent{Thread: th, Agent: "coder"}},
		{"agent rm", surface.DeleteAgent{Thread: th}},
		{"workflows", surface.ListWorkflows{Thread: th}},
		{"workflow feature ship the thing", surface.RunWorkflow{Thread: th, Name: "feature", Ask: "ship the thing"}},
		{"workflow stop", surface.StopWorkflow{Thread: th}},
		// A workflow's ask is everything after the name, so a prompt with
		// spaces survives whole; and neither word eats a sentence that
		// only starts like one.
		{"workflow feature  fix  two spaces", surface.RunWorkflow{Thread: th, Name: "feature", Ask: "fix  two spaces"}},
		{"workflows are listed here", surface.FollowUp{Thread: th, Text: "workflows are listed here"}},
		// An unknown name stays a RunWorkflow: the coordinator says there
		// is no such workflow, rather than silently prompting the agent
		// with half a sentence.
		{"workflow through the night", surface.RunWorkflow{Thread: th, Name: "through", Ask: "the night"}},
		{"add agent config to the readme", surface.FollowUp{Thread: th, Text: "add agent config to the readme"}},
		{"delete the old files", surface.FollowUp{Thread: th, Text: "delete the old files"}},
		{"edit main.go", surface.FollowUp{Thread: th, Text: "edit main.go"}},
	}
	for _, c := range cases {
		got, ok := s.Handle(context.Background(), transport.Inbound{Transport: "slack", Thread: th, Text: c.text})
		if !ok || len(got) != 1 || !reflect.DeepEqual(got[0], c.want) {
			t.Errorf("%q → %+v (ok=%v), want %+v", c.text, got, ok, c.want)
		}
	}
	for _, text := range []string{"default a b", "agent add now", "agent edit coder to use opus", "agent delete coder please", "agent frobnicate", "agent list all", "add agent", "edit agent coder", "delete agent"} {
		got, _ := s.Handle(context.Background(), transport.Inbound{Transport: "slack", Thread: th, Text: text})
		if say, ok := got[0].(surface.Say); !ok || !strings.Contains(say.Text, "usage") {
			t.Errorf("%q → %+v", text, got)
		}
	}
}

func TestHandleCarriesUser(t *testing.T) {
	s := New("chat", "slack", false)
	in := transport.Inbound{Transport: "slack", Thread: "C1/1.0", UserID: "U42", Text: "run coder fix it"}
	if got, _ := s.Handle(context.Background(), in); got[0].(surface.RunTask).User != "U42" {
		t.Errorf("run: user = %q", got[0].(surface.RunTask).User)
	}
	in.Text = "and the tests"
	if got, _ := s.Handle(context.Background(), in); got[0].(surface.FollowUp).User != "U42" {
		t.Errorf("follow-up: user = %q", got[0].(surface.FollowUp).User)
	}
}

func TestRenderMentionsRequester(t *testing.T) {
	th := transport.ThreadID("C1/1.0")
	task := &store.TaskState{ID: "t1", Thread: th, Requester: "U42", Status: store.StatusRunning, Definition: agent.Definition{Name: "coder", Kind: agent.KindClaude, Environment: environment.Spec{Kind: environment.KindLocal}}}
	ev := func(kind surface.EventKind, a *agent.Event) surface.Event {
		return surface.Event{Kind: kind, Thread: th, TaskID: task.ID, Task: task, Agent: a, PromptID: "p1", Question: &agent.Question{Text: "Which?"}}
	}
	cases := []struct {
		name string
		ev   surface.Event
		want string // Mention of the one message that is not the status line
	}{
		{"started", ev(surface.EventStarted, nil), ""},
		{"text", ev(surface.EventAgent, &agent.Event{Type: agent.EventText, Text: "hi"}), ""},
		{"tool", ev(surface.EventAgent, &agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolInput: map[string]any{"command": "ls"}}), ""},
		{"permission", ev(surface.EventPermission, &agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolInput: map[string]any{"command": "rm"}}), "U42"},
		{"question", ev(surface.EventQuestion, nil), "U42"},
		{"result", ev(surface.EventAgent, &agent.Event{Type: agent.EventResult, Cost: 0.1}), "U42"},
		{"agent error", ev(surface.EventAgent, &agent.Event{Type: agent.EventError, Text: "boom"}), "U42"},
		{"error", surface.Event{Kind: surface.EventError, Thread: th, TaskID: task.ID, Task: task, Text: "executor: start environment: no docker"}, "U42"},
		{"notice", surface.Event{Kind: surface.EventNotice, Thread: th, TaskID: task.ID, Task: task, Text: "▶️ dispatch is back — reply to continue"}, "U42"},
		{"reply", surface.Event{Kind: surface.EventReply, Thread: th, TaskID: task.ID, Task: task, Text: "task `t1` — status *idle*"}, ""},
		{"failed", surface.Event{Kind: surface.EventFinished, Thread: th, TaskID: task.ID, Task: &store.TaskState{Requester: "U42", Status: store.StatusFailed}}, "U42"},
		{"cancelled", surface.Event{Kind: surface.EventFinished, Thread: th, TaskID: task.ID, Task: &store.TaskState{Requester: "U42", Status: store.StatusCancelled}}, ""},
		{"no task", surface.Event{Kind: surface.EventQuestion, Thread: th, PromptID: "p2", Question: &agent.Question{Text: "Which agent?"}}, ""},
	}
	for _, c := range cases {
		s := New("chat", "slack", true)
		out := lines(s.Render(c.ev))
		if len(out) != 1 {
			t.Fatalf("%s: got %d messages: %+v", c.name, len(out), out)
		}
		if out[0].Mention != c.want {
			t.Errorf("%s: mention = %q want %q", c.name, out[0].Mention, c.want)
		}
		if out[0].Mention != "" && out[0].Markdown {
			t.Errorf("%s: a mention on Markdown text, which Slack's markdown block does not render", c.name)
		}
	}
}

// TestRenderMentionsAsker: a turn someone other than the requester asked
// for addresses them; a task without an asker falls back to the requester.
func TestRenderMentionsAsker(t *testing.T) {
	th := transport.ThreadID("C1/1.0")
	for _, c := range []struct {
		requester, asker, want string
	}{
		{"U42", "U7", "U7"},
		{"U42", "", "U42"},
		{"", "", ""},
	} {
		task := &store.TaskState{ID: "t1", Thread: th, Requester: c.requester, Asker: c.asker, Definition: agent.Definition{Name: "coder"}}
		s := New("chat", "slack", true)
		out := lines(s.Render(surface.Event{Kind: surface.EventAgent, Thread: th, TaskID: task.ID, Task: task, Agent: &agent.Event{Type: agent.EventResult}}))
		if len(out) != 1 || out[0].Mention != c.want {
			t.Errorf("requester %q asker %q: got %+v, want mention %q", c.requester, c.asker, out, c.want)
		}
	}
}

func TestPermissionPromptIsCutWithItsFenceClosed(t *testing.T) {
	s := New("chat", "slack", false)
	task := &store.TaskState{ID: "t1", Requester: "U42", Definition: agent.Definition{Name: "coder"}}
	ev := surface.Event{Kind: surface.EventPermission, Thread: "C1/1.0", TaskID: "t1", Task: task, PromptID: "p1",
		Agent: &agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolInput: map[string]any{"command": strings.Repeat("中", 3000)}}}
	out := lines(s.Render(ev))
	if len(out) != 1 || !strings.HasSuffix(out[0].Text, "…```") || !utf8.ValidString(out[0].Text) || len(out[0].Text) > 2600 {
		t.Fatalf("cut prompt = %d bytes, valid=%v, tail %q", len(out[0].Text), utf8.ValidString(out[0].Text), out[0].Text[len(out[0].Text)-12:])
	}
}

func TestRenderInit(t *testing.T) {
	task := &store.TaskState{Definition: agent.Definition{Name: "coder", Kind: agent.KindClaude, Environment: environment.Spec{Kind: environment.KindDocker, Image: "dispatch/dev", Workdir: "/cfg"}}}
	full := &agent.Event{Type: agent.EventInit, Model: "claude-haiku-4-5-20251001", Mode: agent.PermissionAcceptEdits,
		Version: "2.1.239", Billing: agent.BillingSubscription, Workdir: "/work"}
	const fullLine = "🤖 `claude-haiku-4-5-20251001` · acceptEdits · claude 2.1.239 · subscription · docker dispatch/dev /work"
	cases := []struct {
		name    string
		verbose bool
		agent   *agent.Event
		want    string // "" = nothing posted
	}{
		{"full", true, full, fullLine},
		// A quiet surface posts it too: it is the answer to "what am I talking to".
		{"quiet", false, full, fullLine},
		// Older CLIs report less; the configured workdir stands in.
		{"sparse", true, &agent.Event{Type: agent.EventInit, Model: "m", Mode: agent.PermissionManual}, "🤖 `m` · manual · docker dispatch/dev /cfg"},
		// An agent that reports nothing beyond a session id has nothing to say.
		{"bare", true, &agent.Event{Type: agent.EventInit, Session: "s"}, ""},
		// A sub-agent's init is not the session the human talks to.
		{"sub-agent", true, &agent.Event{Type: agent.EventInit, Model: "m", ParentID: "toolu_1"}, ""},
	}
	for _, c := range cases {
		s := New("chat", "slack", c.verbose)
		out := lines(s.Render(surface.Event{Kind: surface.EventAgent, Thread: "C1/1.0", Task: task, Agent: c.agent}))
		got := ""
		if len(out) > 1 {
			t.Fatalf("%s: got %d messages", c.name, len(out))
		} else if len(out) == 1 {
			got = out[0].Text
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestRenderInitOncePerThread(t *testing.T) {
	s := New("chat", "slack", false)
	task := &store.TaskState{Definition: agent.Definition{Name: "coder", Kind: agent.KindClaude, Environment: environment.Spec{Kind: environment.KindLocal}}}
	init := func(thread transport.ThreadID, model string) surface.Event {
		return surface.Event{Kind: surface.EventAgent, Thread: thread, Task: task, Agent: &agent.Event{Type: agent.EventInit, Model: model, Mode: agent.PermissionManual}}
	}
	if got := lines(s.Render(init("C1/1.0", "m1"))); len(got) != 1 {
		t.Fatalf("first init: %d messages", len(got))
	}
	// An idle resume reports the same details: nothing new to say.
	if got := lines(s.Render(init("C1/1.0", "m1"))); got != nil {
		t.Errorf("repeat init posted again: %q", got[0].Text)
	}
	// A change is news; another thread has not seen it at all.
	if got := lines(s.Render(init("C1/1.0", "m2"))); len(got) != 1 {
		t.Errorf("changed init not posted")
	}
	if got := lines(s.Render(init("C2/1.0", "m1"))); len(got) != 1 {
		t.Errorf("other thread not posted")
	}
}

// lines drops the status line (keyed messages) from rendered output.
func lines(out []transport.Outbound) []transport.Outbound {
	var kept []transport.Outbound
	for _, o := range out {
		if o.Key == "" {
			kept = append(kept, o)
		}
	}
	return kept
}

// TestStatusLine follows one turn: the live line appears with the start,
// follows tool calls (throttled; heartbeats always redraw it), moves below
// every ordinary message, leaves while a prompt is open, comes back on the
// heartbeat that follows the answer, and ends in the closing line.
func TestStatusLine(t *testing.T) {
	s := New("chat", "slack", false)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	th := transport.ThreadID("C1/1.0")
	task := &store.TaskState{ID: "t1", Thread: th, Status: store.StatusRunning, Definition: agent.Definition{Name: "coder", Kind: agent.KindClaude, Environment: environment.Spec{Kind: environment.KindLocal}}}
	agentEv := func(a agent.Event) surface.Event {
		return surface.Event{Kind: surface.EventAgent, Thread: th, TaskID: task.ID, Task: task, Agent: &a}
	}
	texts := func(out []transport.Outbound) []string {
		var got []string
		for _, o := range out {
			switch {
			case o.Key != "" && o.Text == "":
				got = append(got, "[remove "+o.Key+"]")
			case o.Key != "":
				got = append(got, "["+o.Key+"] "+o.Text)
			default:
				got = append(got, o.Text)
			}
		}
		return got
	}
	check := func(name string, out []transport.Outbound, want ...string) {
		t.Helper()
		got := texts(out)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want)
		}
	}

	check("started", s.Render(surface.Event{Kind: surface.EventStarted, Thread: th, TaskID: task.ID, Task: task}),
		"▶️ task `t1` started with agent *coder* (local)", "[status] ⏳ starting · 0s")
	now = now.Add(4 * time.Second)
	check("init", s.Render(agentEv(agent.Event{Type: agent.EventInit, Model: "m", Mode: agent.PermissionManual})),
		"[remove status]", "🤖 `m` · manual · local", "[status] ⏳ thinking · 4s")
	now = now.Add(time.Second)
	// A tool call right after a redraw waits for the next heartbeat.
	check("tool throttled", s.Render(agentEv(agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "1", ToolInput: map[string]any{"command": "go test ./...\nand more"}})))
	now = now.Add(5 * time.Second)
	check("heartbeat", s.Render(surface.Event{Kind: surface.EventHeartbeat, Thread: th, TaskID: task.ID, Task: task}),
		"[status] 🔧 Bash `go test ./...…` · 10s · 1 tool call")
	now = now.Add(5 * time.Second)
	check("tool result", s.Render(agentEv(agent.Event{Type: agent.EventToolResult, ToolID: "1", Text: "ok"})),
		"[status] ⏳ thinking · 15s · 1 tool call")
	// A refusal by the agent's own policy is posted even when tool calls are not.
	check("tool denied", s.Render(agentEv(agent.Event{Type: agent.EventToolDenied, Tool: "Bash", ToolID: "1b", Text: "Blocked by classifier"})),
		"[remove status]", "🚫 *Bash* refused by the agent's own policy: Blocked by classifier", "[status] ⏳ thinking · 15s · 1 tool call")
	out := s.Render(agentEv(agent.Event{Type: agent.EventText, Text: "**Looking** at it"}))
	check("text", out, "[remove status]", "**Looking** at it", "[status] ⏳ thinking · 15s · 1 tool call")
	if !out[1].Markdown {
		t.Error("agent text not flagged as Markdown")
	}
	perm := agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolID: "2", ToolInput: map[string]any{"command": "rm -rf build"}}
	check("permission", s.Render(surface.Event{Kind: surface.EventPermission, Thread: th, TaskID: task.ID, Task: task, Agent: &perm, PromptID: "t1:2"}),
		"[remove status]", "🔐 *Bash* wants to run:\n```rm -rf build```")
	waiting := *task
	waiting.Status = store.StatusWaitingPermission
	check("heartbeat while waiting", s.Render(surface.Event{Kind: surface.EventHeartbeat, Thread: th, TaskID: task.ID, Task: &waiting}))
	check("status reply while waiting", s.Render(surface.Event{Kind: surface.EventReply, Thread: th, Text: "task `t1`"}), "task `t1`")
	now = now.Add(30 * time.Second)
	// The answer came: the coordinator's heartbeat brings the line back,
	// showing the tool that was approved.
	check("heartbeat after answer", s.Render(surface.Event{Kind: surface.EventHeartbeat, Thread: th, TaskID: task.ID, Task: task}),
		"[status] 🔧 Bash `rm -rf build` · 45s · 1 tool call")
	now = now.Add(20 * time.Second)
	check("result", s.Render(agentEv(agent.Event{Type: agent.EventResult, Text: "done", Cost: 0.0125, Billing: agent.BillingAPIKey})),
		"[remove status]", "✅ done · 1m05s · 1 tool call · m · $0.013")
	check("finished", s.Render(surface.Event{Kind: surface.EventFinished, Thread: th, TaskID: task.ID, Task: &store.TaskState{Status: store.StatusIdle}}))

	// On a subscription the closing line carries no cost; the usage
	// event that follows is the cost, and it addresses nobody.
	check("subscription turn", s.Render(agentEv(agent.Event{Type: agent.EventText, Text: "ok"})), "ok", "[status] ⏳ thinking · 0s")
	check("subscription result", s.Render(agentEv(agent.Event{Type: agent.EventResult, Text: "done", Cost: 0.31, Billing: agent.BillingSubscription})),
		"[remove status]", "✅ done · 0s · m")
	usage := agentEv(agent.Event{Type: agent.EventUsage, Usage: &agent.Usage{Windows: []agent.UsageWindow{{Name: "5h", Used: 15}, {Name: "7d", Used: 28}}}})
	meter := s.Render(usage)
	check("usage after the turn", meter, "📊 plan usage\n▰▰▱▱▱▱▱▱▱▱ 15% · 5h\n▰▰▰▱▱▱▱▱▱▱ 28% · 7d")
	if meter[0].Mention != "" || s.turns[th] != nil {
		t.Fatalf("usage line: mention=%q, turn started=%v", meter[0].Mention, s.turns[th] != nil)
	}
	// A quick follow-up: the meter lands inside the next turn, above its status line.
	check("quick follow-up", s.Render(agentEv(agent.Event{Type: agent.EventToolUse, Tool: "Read", ToolID: "9", ToolInput: map[string]any{"file_path": "/b.go"}})),
		"[status] 🔧 Read `/b.go` · 0s · 1 tool call")
	check("usage inside the next turn", s.Render(usage),
		"[remove status]", "📊 plan usage\n▰▰▱▱▱▱▱▱▱▱ 15% · 5h\n▰▰▰▱▱▱▱▱▱▱ 28% · 7d", "[status] 🔧 Read `/b.go` · 0s · 1 tool call")
	check("follow-up result", s.Render(agentEv(agent.Event{Type: agent.EventResult, Text: "done", Billing: agent.BillingSubscription})),
		"[remove status]", "✅ done · 0s · 1 tool call · m")

	// A follow-up to the live process has no started event: the first
	// agent event opens the next turn.
	now = now.Add(time.Minute)
	check("follow-up tool", s.Render(agentEv(agent.Event{Type: agent.EventToolUse, Tool: "Read", ToolID: "3", ToolInput: map[string]any{"file_path": "/a.go"}})),
		"[status] 🔧 Read `/a.go` · 0s · 1 tool call")
	now = now.Add(3 * time.Second)
	check("follow-up error", s.Render(agentEv(agent.Event{Type: agent.EventError, Text: "boom"})), "[remove status]", "❌ boom")

	// A task that dies outside the agent: the coordinator's error line
	// explains it, the finished event only takes the status line down.
	check("next turn", s.Render(surface.Event{Kind: surface.EventStarted, Thread: th, TaskID: task.ID, Task: task}),
		"▶️ task `t1` started with agent *coder* (local)", "[status] ⏳ starting · 0s")
	check("error", s.Render(surface.Event{Kind: surface.EventError, Thread: th, TaskID: task.ID, Task: task, Text: "start environment: no docker"}),
		"[remove status]", "❌ start environment: no docker", "[status] ⏳ starting · 0s")
	check("failed after error", s.Render(surface.Event{Kind: surface.EventFinished, Thread: th, TaskID: task.ID, Task: &store.TaskState{Status: store.StatusFailed}}),
		"[remove status]")

	// Cancelled mid-turn: the line goes, the outcome stays.
	check("third turn", s.Render(surface.Event{Kind: surface.EventResumed, Thread: th, TaskID: task.ID, Task: task}),
		"⏯️ resuming session with agent *coder*", "[status] ⏳ starting · 0s")
	check("cancelled", s.Render(surface.Event{Kind: surface.EventFinished, Thread: th, TaskID: task.ID, Task: &store.TaskState{Status: store.StatusCancelled}}),
		"[remove status]", "⏹️ cancelled")
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		400 * time.Millisecond: "0s",
		12 * time.Second:       "12s",
		65 * time.Second:       "1m05s",
		62 * time.Minute:       "1h02m",
	}
	for d, want := range cases {
		if got := formatDuration(d); got != want {
			t.Errorf("%s: got %q want %q", d, got, want)
		}
	}
}
