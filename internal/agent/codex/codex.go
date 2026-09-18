// Package codex drives the Codex CLI app-server JSON-RPC protocol.
//
// Unlike `codex exec`, app-server keeps a thread open, streams tool and text
// events, accepts mid-turn steering, and sends approval requests back to its
// client. That is the part of Codex needed for dispatch's interactive agent
// contract.
//
// Three things the protocol does that this driver has to undo, because every
// layer above expects the vocabulary the claude driver established:
//
//   - A thread's sandbox, approval policy, model and developer instructions
//     are *not* remembered across `thread/resume`: a bare resume falls back to
//     the host's own config.toml. Every resume re-sends them (threadParams).
//   - A command is reported as the shell invocation Codex runs it with
//     (`/usr/bin/zsh -lc 'gh pr create …'`). internal/work reads a command by
//     what it begins with and auto_allow matches a prefix, so the wrapper is
//     stripped (shellCommand) before the command leaves this package.
//   - Assistant text arrives twice, once as deltas and once whole. Only the
//     whole message is emitted; a surface that posted every delta would post a
//     message and write a log record per word.
//
// And one thing the protocol leaves out and the driver has to ask for: nothing
// on a turn says how it is paid for, and Codex prices nothing, so a result
// carries no cost. The handshake asks `account/read` before the thread starts
// — a ChatGPT login is a subscription and an API key is metered (agent.Billing,
// stamped on init and every result). On a subscription each ended turn is
// followed by `account/rateLimits/read`, reported as agent.EventUsage the way
// the claude driver reports get_usage: the plan's windows, not a dollar figure
// nobody is charged. Neither lookup can fail the session; an answer that never
// comes costs the meter.
package codex

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
)

const shareFilesHint = "You are operated through a chat channel. The human cannot open files on this machine. To show them a file you produced, write its absolute path on its own in your reply; dispatch uploads every mentioned path that exists. Files the human attaches are copied into this environment and their paths are listed at the end of that message."

// Agent implements agent.Agent for Codex app-server (Codex CLI 0.153+).
type Agent struct {
	Binary      string
	Credentials string // host auth.json lent to Docker environments; empty uses Codex's default
}

func New() *Agent                 { return &Agent{Binary: "codex"} }
func (a *Agent) Kind() agent.Kind { return agent.KindCodex }

func (a *Agent) Start(ctx context.Context, env environment.Environment, def agent.Definition, prompt string) (agent.Run, error) {
	return a.start(ctx, env, def, "", prompt)
}

func (a *Agent) Resume(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	if session == "" {
		return nil, fmt.Errorf("codex: resume requires a session id")
	}
	return a.start(ctx, env, def, session, prompt)
}

func (a *Agent) start(ctx context.Context, env environment.Environment, def agent.Definition, session, prompt string) (agent.Run, error) {
	bin := a.Binary
	if bin == "" {
		bin = "codex"
	}
	hint := a.lendLogin(ctx, env, def)
	proc, err := env.Exec(ctx, bin, "app-server")
	if err != nil {
		return nil, fmt.Errorf("codex: exec app-server: %w", err)
	}
	r := &run{proc: proc, events: make(chan agent.Event, 64), pending: map[string]int64{}, edits: map[string]map[string]any{}, steers: map[int64]string{}, usageReqs: map[int64]bool{}, prompt: prompt, resume: session, loginHint: hint, done: make(chan struct{})}
	if err := r.request(initRequest, "initialize", map[string]any{"clientInfo": map[string]any{"name": "dispatch", "version": "1"}}); err != nil {
		_ = proc.Kill()
		return nil, fmt.Errorf("codex: initialize: %w", err)
	}
	go r.drainStderr()
	go r.loop(def)
	return r, nil
}

const (
	initRequest    int64 = 1
	accountRequest int64 = 2
	threadRequest  int64 = 3
	turnRequest    int64 = 4
)

// methodNotFound is the JSON-RPC code dispatch answers a server request it
// does not implement with. Silence is not an option: Codex waits for every
// request it sends, so an unanswered one hangs the turn forever.
const methodNotFound = -32601

