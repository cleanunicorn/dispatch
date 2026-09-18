package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
)

type fakeProc struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	exited  chan struct{}
	got     chan string
	code    int
}

func newFakeProc() *fakeProc {
	f := &fakeProc{exited: make(chan struct{}), got: make(chan string, 32)}
	f.stdinR, f.stdinW = io.Pipe()
	f.stdoutR, f.stdoutW = io.Pipe()
	go func() {
		s := bufio.NewScanner(f.stdinR)
		for s.Scan() {
			f.got <- s.Text()
		}
	}()
	return f
}
func (f *fakeProc) Stdin() io.WriteCloser { return f.stdinW }
func (f *fakeProc) Stdout() io.Reader     { return f.stdoutR }
func (f *fakeProc) Stderr() io.Reader     { return strings.NewReader("") }
func (f *fakeProc) Wait() (int, error)    { <-f.exited; return f.code, nil }
func (f *fakeProc) Kill() error           { f.exit(); return nil }
func (f *fakeProc) exit() {
	select {
	case <-f.exited:
	default:
		close(f.exited)
		_ = f.stdoutW.Close()
	}
}
func (f *fakeProc) say(s string) { _, _ = io.WriteString(f.stdoutW, s+"\n") }
func (f *fakeProc) wrote(t *testing.T, sub string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case s := <-f.got:
			if strings.Contains(s, sub) {
				return s
			}
		case <-deadline:
			t.Fatalf("did not write %q", sub)
		}
	}
}

type fakeEnv struct{ p *fakeProc }

func (e fakeEnv) Kind() environment.Kind      { return environment.KindLocal }
func (e fakeEnv) Start(context.Context) error { return nil }
func (e fakeEnv) Exec(context.Context, string, ...string) (environment.Process, error) {
	return e.p, nil
}
func (e fakeEnv) CopyIn(context.Context, io.Reader, string) error        { return nil }
func (e fakeEnv) CopyOut(context.Context, string) (io.ReadCloser, error) { return nil, io.EOF }
func (e fakeEnv) Stop(context.Context) error                             { return nil }

func next(t *testing.T, r agent.Run) agent.Event {
	t.Helper()
	select {
	case e := <-r.Events():
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return agent.Event{}
	}
}

