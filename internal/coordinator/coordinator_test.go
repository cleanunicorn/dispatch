package coordinator

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
	"github.com/cleanunicorn/dispatch/internal/executor"
	execlocal "github.com/cleanunicorn/dispatch/internal/executor/local"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/surface/chat"
	"github.com/cleanunicorn/dispatch/internal/surface/feed"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// fakeTransport records outbound messages and lets the test inject inbound ones.
type fakeTransport struct {
	name  string
	inbox chan<- transport.Inbound
	ready chan struct{}

	mu         sync.Mutex
	out        []transport.Outbound
	remembered []transport.ThreadID
	forgotten  []transport.ThreadID
	followed   []transport.ThreadID
	reacted    map[transport.ThreadID][]string
	removed    map[transport.ThreadID][]string // every Unreact, whether the reaction was there
}

func (f *fakeTransport) unreacted(th transport.ThreadID, emoji string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.removed[th], emoji)
}

func (f *fakeTransport) Name() string { return f.name }
func (f *fakeTransport) Remember(th transport.ThreadID) {
	f.mu.Lock()
	f.remembered = append(f.remembered, th)
	f.mu.Unlock()
}

// Forget implements transport.ThreadCloser.
func (f *fakeTransport) Forget(th transport.ThreadID) {
	f.mu.Lock()
	f.forgotten = append(f.forgotten, th)
	f.mu.Unlock()
}

// Follow implements transport.ThreadCloser.
func (f *fakeTransport) Follow(th transport.ThreadID) {
	f.mu.Lock()
	f.followed = append(f.followed, th)
	f.mu.Unlock()
}

// React implements transport.Reactor.
func (f *fakeTransport) React(ctx context.Context, th transport.ThreadID, emoji string) error {
	f.mu.Lock()
	if f.reacted == nil {
		f.reacted = map[transport.ThreadID][]string{}
	}
	f.reacted[th] = append(f.reacted[th], emoji)
	f.mu.Unlock()
	return nil
}