type run struct {
	proc                         environment.Process
	events                       chan agent.Event
	writeMu                      sync.Mutex
	mu                           sync.Mutex
	pending                      map[string]int64          // approval key -> server request id
	edits                        map[string]map[string]any // fileChange item id -> its input, for the approval that follows
	steers                       map[int64]string          // our request id -> the text it carried, to re-send if the turn ended first
	thread, turn, prompt, resume string
	next                         int64
	stderr                       strings.Builder
	loginHint                    string
	done                         chan struct{}
	ended                        bool           // loop goroutine only: this turn already reported an ending
	billing                      agent.Billing  // loop goroutine only: from account/read, stamped on init and results
	plan                         string         // loop goroutine only: the ChatGPT plan account/read named
	usageReqs                    map[int64]bool // loop goroutine only: ids of account/rateLimits/read requests in flight
}

func (r *run) Events() <-chan agent.Event { return r.events }

func mode(m agent.PermissionMode) (approval, sandbox string) {
	switch m {
	case agent.PermissionBypass:
		return "never", "danger-full-access"
	case agent.PermissionAcceptEdits, agent.PermissionAuto:
		return "on-request", "workspace-write"
	default:
		return "untrusted", "read-only"
	}
}

// threadParams is what a thread runs as. thread/resume takes the same keys as
// thread/start and inherits none of them, so a resumed thread that is not
// told again falls back to the *host's* Codex defaults — a manual definition
// would come back from its first idle timeout with a full-access sandbox.
func threadParams(def agent.Definition) map[string]any {
	approval, sandbox := mode(def.PermissionMode)
	p := map[string]any{"approvalPolicy": approval, "sandbox": sandbox, "developerInstructions": strings.TrimSpace(shareFilesHint + "\n\n" + def.SystemPrompt), "serviceName": "dispatch"}
	if def.Model != "" {
		p["model"] = def.Model
	}
	return p
}
func turnParams(thread, prompt string) map[string]any {
	return map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": prompt}}}
}