func TestAppServerRoundTrip(t *testing.T) {
	f := newFakeProc()
	r, err := (&Agent{Binary: "codex"}).Start(context.Background(), fakeEnv{f}, agent.Definition{Kind: agent.KindCodex, Model: "gpt-test", PermissionMode: agent.PermissionManual}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	f.wrote(t, `"method":"initialize"`)
	f.say(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	f.wrote(t, `"method":"initialized"`)
	f.wrote(t, `"method":"account/read"`)
	f.say(`{"jsonrpc":"2.0","id":2,"result":{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}}`)
	start := f.wrote(t, `"method":"thread/start"`)
	if !strings.Contains(start, `"approvalPolicy":"untrusted"`) || !strings.Contains(start, `"sandbox":"read-only"`) {
		t.Fatalf("thread start = %s", start)
	}
	f.say(`{"jsonrpc":"2.0","id":3,"result":{"thread":{"id":"thr-1","model":"gpt-test"}}}`)
	if e := next(t, r); e.Type != agent.EventInit || e.Session != "thr-1" || e.Model != "gpt-test" || e.Billing != agent.BillingAPIKey {
		t.Fatalf("init = %+v", e)
	}
	f.wrote(t, `"method":"turn/start"`)
	f.say(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-1","turn":{"id":"turn-1"}}}`)
	// A delta is not an event: item/completed carries the same text whole,
	// and a surface that posted both would post a message per word.
	f.say(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"thr-1","itemId":"msg-1","delta":"hello"}}`)
	f.say(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thr-1","item":{"id":"cmd-1","type":"commandExecution","command":"/usr/bin/zsh -lc 'git status'"}}}`)
	if e := next(t, r); e.Type != agent.EventToolUse || e.Tool != agent.ToolBash || e.ToolInput["command"] != "git status" {
		t.Fatalf("tool use = %+v", e)
	}
	f.say(`{"jsonrpc":"2.0","id":9,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr-1","itemId":"cmd-1","command":"git status","reason":"inspect"}}`)
	if e := next(t, r); e.Type != agent.EventNeedsPermission || e.ToolID != "cmd-1" || e.Tool != agent.ToolBash {
		t.Fatalf("permission = %+v", e)
	}
	if err := r.Decide(context.Background(), agent.PermissionDecision{ToolID: "cmd-1", Allow: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.wrote(t, `"id":9`); !strings.Contains(got, `"decision":"accept"`) {
		t.Fatalf("approval = %s", got)
	}
	f.say(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-1","item":{"id":"cmd-1","type":"commandExecution","aggregatedOutput":"clean"}}}`)
	if e := next(t, r); e.Type != agent.EventToolResult || e.ToolID != "cmd-1" || e.Text != "clean" {
		t.Fatalf("tool result = %+v", e)
	}
	f.say(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-1","turn":{"id":"turn-1","status":"completed"}}}`)
	if e := next(t, r); e.Type != agent.EventResult || e.Billing != agent.BillingAPIKey {
		t.Fatalf("result = %+v", e)
	}
	// A metered session has no plan to report on.
	select {
	case e := <-r.Events():
		t.Fatalf("API-key session sent %+v after its result", e)
	case <-time.After(200 * time.Millisecond):
	}
	f.exit()
}

func TestModes(t *testing.T) {
	for _, tt := range []struct {
		m                 agent.PermissionMode
		approval, sandbox string
	}{
		{agent.PermissionManual, "untrusted", "read-only"}, {agent.PermissionAcceptEdits, "on-request", "workspace-write"}, {agent.PermissionAuto, "on-request", "workspace-write"}, {agent.PermissionBypass, "never", "danger-full-access"},
	} {
		if a, s := mode(tt.m); a != tt.approval || s != tt.sandbox {
			t.Errorf("mode(%q) = %q, %q", tt.m, a, s)
		}
	}
}

// TestLiveAppServer is a cheap protocol smoke test against the installed
// Codex CLI. It is opt-in because it uses the caller's Codex account.
func TestLiveAppServer(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") != "1" {
		t.Skip("set DISPATCH_LIVE=1 to run against the real Codex CLI")
	}
	env, err := envlocal.Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := New().Start(context.Background(), env, agent.Definition{Kind: agent.KindCodex, PermissionMode: agent.PermissionManual}, "Reply with exactly CODEX_DRIVER_OK. Do not run tools.")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	deadline := time.After(90 * time.Second)
	var text string
	for {
		select {
		case ev, ok := <-r.Events():
			if !ok {
				t.Fatalf("Codex ended without a result; text=%q", text)
			}
			if ev.Type == agent.EventText {
				text = ev.Text
			}
			if ev.Type == agent.EventError {
				t.Fatalf("Codex error: %s", ev.Text)
			}
			if ev.Type == agent.EventResult {
				if text != "CODEX_DRIVER_OK" {
					t.Fatalf("reply = %q", text)
				}
				if ev.Billing != agent.BillingSubscription {
					t.Logf("billing = %q: no plan usage to check", ev.Billing)
					return
				}
				// A ChatGPT login reports the plan's windows after the turn.
				select {
				case u := <-r.Events():
					if u.Type != agent.EventUsage || u.Usage == nil || len(u.Usage.Windows) == 0 {
						t.Fatalf("after a subscription result = %+v", u)
					}
					t.Logf("usage: plan=%q windows=%+v", u.Usage.Plan, u.Usage.Windows)
				case <-time.After(30 * time.Second):
					t.Fatal("no usage after a subscription result")
				}
				return
			}
		case <-deadline:
			t.Fatal("Codex app-server did not complete")
		}
	}
}

func TestShellCommand(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{`/usr/bin/zsh -lc 'git status'`, "git status"},
		{`bash -c 'gh pr create --title x'`, "gh pr create --title x"},
		{`/bin/sh -lic 'echo '\''hi'\'''`, "echo 'hi'"},
		{`git status`, "git status"},                     // not wrapped
		{`node -c 'x'`, `node -c 'x'`},                   // not a shell
		{`zsh -lc 'a' && 'b'`, `zsh -lc 'a' && 'b'`},     // not one quoted word
		{`zsh -l 'a'`, `zsh -l 'a'`},                     // no -c
		{`zsh -lc "git status"`, `zsh -lc "git status"`}, // only single quotes are unwrapped
	} {
		if got := shellCommand(tt.in); got != tt.want {
			t.Errorf("shellCommand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A resumed thread inherits nothing from the thread it resumes: Codex falls
// back to the host's own config, so a manual definition would come back from
// an idle timeout with a full-access sandbox unless resume says so again.
func TestResumeCarriesThreadOptions(t *testing.T) {
	f := newFakeProc()
	def := agent.Definition{Kind: agent.KindCodex, Model: "gpt-test", PermissionMode: agent.PermissionManual, SystemPrompt: "be brief"}
	if _, err := (&Agent{Binary: "codex"}).Resume(context.Background(), fakeEnv{f}, def, "thr-old", "again"); err != nil {
		t.Fatal(err)
	}
	f.wrote(t, `"method":"initialize"`)
	f.say(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	f.wrote(t, `"method":"account/read"`)
	f.say(`{"jsonrpc":"2.0","id":2,"result":{"account":null,"requiresOpenaiAuth":true}}`)
	got := f.wrote(t, `"method":"thread/resume"`)
	for _, want := range []string{`"threadId":"thr-old"`, `"approvalPolicy":"untrusted"`, `"sandbox":"read-only"`, `"model":"gpt-test"`, "be brief"} {
		if !strings.Contains(got, want) {
			t.Errorf("thread/resume missing %s: %s", want, got)
		}
	}
	f.exit()
}

// A steer races the turn it steers: when the turn ends first Codex rejects
// expectedTurnId, and the message has to arrive as a turn of its own rather
// than taking the warm session down with it.
func TestSteerLosesRaceAndBecomesTurn(t *testing.T) {
	f := newFakeProc()
	r := ready(t, f)
	if err := r.Send(context.Background(), "and also this"); err != nil {
		t.Fatal(err)
	}
	steer := f.wrote(t, `"method":"turn/steer"`)
	f.say(`{"jsonrpc":"2.0","id":` + itoa(requestID(t, steer)) + `,"error":{"code":-32602,"message":"expectedTurnId does not match"}}`)
	got := f.wrote(t, `"method":"turn/start"`)
	if !strings.Contains(got, "and also this") {
		t.Fatalf("re-sent turn = %s", got)
	}
	select {
	case e := <-r.Events():
		t.Fatalf("a lost steer must not end the session: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
	f.exit()
}

// Codex waits for an answer to every request it sends. A method dispatch does
// not implement must still be answered or the turn hangs forever.
func TestUnknownServerRequestIsRefused(t *testing.T) {
	f := newFakeProc()
	r := ready(t, f)
	f.say(`{"jsonrpc":"2.0","id":77,"method":"item/permissions/requestApproval","params":{"threadId":"thr-1","itemId":"p-1"}}`)
	got := f.wrote(t, `"id":77`)
	if !strings.Contains(got, `"error"`) || !strings.Contains(got, "item/permissions/requestApproval") {
		t.Fatalf("refusal = %s", got)
	}
	// The session is untouched by a request it could not answer.
	f.say(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-1","item":{"id":"m-1","type":"agentMessage","text":"carrying on"}}}`)
	if e := next(t, r); e.Type != agent.EventText || e.Text != "carrying on" {
		t.Fatalf("event = %+v", e)
	}
	f.exit()
}

// A fileChange approval carries ids only; the changes came with the item.
func TestFileChangeApprovalKeepsTheChanges(t *testing.T) {
	f := newFakeProc()
	r := ready(t, f)
	f.say(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thr-1","item":{"id":"fc-1","type":"fileChange","changes":[{"path":"/w/a.go","kind":"update","diff":"@@"}]}}}`)
	if e := next(t, r); e.Type != agent.EventToolUse || e.Tool != agent.ToolEdit || e.ToolInput["file_path"] != "/w/a.go" {
		t.Fatalf("tool use = %+v", e)
	}
	f.say(`{"jsonrpc":"2.0","id":12,"method":"item/fileChange/requestApproval","params":{"threadId":"thr-1","itemId":"fc-1","turnId":"turn-1"}}`)
	e := next(t, r)
	if e.Type != agent.EventNeedsPermission || e.Tool != agent.ToolEdit || e.ToolInput["file_path"] != "/w/a.go" {
		t.Fatalf("permission = %+v", e)
	}
	f.exit()
}

// A sub-agent runs as a thread of its own on the same connection; its
// turn/completed is not this run's turn ending.
func TestOtherThreadNotificationsIgnored(t *testing.T) {
	f := newFakeProc()
	r := ready(t, f)
	f.say(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-other","turn":{"id":"t-x","status":"completed"}}}`)
	f.say(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-1","item":{"id":"m-1","type":"agentMessage","text":"mine"}}}`)
	if e := next(t, r); e.Type != agent.EventText || e.Text != "mine" {
		t.Fatalf("event = %+v", e)
	}
	f.exit()
}

// ready drives the handshake up to a started turn and returns the run.
func ready(t *testing.T, f *fakeProc) agent.Run {
	t.Helper()
	return readyAs(t, f, `{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`)
}

// readyAs is ready with the answer account/read gets.
func readyAs(t *testing.T, f *fakeProc, account string) agent.Run {
	t.Helper()
	r, err := (&Agent{Binary: "codex"}).Start(context.Background(), fakeEnv{f}, agent.Definition{Kind: agent.KindCodex, PermissionMode: agent.PermissionManual}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	f.wrote(t, `"method":"initialize"`)
	f.say(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	f.wrote(t, `"method":"account/read"`)
	f.say(`{"jsonrpc":"2.0","id":2,"result":` + account + `}`)
	f.wrote(t, `"method":"thread/start"`)
	f.say(`{"jsonrpc":"2.0","id":3,"result":{"thread":{"id":"thr-1"}}}`)
	if e := next(t, r); e.Type != agent.EventInit {
		t.Fatalf("init = %+v", e)
	}
	f.wrote(t, `"method":"turn/start"`)
	f.say(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-1","turn":{"id":"turn-1"}}}`)
	// turn/started emits nothing, so wait on a message behind it: the reader
	// is one goroutine, and an event from a later line proves it got there.
	f.say(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr-1","item":{"id":"m-0","type":"agentMessage","text":"working"}}}`)
	if e := next(t, r); e.Type != agent.EventText || e.Text != "working" {
		t.Fatalf("turn did not start: %+v", e)
	}
	return r
}

// A ChatGPT login is a plan, not a bill: the result carries no dollar figure
// to show, and the plan's windows follow it as an EventUsage.
func TestSubscriptionReportsPlanUsage(t *testing.T) {
	f := newFakeProc()
	r := readyAs(t, f, `{"account":{"type":"chatgpt","email":"x@y","planType":"prolite"},"requiresOpenaiAuth":true}`)
	f.say(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-1","turn":{"id":"turn-1","status":"completed"}}}`)
	if e := next(t, r); e.Type != agent.EventResult || e.Billing != agent.BillingSubscription {
		t.Fatalf("result = %+v", e)
	}
	req := f.wrote(t, `"method":"account/rateLimits/read"`)
	// Captured from Codex CLI 0.154.0 on a prolite plan, plus a model bucket.
	f.say(`{"id":` + itoa(requestID(t, req)) + `,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":47,"windowDurationMins":10080,"resetsAt":1790060019},"secondary":null,"planType":"prolite"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1790000000},"secondary":{"usedPercent":47,"windowDurationMins":10080,"resetsAt":1790060019},"planType":"prolite"},"codex_spark":{"limitId":"codex_spark","limitName":"GPT-5.3-Codex-Spark","primary":{"usedPercent":3,"windowDurationMins":10080,"resetsAt":null},"secondary":null}}}}`)
	e := next(t, r)
	if e.Type != agent.EventUsage || e.Usage == nil || e.Billing != agent.BillingSubscription {
		t.Fatalf("usage = %+v", e)
	}
	want := []agent.UsageWindow{
		{Name: "5h", Used: 12, ResetsAt: time.Unix(1790000000, 0)},
		{Name: "7d", Used: 47, ResetsAt: time.Unix(1790060019, 0)},
		{Name: "GPT-5.3-Codex-Spark", Used: 3},
	}
	if e.Usage.Plan != "prolite" || len(e.Usage.Windows) != len(want) {
		t.Fatalf("usage = %+v", e.Usage)
	}
	for i, w := range want {
		if g := e.Usage.Windows[i]; g.Name != w.Name || g.Used != w.Used || !g.ResetsAt.Equal(w.ResetsAt) {
			t.Errorf("window %d = %+v, want %+v", i, g, w)
		}
	}
	f.exit()
}

// A failed lookup costs the meter, never the session: no error event, and
// the next message still reaches the agent.
func TestUsageLookupFailureIsQuiet(t *testing.T) {
	f := newFakeProc()
	r := readyAs(t, f, `{"account":{"type":"chatgpt","email":"x@y","planType":"plus"},"requiresOpenaiAuth":true}`)
	f.say(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-1","turn":{"id":"turn-1","status":"completed"}}}`)
	if e := next(t, r); e.Type != agent.EventResult {
		t.Fatalf("result = %+v", e)
	}
	req := f.wrote(t, `"method":"account/rateLimits/read"`)
	f.say(`{"id":` + itoa(requestID(t, req)) + `,"error":{"code":-32603,"message":"backend unavailable"}}`)
	select {
	case e := <-r.Events():
		t.Fatalf("failed usage lookup sent %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
	if err := r.Send(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	f.wrote(t, `"method":"turn/start"`)
	f.exit()
}

// An older CLI without account/read still starts the thread; billing is
// then unknown, as it was before the driver asked.
func TestAccountReadUnsupported(t *testing.T) {
	f := newFakeProc()
	r, err := (&Agent{Binary: "codex"}).Start(context.Background(), fakeEnv{f}, agent.Definition{Kind: agent.KindCodex}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	f.wrote(t, `"method":"initialize"`)
	f.say(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	f.wrote(t, `"method":"account/read"`)
	f.say(`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"method not found"}}`)
	f.wrote(t, `"method":"thread/start"`)
	f.say(`{"jsonrpc":"2.0","id":3,"result":{"thread":{"id":"thr-1"}}}`)
	if e := next(t, r); e.Type != agent.EventInit || e.Billing != agent.BillingUnknown {
		t.Fatalf("init = %+v", e)
	}
	f.exit()
}

func requestID(t *testing.T, line string) int64 {
	t.Helper()
	var m struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("bad request %q: %v", line, err)
	}
	return m.ID
}
func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// TestLiveShellUnwrap pins the one protocol detail that no fixture can keep
// honest: app-server reports a command as the shell invocation it runs, and
// everything above this package reads a command by what it begins with.
func TestLiveShellUnwrap(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") != "1" {
		t.Skip("set DISPATCH_LIVE=1 to run against the real Codex CLI")
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/hello.txt", []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := envlocal.Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := New().Start(context.Background(), env, agent.Definition{Kind: agent.KindCodex, PermissionMode: agent.PermissionBypass},
		"Run exactly `cat hello.txt` in the shell, then reply DONE.")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	deadline := time.After(120 * time.Second)
	for {
		select {
		case ev, ok := <-r.Events():
			if !ok {
				t.Fatal("Codex ended without running the command")
			}
			if ev.Type == agent.EventToolUse && ev.Tool == agent.ToolBash {
				cmd, _ := ev.ToolInput["command"].(string)
				if !strings.HasPrefix(cmd, "cat ") {
					t.Fatalf("command reached dispatch wrapped: %q", cmd)
				}
				return
			}
			if ev.Type == agent.EventError {
				t.Fatalf("Codex error: %s", ev.Text)
			}
		case <-deadline:
			t.Fatal("Codex did not run a command")
		}
	}
}