// Unreact implements transport.Reactor. Every removal is recorded, present
// or not: like Slack, a reaction from a previous process is not in reacted.
func (f *fakeTransport) Unreact(ctx context.Context, th transport.ThreadID, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removed == nil {
		f.removed = map[transport.ThreadID][]string{}
	}
	f.removed[th] = append(f.removed[th], emoji)
	cur := f.reacted[th]
	for i, e := range cur {
		if e == emoji {
			f.reacted[th] = append(cur[:i:i], cur[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeTransport) forgot(th transport.ThreadID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.forgotten {
		if t == th {
			return true
		}
	}
	return false
}

// forgetCount is how many times the coordinator told the transport to
// forget th; a mention in a closed thread must re-tombstone it.
func (f *fakeTransport) forgetCount(th transport.ThreadID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.forgotten {
		if t == th {
			n++
		}
	}
	return n
}

func (f *fakeTransport) wasRemembered(th transport.ThreadID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.remembered {
		if t == th {
			return true
		}
	}
	return false
}

// reactions is what is on the thread's root message right now.
func (f *fakeTransport) reactions(th transport.ThreadID) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reacted[th]...)
}

func (f *fakeTransport) Run(ctx context.Context, inbox chan<- transport.Inbound) error {
	f.inbox = inbox
	close(f.ready)
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeTransport) Send(ctx context.Context, msg transport.Outbound) error {
	f.mu.Lock()
	f.out = append(f.out, msg)
	f.mu.Unlock()
	return nil
}
func (f *fakeTransport) say(th transport.ThreadID, text string) { f.sayAs(th, "u1", text) }
func (f *fakeTransport) sayAs(th transport.ThreadID, user, text string) {
	f.inbox <- transport.Inbound{Transport: f.name, Thread: th, UserID: user, Text: text}
}
func (f *fakeTransport) decide(th transport.ThreadID, id, choice string) {
	f.decideAs(th, "u1", id, choice)
}
func (f *fakeTransport) decideAs(th transport.ThreadID, user, id, choice string) {
	f.inbox <- transport.Inbound{Transport: f.name, Thread: th, UserID: user, Decision: &transport.Decision{PromptID: id, Choice: choice}}
}
func (f *fakeTransport) waitFor(t *testing.T, th transport.ThreadID, sub string) transport.Outbound {
	t.Helper()
	return f.waitForN(t, th, sub, 1)
}

// waitForN returns the n-th message on th containing sub.
func (f *fakeTransport) waitForN(t *testing.T, th transport.ThreadID, sub string, n int) transport.Outbound {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		seen := 0
		for _, o := range f.out {
			if o.Thread == th && strings.Contains(o.Text, sub) {
				seen++
				if seen == n {
					f.mu.Unlock()
					return o
				}
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("fewer than %d outbounds on %s containing %q; got %+v", n, th, sub, f.out)
	return transport.Outbound{}
}

// fakeAgent: asks permission for Bash, reports the decision, then a result.
// fakeAgent's merge field says how it answers the turn `merge` asks for:
// "" merges the pull request and gets gh's confirmation back, "refused"
// runs the same command and is turned down by GitHub.
type fakeAgent struct{ merge string }

// fakePlan is what the fake planner answers. "nonsense" asks for a plan
// that parses and is then refused by workflow.Validate — an agent nobody
// defined — which is the case that must leave the thread with a message
// and nothing started.
func fakePlan(prompt string) string {
	if strings.Contains(prompt, "nonsense") {
		return `{"name":"x","steps":[{"name":"a","agent":"ghost","prompt":"do it"}]}`
	}
	if strings.Contains(prompt, "garbage") {
		return "I could not work out any steps for that."
	}
	return `{"name":"x","description":"implement then approve","steps":[
		{"name":"implement","agent":"coder","prompt":"{{.Ask}}","expect":"pr"},
		{"name":"approve","gate":"Merge {{.PR}}?"}
	]}`
}

func (fakeAgent) Kind() agent.Kind { return "fake" }
func (f fakeAgent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	r := newFakeRun()
	r.merge = f.merge
	if strings.HasPrefix(prompt, "ask") {
		go func() {
			r.emit(agent.Event{Type: agent.EventInit, Session: "sess-q"})
			r.emit(agent.Event{Type: agent.EventQuestion, Tool: "AskUserQuestion", ToolID: "q-1", Questions: []agent.Question{
				{Header: "Fruit", Text: "Apple or Banana?", Options: []agent.Option{{Label: "Apple"}, {Label: "Banana", Description: "long"}}},
				{Header: "Size", Text: "Big or small?", Options: []agent.Option{{Label: "Big"}, {Label: "Small"}}},
			}})
			select {
			case d := <-r.decided:
				r.emit(agent.Event{Type: agent.EventText, Text: fmt.Sprintf("answers=%s|%s", d.Answers["Apple or Banana?"], d.Answers["Big or small?"])})
				r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: "sess-q"})
			case <-r.done:
			}
		}()
		return r, nil
	}
	// The planning turn (plan.go). It is recognised by its own prompt,
	// which is what dispatch really sends, and it answers with JSON the
	// way a model does: prose around one object, which ParsePlan reads
	// past. What it plans depends on the ask, so one fake covers a plan
	// that holds up and one that does not.
	if strings.Contains(prompt, "composing a dispatch workflow") {
		go func() {
			r.emit(agent.Event{Type: agent.EventInit, Session: "sess-plan"})
			r.emit(agent.Event{Type: agent.EventText, Text: "Here you go:\n\n" + fakePlan(prompt)})
			r.emit(agent.Event{Type: agent.EventResult, Text: "planned", Session: "sess-plan"})
		}()
		return r, nil
	}
	// "botch": a turn that opened a pull request and then failed. The
	// half-finished work it left behind is what a human has to deal with.
	if strings.HasPrefix(prompt, "botch") {
		go func() {
			r.emit(agent.Event{Type: agent.EventInit, Session: "sess-b"})
			r.emit(agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "b-1", ToolInput: map[string]any{"command": "git switch -c half-done"}})
			r.emit(agent.Event{Type: agent.EventToolResult, ToolID: "b-1", Text: "Switched to a new branch 'half-done'"})
			r.emit(agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "b-2", ToolInput: map[string]any{"command": "gh pr create --title wip"}})
			r.emit(agent.Event{Type: agent.EventToolResult, ToolID: "b-2", Text: "https://github.com/cleanunicorn/dispatch/pull/60"})
			r.emit(agent.Event{Type: agent.EventError, Text: "the build fell over", Session: "sess-b"})
		}()
		return r, nil
	}
	// "slow": a turn that only ends when its process is stopped — the
	// shape of a turn a restart cuts short, with nothing racy about how
	// its records land.
	if strings.HasPrefix(prompt, "slow") {
		go func() {
			r.emit(agent.Event{Type: agent.EventInit, Session: "sess-slow"})
			<-r.done
		}()
		return r, nil
	}
	// "ship": a turn that does what the work overview is mined from —
	// a branch, a pull request, and the issue its body closes.
	if strings.HasPrefix(prompt, "ship") {
		go func() {
			r.emit(agent.Event{Type: agent.EventInit, Session: "sess-s"})
			r.emit(agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "s-1", ToolInput: map[string]any{"command": "git switch -c fix-47"}})
			r.emit(agent.Event{Type: agent.EventToolResult, ToolID: "s-1", Text: "Switched to a new branch 'fix-47'"})
			r.emit(agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "s-2", ToolInput: map[string]any{"command": `gh pr create --body "Closes #47"`}})
			r.emit(agent.Event{Type: agent.EventToolResult, ToolID: "s-2", Text: "https://github.com/cleanunicorn/dispatch/pull/51"})
			r.emit(agent.Event{Type: agent.EventResult, Text: "opened", Session: "sess-s"})
		}()
		return r, nil
	}
	go func() {
		r.emit(agent.Event{Type: agent.EventInit, Session: "sess-1"})
		r.emit(agent.Event{Type: agent.EventNeedsPermission, Tool: "Bash", ToolID: "tool-1", ToolInput: map[string]any{"command": "ls"}})
		select {
		case d := <-r.decided:
			r.emit(agent.Event{Type: agent.EventText, Text: fmt.Sprintf("allowed=%v", d.Allow)})
			r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: "sess-1", Cost: 0.01})
			r.emit(agent.Event{Type: agent.EventUsage, Usage: &agent.Usage{Plan: "max", Windows: []agent.UsageWindow{{Name: "5h", Used: 15}}}})
		case <-r.done:
		}
	}()
	return r, nil
}
func (f fakeAgent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	r := newFakeRun()
	r.merge = f.merge
	// The turn `merge` asks for: the agent runs the merge itself, and what
	// gh answered is the only thing dispatch reads (internal/work).
	if strings.Contains(prompt, "gh pr merge") {
		go func() {
			r.emit(agent.Event{Type: agent.EventInit, Session: session})
			r.emit(agent.Event{Type: agent.EventText, Text: "echo:" + prompt})
			r.mergeTurn(session, prompt)
		}()
		return r, nil
	}
	go func() {
		r.emit(agent.Event{Type: agent.EventInit, Session: session})
		r.emit(agent.Event{Type: agent.EventText, Text: "echo:" + prompt})
		r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: session})
	}()
	return r, nil
}