func (r *run) Send(ctx context.Context, text string) error {
	r.mu.Lock()
	thread, turn := r.thread, r.turn
	r.mu.Unlock()
	if thread == "" {
		return fmt.Errorf("codex: session is not ready")
	}
	if turn == "" {
		return r.request(r.id(), "turn/start", turnParams(thread, text))
	}
	// turn/steer requires the turn id as a precondition and fails if the turn
	// ended in between; the text is then remembered so the error handler can
	// deliver it as a turn of its own.
	id := r.id()
	r.mu.Lock()
	r.steers[id] = text
	r.mu.Unlock()
	return r.request(id, "turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn, "input": []map[string]string{{"type": "text", "text": text}}})
}
func (r *run) Decide(ctx context.Context, d agent.PermissionDecision) error {
	r.mu.Lock()
	id, ok := r.pending[d.ToolID]
	if ok {
		delete(r.pending, d.ToolID)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("codex: no pending permission for tool id %q", d.ToolID)
	}
	decision := "decline"
	if d.Allow {
		decision = "accept"
	}
	return r.reply(id, map[string]any{"decision": decision})
}
func (r *run) Stop() error {
	r.mu.Lock()
	thread, turn := r.thread, r.turn
	r.mu.Unlock()
	if thread != "" && turn != "" {
		_ = r.request(r.id(), "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	}
	r.writeMu.Lock()
	_ = r.proc.Stdin().Close()
	r.writeMu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-time.After(5 * time.Second):
		return r.proc.Kill()
	}
}
func (r *run) id() int64 { r.mu.Lock(); defer r.mu.Unlock(); r.next++; return turnRequest + r.next }
func (r *run) request(id int64, method string, params any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}
func (r *run) reply(id int64, result any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func (r *run) replyError(id int64, code int, message string) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func (r *run) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err = r.proc.Stdin().Write(append(b, '\n'))
	return err
}
func (r *run) drainStderr() {
	sc := bufio.NewScanner(r.proc.Stderr())
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		r.mu.Lock()
		if r.stderr.Len() < 16*1024 {
			r.stderr.WriteString(sc.Text() + "\n")
		}
		r.mu.Unlock()
	}
}

func (r *run) loop(def agent.Definition) {
	defer close(r.events)
	defer close(r.done)
	sc := bufio.NewScanner(r.proc.Stdout())
	sc.Buffer(make([]byte, 0, 256*1024), 16<<20)
	for sc.Scan() {
		raw := append([]byte(nil), sc.Bytes()...)
		var m message
		if err := json.Unmarshal(raw, &m); err != nil {
			r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: "codex: bad line: " + err.Error(), Raw: raw}
			continue
		}
		// A server-initiated approval is also a JSON-RPC request and so has
		// an id. Only messages without a method are responses to requests we
		// sent; requests continue into translate so the human can answer them.
		if m.ID != nil && m.Method == "" {
			if m.Error != nil {
				r.responseError(*m.ID, m.Error, raw, def)
			} else {
				r.response(*m.ID, m, raw, def)
			}
			continue
		}
		if m.Method == "turn/started" {
			r.ended = false // a new turn can end on its own account
		}
		out, known := r.translate(m, raw)
		if m.ID != nil && !known {
			// An unimplemented server request — a permission escalation, an
			// MCP elicitation, a tool asking the human something Codex has no
			// dispatch mapping for yet. Refusing it lets the turn continue.
			_ = r.replyError(*m.ID, methodNotFound, "dispatch does not implement "+m.Method)
		}
		for _, ev := range out {
			if ev.Type == agent.EventResult || ev.Type == agent.EventError {
				r.ended = true
				ev.Billing = r.billing
			}
			r.events <- ev
		}
		if m.Method == "turn/completed" && len(out) > 0 && r.billing == agent.BillingSubscription {
			// Asked after the result went out, so the closing line never
			// waits for the lookup; the answer becomes an EventUsage.
			id := r.id()
			r.usageReqs[id] = true
			_ = r.request(id, "account/rateLimits/read", nil)
		}
	}
	code, _ := r.proc.Wait()
	if code != 0 && !r.ended {
		r.mu.Lock()
		tail := strings.TrimSpace(r.stderr.String())
		r.mu.Unlock()
		r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: r.errorText(fmt.Sprintf("codex exited with code %d: %s", code, tail))}
	}
}

// response handles a successful answer to a request dispatch sent: the three
// setup requests drive the handshake (initialize, account/read, then the
// thread), a rate-limit answer becomes an EventUsage, and anything else only
// settles bookkeeping.
func (r *run) response(id int64, m message, raw []byte, def agent.Definition) {
	switch id {
	case initRequest:
		_ = r.write(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
		_ = r.request(accountRequest, "account/read", map[string]any{})
	case accountRequest:
		r.billing, r.plan = accountBilling(m.Result)
		r.startThread(def)
	case threadRequest:
		var x struct {
			Thread struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"thread"`
		}
		_ = json.Unmarshal(m.Result, &x)
		if x.Thread.ID == "" {
			r.ended = true
			r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: "codex: thread start returned no id", Raw: raw}
			return
		}
		r.mu.Lock()
		r.thread = x.Thread.ID
		r.mu.Unlock()
		model := x.Thread.Model
		if model == "" {
			model = def.Model
		}
		r.events <- agent.Event{Type: agent.EventInit, At: time.Now(), Session: x.Thread.ID, Model: model, Mode: def.PermissionMode, Billing: r.billing, Raw: raw}
		_ = r.request(turnRequest, "turn/start", turnParams(x.Thread.ID, r.prompt))
	default:
		r.mu.Lock()
		delete(r.steers, id)
		r.mu.Unlock()
		usage := r.usageReqs[id]
		delete(r.usageReqs, id)
		if usage {
			if u := parseRateLimits(m.Result, r.plan); u != nil {
				r.events <- agent.Event{Type: agent.EventUsage, At: time.Now(), Usage: u, Billing: r.billing, Raw: raw}
			}
		}
	}
}

// startThread opens the thread the run's turns go to: a new one, or the
// session being resumed.
func (r *run) startThread(def agent.Definition) {
	p := threadParams(def)
	if r.resume != "" {
		p["threadId"] = r.resume
		_ = r.request(threadRequest, "thread/resume", p)
	} else {
		_ = r.request(threadRequest, "thread/start", p)
	}
}

// accountBilling reads an account/read answer: a ChatGPT login is a plan,
// an API key (or a cloud provider's) is metered. No account at all — an
// older CLI, a login the server could not read — is unknown.
func accountBilling(raw json.RawMessage) (agent.Billing, string) {
	var x struct {
		Account *struct {
			Type     string `json:"type"`
			PlanType string `json:"planType"`
		} `json:"account"`
	}
	if json.Unmarshal(raw, &x) != nil || x.Account == nil {
		return agent.BillingUnknown, ""
	}
	switch x.Account.Type {
	case "chatgpt":
		return agent.BillingSubscription, planName(x.Account.PlanType)
	case "apiKey", "amazonBedrock":
		return agent.BillingAPIKey, ""
	}
	return agent.BillingUnknown, ""
}

func planName(p string) string {
	if p == "unknown" {
		return ""
	}
	return p
}

type rateWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int64   `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}
type rateBucket struct {
	LimitID   string      `json:"limitId"`
	LimitName string      `json:"limitName"`
	PlanType  string      `json:"planType"`
	Primary   *rateWindow `json:"primary"`
	Secondary *rateWindow `json:"secondary"`
}

// parseRateLimits reads an account/rateLimits/read answer into agent.Usage:
// the `codex` bucket's windows first, then any other bucket — a model with a
// quota of its own — under its name. Within a bucket the shorter window comes
// first whichever slot Codex sent it in: a plan with only a weekly window
// reports it as primary. nil when there is no window to show.
func parseRateLimits(raw json.RawMessage, plan string) *agent.Usage {
	var x struct {
		RateLimits *rateBucket            `json:"rateLimits"`
		ByID       map[string]*rateBucket `json:"rateLimitsByLimitId"`
	}
	if json.Unmarshal(raw, &x) != nil {
		return nil
	}
	buckets := make([]*rateBucket, 0, len(x.ByID)+1)
	main := x.ByID["codex"]
	if main == nil {
		main = x.RateLimits
	}
	if main != nil {
		buckets = append(buckets, main)
	}
	ids := make([]string, 0, len(x.ByID))
	for id := range x.ByID {
		if id != "codex" && x.ByID[id] != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		buckets = append(buckets, x.ByID[id])
	}
	u := &agent.Usage{Plan: plan}
	for i, b := range buckets {
		if u.Plan == "" {
			u.Plan = planName(b.PlanType)
		}
		name := b.LimitName
		if name == "" {
			name = b.LimitID
		}
		label := ""
		if i > 0 {
			label = name
		}
		ws := make([]*rateWindow, 0, 2)
		for _, w := range []*rateWindow{b.Primary, b.Secondary} {
			if w != nil {
				ws = append(ws, w)
			}
		}
		sort.SliceStable(ws, func(i, j int) bool { return shorter(ws[i].WindowDurationMins, ws[j].WindowDurationMins) })
		for _, w := range ws {
			uw := agent.UsageWindow{Name: windowName(label, w.WindowDurationMins, len(ws) > 1), Used: w.UsedPercent}
			if uw.Name == "" {
				// The plan's own window with no span to name it by.
				uw.Name = cmp.Or(name, "codex")
			}
			if w.ResetsAt > 0 {
				uw.ResetsAt = time.Unix(w.ResetsAt, 0)
			}
			u.Windows = append(u.Windows, uw)
		}
	}
	if len(u.Windows) == 0 {
		return nil
	}
	return u
}

// shorter orders windows by span; one with no span sorts last.
func shorter(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return a > 0 && b <= 0
	}
	return a < b
}

// windowName is "5h" or "7d" for the plan's own windows, and a model's
// bucket's name for its quota — with the span too when the bucket has two.
func windowName(label string, mins int64, both bool) string {
	span := ""
	switch {
	case mins <= 0:
	case mins%(24*60) == 0:
		span = fmt.Sprintf("%dd", mins/(24*60))
	case mins%60 == 0:
		span = fmt.Sprintf("%dh", mins/60)
	default:
		span = fmt.Sprintf("%dm", mins)
	}
	switch {
	case label == "":
		return span
	case both && span != "":
		return label + " " + span
	}
	return label
}

// responseError handles a JSON-RPC error answering one of dispatch's own
// requests. Only initialize and the thread request are fatal — without a
// thread there is no turn that could ever end, so the process is torn down
// rather than left waiting. account/read is the setup request that is not:
// an unknown billing costs the usage meter, so the thread starts anyway, and
// a failed rate-limit lookup after a turn is dropped the same way. A steer
// that lost its race with the turn's end is re-sent as a turn of its own;
// every other error is reported and the session kept, which is what makes a
// warm session survive one bad request.
func (r *run) responseError(id int64, e *rpcError, raw []byte, def agent.Definition) {
	r.mu.Lock()
	text, steered := r.steers[id]
	delete(r.steers, id)
	thread := r.thread
	r.mu.Unlock()
	usage := r.usageReqs[id]
	delete(r.usageReqs, id)
	switch {
	case id == accountRequest:
		// Not knowing how the session is paid for costs the usage meter, not
		// the session.
		r.startThread(def)
		return
	case usage:
		return // the turn already ended; a failed lookup just shows no meter
	}
	if steered && thread != "" {
		r.mu.Lock()
		r.turn = ""
		r.mu.Unlock()
		if err := r.request(r.id(), "turn/start", turnParams(thread, text)); err == nil {
			return
		}
	}
	r.ended = true
	r.events <- agent.Event{Type: agent.EventError, At: time.Now(), Text: r.errorText("codex: " + e.Message), Raw: raw}
	if id == initRequest || id == threadRequest {
		_ = r.proc.Kill() // a failed setup has no turn that could later end
	}
}

func (r *run) errorText(text string) string {
	if r.loginHint != "" && strings.Contains(strings.ToLower(text), "login") {
		return text + r.loginHint
	}
	return text
}

type rpcError struct {
	Message string `json:"message"`
}
type message struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type fileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Diff string `json:"diff"`
}

// translate maps one app-server message to dispatch events. known is false
// for a method this driver does not implement, which the caller answers for
// when the message was a request.
func (r *run) translate(m message, raw []byte) (out []agent.Event, known bool) {
	now := time.Now()
	base := agent.Event{At: now, Raw: raw}
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Item struct {
			ID               string       `json:"id"`
			Type             string       `json:"type"`
			Text             string       `json:"text"`
			Command          string       `json:"command"`
			Status           string       `json:"status"`
			AggregatedOutput string       `json:"aggregatedOutput"`
			Changes          []fileChange `json:"changes"`
		} `json:"item"`
		ItemID     string `json:"itemId"`
		ApprovalID string `json:"approvalId"`
		Command    string `json:"command"`
		Reason     string `json:"reason"`
	}
	_ = json.Unmarshal(m.Params, &p)
	base.Session = p.ThreadID
	// Codex runs sub-agents as threads of their own on the same connection.
	// Their notifications are not this run's: a sub-thread's turn/completed
	// would end the human's turn and its id would overwrite the session.
	// Requests are still answered whatever thread they belong to — a
	// sub-agent left waiting for an approval hangs the turn that spawned it.
	if m.ID == nil && p.ThreadID != "" {
		r.mu.Lock()
		mine := r.thread
		r.mu.Unlock()
		if mine != "" && p.ThreadID != mine {
			return nil, true
		}
	}
	switch m.Method {
	case "turn/started":
		r.mu.Lock()
		r.turn = p.Turn.ID
		r.mu.Unlock()
	case "item/agentMessage/delta":
		// Deltas are dropped: item/completed carries the same text whole.
	case "item/started":
		switch p.Item.Type {
		case "commandExecution":
			out = append(out, event(base, agent.EventToolUse, agent.ToolBash, p.Item.ID, "", map[string]any{"command": shellCommand(p.Item.Command)}))
		case "fileChange":
			in := editInput(p.Item.Changes)
			r.mu.Lock()
			r.edits[p.Item.ID] = in
			r.mu.Unlock()
			out = append(out, event(base, agent.EventToolUse, agent.ToolEdit, p.Item.ID, "", in))
		}
	case "item/completed":
		switch p.Item.Type {
		case "commandExecution":
			out = append(out, event(base, agent.EventToolResult, agent.ToolBash, p.Item.ID, p.Item.AggregatedOutput, nil))
		case "fileChange":
			r.mu.Lock()
			delete(r.edits, p.Item.ID)
			r.mu.Unlock()
			out = append(out, event(base, agent.EventToolResult, agent.ToolEdit, p.Item.ID, "", nil))
		case "agentMessage":
			if p.Item.Text != "" {
				out = append(out, event(base, agent.EventText, "", p.Item.ID, p.Item.Text, nil))
			}
		}
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		tool := agent.ToolBash
		input := map[string]any{"command": shellCommand(p.Command)}
		if m.Method == "item/fileChange/requestApproval" {
			// The approval params carry ids only; the changes came with the
			// item/started of the item that is now asking.
			tool = agent.ToolEdit
			r.mu.Lock()
			input = r.edits[p.ItemID]
			r.mu.Unlock()
		}
		// One item can ask several times (a shell bridge approves each
		// subcommand); approvalId is what tells those callbacks apart.
		key := p.ItemID
		if p.ApprovalID != "" {
			key = p.ApprovalID
		}
		r.mu.Lock()
		if m.ID != nil {
			r.pending[key] = *m.ID
		}
		r.mu.Unlock()
		out = append(out, event(base, agent.EventNeedsPermission, tool, key, p.Reason, input))
	case "turn/completed":
		r.mu.Lock()
		r.turn = ""
		r.mu.Unlock()
		if p.Turn.Error != nil {
			out = append(out, event(base, agent.EventError, "", "", r.errorText(p.Turn.Error.Message), nil))
		} else {
			out = append(out, event(base, agent.EventResult, "", "", "", nil))
		}
	default:
		return nil, false
	}
	return out, true
}