type fakeRun struct {
	events  chan agent.Event
	decided chan agent.PermissionDecision
	done    chan struct{}
	merge   string // fakeAgent.merge, for a message asking for `gh pr merge`

	mu     sync.Mutex
	closed bool
}

func newFakeRun() *fakeRun {
	return &fakeRun{events: make(chan agent.Event, 16), decided: make(chan agent.PermissionDecision, 1), done: make(chan struct{})}
}

// emit sends unless the run was stopped; sends and Stop are serialized by mu.
func (r *fakeRun) emit(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.events <- ev
}

func (r *fakeRun) Events() <-chan agent.Event { return r.events }
func (r *fakeRun) Send(ctx context.Context, text string) error {
	// Asynchronous like a real agent: the executor must see the send's
	// activity before the turn's result, or its idle timer never restarts.
	go func() {
		r.emit(agent.Event{Type: agent.EventText, Text: "echo:" + text})
		if strings.Contains(text, "gh pr merge") {
			r.mergeTurn("sess-1", text)
			return
		}
		r.emit(agent.Event{Type: agent.EventResult, Text: "ok", Session: "sess-1"})
	}()
	return nil
}

// mergeTurn is the turn `merge` asks for: the agent runs the merge itself,
// and what gh answered is the only thing dispatch reads (internal/work).
//
// It runs the command out of the prompt rather than one of its own, so a
// thread that only ever knew a number logs `gh pr merge 51` and hands the
// scan no repository — which is the thread gh's own qualified answer has
// to be recognised on.
func (r *fakeRun) mergeTurn(session, prompt string) {
	answer := "✓ Squashed and merged pull request cleanunicorn/dispatch#51 (fix)"
	say := "merged it"
	if r.merge == "refused" {
		answer = "X Pull request #51 is not mergeable: 1 required check is still pending"
		say = "GitHub would not merge it: a check is still pending"
	}
	cmd := "gh pr merge 51 --squash --delete-branch"
	if m := mergeCmdInPrompt.FindString(prompt); m != "" {
		cmd = m
	}
	r.emit(agent.Event{Type: agent.EventToolUse, Tool: "Bash", ToolID: "m-1",
		ToolInput: map[string]any{"command": cmd}})
	r.emit(agent.Event{Type: agent.EventToolResult, ToolID: "m-1", Text: answer})
	r.emit(agent.Event{Type: agent.EventResult, Text: say, Session: session})
}

// mergeCmdInPrompt picks dispatch's own `gh pr merge …` line out of the
// prompt it wrote (finish.go's mergePrompt puts it in backticks).
var mergeCmdInPrompt = regexp.MustCompile("gh pr merge [^`\n]*")

func (r *fakeRun) Decide(ctx context.Context, d agent.PermissionDecision) error {
	r.decided <- d
	return nil
}
func (r *fakeRun) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.done)
		close(r.events)
	}
	return nil
}

func TestTwoSurfacesOneTransport(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := agent.Definition{Name: "coder", Kind: "fake"}
	if err := st.PutDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	feedThread := transport.ThreadID("C-ops/")
	surfaces := []surface.Surface{
		feed.New("ops", "slack", feedThread, true),
		chat.New("chat", "slack", false),
	}
	c := New(st, ex, []transport.Transport{tr}, surfaces, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/1.0")
	tr.say(th, "run coder do the thing")
	tr.waitFor(t, th, "started with agent *coder*")
	tr.waitFor(t, feedThread, "started — agent *coder*")

	// Both surfaces render the permission prompt with their own prefix.
	chatPrompt := tr.waitFor(t, th, "wants to run")
	feedPrompt := tr.waitFor(t, feedThread, "wants to run")
	if chatPrompt.Prompt == nil || !strings.HasPrefix(chatPrompt.Prompt.ID, "chat:") {
		t.Fatalf("chat prompt = %+v", chatPrompt.Prompt)
	}
	if feedPrompt.Prompt == nil || !strings.HasPrefix(feedPrompt.Prompt.ID, "ops:") {
		t.Fatalf("feed prompt = %+v", feedPrompt.Prompt)
	}

	// Decide from the feed surface; the chat thread still gets the result.
	tr.decide(feedThread, feedPrompt.Prompt.ID, "allow")
	tr.waitFor(t, th, "allowed=true")
	tr.waitFor(t, th, "✅ done")
	tr.waitFor(t, feedThread, "done")

	// Follow-up while still live.
	tr.say(th, "more please")
	tr.waitFor(t, th, "echo:more please")

	// Status reply goes only to the chat thread.
	tr.say(th, "status")
	tr.waitFor(t, th, "status *")

	// Plain chatter in the feed thread is swallowed by the feed surface.
	tr.say(feedThread, "hello ops")
	time.Sleep(50 * time.Millisecond)
	tr.mu.Lock()
	for _, o := range tr.out {
		if o.Thread == feedThread && strings.Contains(o.Text, "no task") {
			t.Fatalf("feed chatter leaked to chat surface: %+v", o)
		}
	}
	tr.mu.Unlock()

	// After the idle timeout the process is gone; the task is idle with a
	// session and the next message resumes it.
	id := firstTask(t, st)
	deadline := time.Now().Add(3 * time.Second)
	for ex.IsRunning(id) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ex.IsRunning(id) {
		t.Fatal("task still running after idle timeout")
	}
	ts, _ := st.GetTask(ctx, id)
	if ts.Status != store.StatusIdle || ts.Session != "sess-1" {
		t.Fatalf("task after idle = %+v", ts)
	}
	tr.say(th, "again")
	tr.waitFor(t, th, "resuming session")
	tr.waitFor(t, th, "echo:again")

	// Plain text on a fresh thread starts a task with the default agent.
	th2 := transport.ThreadID("C-dev/2.0")
	tr.say(th2, "just do the thing")
	tr.waitFor(t, th2, "started with agent *coder*")

	// Questions: first answered with a button, second with a typed reply.
	th3 := transport.ThreadID("C-dev/3.0")
	tr.say(th3, "ask me things")
	q1 := tr.waitFor(t, th3, "Apple or Banana?")
	if q1.Prompt == nil || len(q1.Prompt.Options) != 2 || !q1.Prompt.FreeText {
		t.Fatalf("question prompt = %+v", q1.Prompt)
	}
	tr.decide(th3, q1.Prompt.ID, "Banana")
	tr.waitFor(t, th3, "Big or small?")
	tr.say(th3, "medium, actually")
	tr.waitFor(t, th3, "answers=Banana|medium, actually")
}

// mentionHarness is a coordinator over a fake agent whose finished turns
// stay warm for idle.
func mentionHarness(t *testing.T, idle time.Duration) (context.Context, *Coordinator, store.Store, *fakeTransport) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, idle)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	go c.Run(ctx)
	<-tr.ready
	return ctx, c, st, tr
}