// editInput describes a patch the way the layers above read an edit: one file
// is named by file_path, which is what the chat surface shows.
func editInput(changes []fileChange) map[string]any {
	in := map[string]any{}
	if len(changes) == 1 {
		in["file_path"] = changes[0].Path
	}
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	if len(paths) > 0 {
		in["files"] = paths
	}
	return in
}

// shells are the interpreters Codex wraps a command in.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// shellCommand unwraps `/usr/bin/zsh -lc 'gh pr create …'` to the command the
// agent actually asked for. Everything above this package reads a command by
// what it *begins* with — internal/work only believes a `gh` that starts the
// line, auto_allow matches `Bash(git:*)` on a prefix — so the wrapper would
// hide every command from all of them. Anything that is not exactly a shell
// invoked with a single-quoted -c script is returned untouched.
func shellCommand(cmd string) string {
	bin, rest, ok := strings.Cut(strings.TrimSpace(cmd), " ")
	if !ok || !shells[path.Base(bin)] {
		return cmd
	}
	flag, script, ok := strings.Cut(strings.TrimSpace(rest), " ")
	if !ok || !strings.HasPrefix(flag, "-") || !strings.HasSuffix(flag, "c") {
		return cmd
	}
	if s, ok := unquote(strings.TrimSpace(script)); ok {
		return s
	}
	return cmd
}

// unquote reads one POSIX single-quoted word, including the '\” form an
// embedded quote takes. It fails unless the word is the whole of s, so
// `zsh -lc 'a' && 'b'` — two words — is left alone.
func unquote(s string) (string, bool) {
	if len(s) < 2 || s[0] != '\'' {
		return "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i == len(s)-1 {
			return b.String(), true
		}
		if strings.HasPrefix(s[i:], `'\''`) {
			b.WriteByte('\'')
			i += 4
			continue
		}
		return "", false
	}
	return "", false
}

func event(base agent.Event, typ agent.EventType, tool, id, text string, input map[string]any) agent.Event {
	base.Type = typ
	base.Tool = tool
	base.ToolID = id
	base.Text = text
	base.ToolInput = input
	return base
}