// waitUnbound waits for th's task to let go of the thread — its process
// ended after the idle timeout — so the next message takes the cold path.
func waitUnbound(t *testing.T, c *Coordinator, th transport.ThreadID) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, busy := c.lookup(th); !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("thread still bound to its task after the idle timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestMentionFollowsAsker: the lines that need a human address the one
// whose message the turn answers — the requester on the first turn, even
// when someone else answers its prompt, and whoever followed up after
// that — while the task's requester stays the one who started it.
func TestMentionFollowsAsker(t *testing.T) {
	ctx, c, st, tr := mentionHarness(t, 200*time.Millisecond)

	th := transport.ThreadID("C-dev/1.0")
	tr.sayAs(th, "u1", "run coder do the thing")
	p := tr.waitFor(t, th, "wants to run")
	if p.Mention != "u1" {
		t.Errorf("prompt addressed to %q, want u1", p.Mention)
	}
	tr.decideAs(th, "u2", p.Prompt.ID, "allow")
	if o := tr.waitFor(t, th, "✅ done"); o.Mention != "u1" {
		t.Errorf("done line addressed to %q after u2 allowed, want u1", o.Mention)
	}

	// u2 follows up after the idle timeout: the resumed turn reports to u2.
	id := firstTask(t, st)
	waitUnbound(t, c, th)
	tr.sayAs(th, "u2", "again")
	tr.waitFor(t, th, "echo:again")
	if o := tr.waitForN(t, th, "✅ done", 2); o.Mention != "u2" {
		t.Errorf("resumed done line addressed to %q after u2 followed up, want u2", o.Mention)
	}
	if ts, err := st.GetTask(ctx, id); err != nil || ts.Requester != "u1" || ts.Asker != "u2" {
		t.Errorf("requester, asker = %q, %q err=%v, want u1, u2", ts.Requester, ts.Asker, err)
	}

	// A new task on the same thread, started by u2, is u2's. `run` is
	// refused while the thread is bound to the first task, so wait for
	// exactly that to clear.
	waitUnbound(t, c, th)
	tr.sayAs(th, "u2", "run coder other thing")
	if o := tr.waitForN(t, th, "wants to run", 2); o.Mention != "u2" {
		t.Errorf("u2's own task addressed to %q, want u2", o.Mention)
	}
	if ts, err := st.LatestTaskForThread(ctx, th); err != nil || ts.ID == id || ts.Requester != "u2" {
		t.Errorf("u2's task = %+v err=%v, want a new task with requester u2", ts, err)
	}
}

// TestMentionFollowsAskerWarm: a follow-up that reaches the agent process
// still kept alive after its turn addresses its own author too, and the
// turn after that goes back to whoever wrote it.
func TestMentionFollowsAskerWarm(t *testing.T) {
	ctx, c, st, tr := mentionHarness(t, time.Minute)

	th := transport.ThreadID("C-dev/2.0")
	tr.sayAs(th, "u1", "run coder do the thing")
	p := tr.waitFor(t, th, "wants to run")
	tr.decideAs(th, "u1", p.Prompt.ID, "allow")
	if o := tr.waitFor(t, th, "✅ done"); o.Mention != "u1" {
		t.Errorf("first done line addressed to %q, want u1", o.Mention)
	}
	id := firstTask(t, st)
	if bound, ok := c.lookup(th); !ok || bound != id {
		t.Fatalf("thread not bound to the warm task: %q %v", bound, ok)
	}
	tr.sayAs(th, "u2", "again")
	tr.waitFor(t, th, "echo:again")
	if o := tr.waitForN(t, th, "✅ done", 2); o.Mention != "u2" {
		t.Errorf("warm follow-up done line addressed to %q, want u2", o.Mention)
	}
	tr.sayAs(th, "u1", "once more")
	tr.waitFor(t, th, "echo:once more")
	if o := tr.waitForN(t, th, "✅ done", 3); o.Mention != "u1" {
		t.Errorf("u1's next done line addressed to %q, want u1", o.Mention)
	}
	if ts, err := st.GetTask(ctx, id); err != nil || ts.Requester != "u1" || ts.Asker != "u1" {
		t.Errorf("requester, asker = %q, %q err=%v, want u1, u1", ts.Requester, ts.Asker, err)
	}
}

// TestMentionQueuesAMidTurnAsker: a message written while a turn is still
// going is answered after it, so the turn in progress keeps addressing its
// own asker and the one after it addresses the writer.
func TestMentionQueuesAMidTurnAsker(t *testing.T) {
	_, _, _, tr := mentionHarness(t, time.Minute)

	th := transport.ThreadID("C-dev/3.0")
	tr.sayAs(th, "u1", "run coder do the thing")
	p := tr.waitFor(t, th, "wants to run") // u1's turn is open, waiting on the prompt
	// The fake answers a send with a turn of its own at once, which ends
	// first here; what matters is the order the two closing lines take.
	tr.sayAs(th, "u2", "and also this")
	tr.waitFor(t, th, "echo:and also this")
	if o := tr.waitFor(t, th, "✅ done"); o.Mention != "u1" {
		t.Errorf("the turn in progress closed addressing %q, want u1", o.Mention)
	}
	tr.decideAs(th, "u1", p.Prompt.ID, "allow")
	if o := tr.waitForN(t, th, "✅ done", 2); o.Mention != "u2" {
		t.Errorf("the turn after it closed addressing %q, want u2", o.Mention)
	}
}

func TestGracefulRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "c.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := st.PutDefinition(ctx, agent.Definition{Name: "coder", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, time.Minute)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	c.DrainTimeout = time.Second
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()
	<-tr.ready

	th := transport.ThreadID("C-dev/9.0")
	tr.say(th, "run coder do it")
	tr.waitFor(t, th, "wants to run") // task is live, waiting on a permission

	cancel() // SIGTERM
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not stop")
	}
	tr.waitFor(t, th, "dispatch is restarting")
	id := firstTask(t, st)
	ts, _ := st.GetTask(context.Background(), id)
	if ts.Status != store.StatusInterrupted || ts.Session != "sess-1" || ts.Transport != "slack" {
		t.Fatalf("task after shutdown = %+v", ts)
	}
	st.Close()

	// Restart: recovered task gets a "back" notice and the next reply resumes it.
	st2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	tr2 := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c2 := New(st2, ex, []transport.Transport{tr2}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c2.DefaultDefinition = "coder"
	go c2.Run(ctx2)
	<-tr2.ready
	if o := tr2.waitFor(t, th, "dispatch is back"); o.Mention != "u1" {
		t.Errorf("restart notice addressed to %q, want the requester u1", o.Mention)
	}
	tr2.mu.Lock()
	seeded := len(tr2.remembered) == 1 && tr2.remembered[0] == th
	tr2.mu.Unlock()
	if !seeded {
		t.Fatalf("transport not re-seeded with task thread: %v", tr2.remembered)
	}
	tr2.say(th, "carry on")
	tr2.waitFor(t, th, "resuming session")
	tr2.waitFor(t, th, "echo:carry on")
}

func firstTask(t *testing.T, st store.Store) executor.TaskID {
	tasks, err := st.ListTasks(context.Background(), "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("no tasks: %v", err)
	}
	return tasks[0].ID
}

func TestChannelDefaultsAndRunPicker(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, name := range []string{"coder", "reviewer"} {
		if err := st.PutDefinition(ctx, agent.Definition{Name: name, Kind: "fake"}); err != nil {
			t.Fatal(err)
		}
	}

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.WorkdirRoot = t.TempDir()
	c.DefaultDefinition = "coder"
	c.ChannelAgents = map[string]string{"slack/C-review": "reviewer"}
	var saved []string
	var savedMu sync.Mutex
	c.SaveChannelAgent = func(_ context.Context, tr, ch, a string) error {
		savedMu.Lock()
		saved = append(saved, tr+"/"+ch+"="+a)
		savedMu.Unlock()
		return nil
	}
	go c.Run(ctx)
	<-tr.ready

	// Plain text follows the channel default, else the global one.
	tr.say("C-review/1.0", "look at this")
	tr.waitFor(t, "C-review/1.0", "started with agent *reviewer*")
	// ...and records who asked, like `run` does.
	if o := tr.waitFor(t, "C-review/1.0", "wants to run"); o.Mention != "u1" {
		t.Errorf("default-agent prompt addressed to %q, want u1", o.Mention)
	}
	if ts, err := st.LatestTaskForThread(ctx, "C-review/1.0"); err != nil || ts.Requester != "u1" {
		t.Errorf("default-agent requester = %q err=%v", ts.Requester, err)
	}
	tr.say("C-dev/1.0", "build this")
	tr.waitFor(t, "C-dev/1.0", "started with agent *coder*")

	// An unknown agent name falls back to the channel default too.
	tr.say("C-review/2.0", "run nosuch thing")
	tr.waitFor(t, "C-review/2.0", "started with agent *reviewer*")

	// `default` shows, `default <agent>` sets and persists.
	tr.say("C-dev/2.0", "default")
	tr.waitFor(t, "C-dev/2.0", "global default *coder*")
	tr.say("C-dev/2.0", "default nosuch")
	tr.waitFor(t, "C-dev/2.0", "unknown agent")
	tr.say("C-dev/2.0", "default reviewer")
	tr.waitFor(t, "C-dev/2.0", "now *reviewer*")
	savedMu.Lock()
	if len(saved) != 1 || saved[0] != "slack/C-dev=reviewer" {
		t.Fatalf("saved = %v", saved)
	}
	savedMu.Unlock()
	tr.say("C-dev/3.0", "next thing")
	tr.waitFor(t, "C-dev/3.0", "started with agent *reviewer*")
	tr.say("C-dev/4.0", "agents")
	if o := tr.waitFor(t, "C-dev/4.0", "*reviewer*"); !strings.Contains(o.Text, "*reviewer* — fake/, env , mode  · _default here_") || strings.Count(o.Text, "default here") != 1 {
		t.Fatalf("agents = %q", o.Text)
	}

	// Bare `run`: pick the agent from a list, then type the prompt.
	th := transport.ThreadID("C-dev/5.0")
	tr.say(th, "run")
	q := tr.waitFor(t, th, "Which agent?")
	if q.Prompt == nil || len(q.Prompt.Options) != 2 || !q.Prompt.FreeText || !strings.Contains(q.Prompt.Options[1].Description, "default here") {
		t.Fatalf("picker prompt = %+v", q.Prompt)
	}
	// Someone else answers the picker: the task is still u1's.
	tr.decideAs(th, "u2", q.Prompt.ID, "coder")
	p := tr.waitFor(t, th, "What should *coder* do?")
	if p.Prompt == nil || len(p.Prompt.Options) != 0 {
		t.Fatalf("prompt question = %+v", p.Prompt)
	}
	tr.sayAs(th, "u2", "do the thing")
	tr.waitFor(t, th, "started with agent *coder*")
	if o := tr.waitFor(t, th, "wants to run"); o.Mention != "u1" {
		t.Errorf("permission prompt addressed to %q, want the one who typed `run`", o.Mention)
	}
	if ts, err := st.LatestTaskForThread(ctx, th); err != nil || ts.Requester != "u1" {
		t.Errorf("requester = %q err=%v", ts.Requester, err)
	}

	// `run <agent>` without a prompt asks for the prompt; a typed agent
	// name works for the picker too; `cancel` abandons it.
	th2 := transport.ThreadID("C-dev/6.0")
	tr.say(th2, "run reviewer")
	tr.waitFor(t, th2, "What should *reviewer* do?")
	tr.say(th2, "cancel")
	tr.waitFor(t, th2, "run cancelled")
	th3 := transport.ThreadID("C-dev/7.0")
	tr.say(th3, "run")
	tr.waitFor(t, th3, "Which agent?")
	tr.say(th3, "nosuch")
	tr.waitFor(t, th3, "no agent named")
	tr.say(th3, "reviewer")
	tr.waitFor(t, th3, "What should *reviewer* do?")
	tr.say(th3, "review it")
	tr.waitFor(t, th3, "started with agent *reviewer*")
}
