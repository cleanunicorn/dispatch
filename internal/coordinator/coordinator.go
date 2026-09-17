// Package coordinator is the long-running brain. It owns tasks: it turns
// surface intents into executor work, fans executor/agent events back out
// to every surface, relays permission decisions (or lets a decider answer
// the ones inside the operator's auto_allow ceiling — see decide.go), and
// persists everything
// in the store so a restart can resume sessions.
//
//	transports --Inbound--> surfaces --Intent--> Coordinator --Task--> Executor
//	transports <--Outbound-- surfaces <--Event-- Coordinator <--agent.Event--
//
// It also owns which conversations are still open: a closed thread (see
// CloseThread) is not re-seeded on the transport, not resumed on a restart
// and not spoken to, until a human brings work back to it.
//
// And it keeps humans told where every thread stands: every Heartbeat
// while a turn runs it broadcasts surface.EventHeartbeat (the chat
// surface's status line lives on it), and on a transport that can react it
// keeps one mark on the thread's root message — ⏳ while the agent works,
// ✋ while it waits for a decision, 📬 once it has answered and the thread
// waits for its next message, ❌ when the task failed, ✅ once the thread
// is closed. A task thread is never bare: it is being worked on, waiting
// on a human, or closed (see mark).
//
// Where a thread stands includes where its code went. At the end of a
// turn and on an answered `status` it reads the thread's own records back
// for the repository, branch, pull request and issue being worked on
// (overview.go, over internal/work) and hands the answer to the surfaces
// on surface.Event.Work, so each renders it its own way.
package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/decider"
	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/executor"
	execlocal "github.com/cleanunicorn/dispatch/internal/executor/local"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

// Coordinator wires transports, surfaces, executor and store together.
type Coordinator struct {
	Store      store.Store
	Executor   executor.Executor
	Transports []transport.Transport
	Surfaces   []surface.Surface // offered inbound messages in this order
	Log        *slog.Logger
	// DefaultDefinition is used by RunTask intents without an agent name
	// (or whose agent name is unknown, in which case the name is treated
	// as part of the prompt).
	DefaultDefinition string
	// ChannelAgents overrides DefaultDefinition per channel, keyed by
	// "<transport>/<channel>" (the channel is the part of the thread id
	// before the first "/"). Read and updated under mu once Run started.
	ChannelAgents map[string]string
	// SaveChannelAgent persists a per-channel default set from chat (the
	// config file). Nil keeps it in memory only.
	SaveChannelAgent func(ctx context.Context, transportName, channel, agent string) error
	// WorkdirRoot hosts per-task working directories for definitions that
	// do not pin one.
	WorkdirRoot string
	// DrainTimeout bounds how long Run waits for live tasks to stop after
	// its context is cancelled (executors drain in-flight tool calls).
	DrainTimeout time.Duration
	// SaveDefinition persists a definition created from chat outside the
	// store (the config file), so it survives a re-seed on restart. Nil
	// keeps it in the store only.
	SaveDefinition func(ctx context.Context, d agent.Definition) error
	// UpdateDefinition persists a definition changed from chat outside the
	// store (the config file). Nil keeps the change in the store only.
	UpdateDefinition func(ctx context.Context, d agent.Definition) error
	// DeleteDefinition removes a definition deleted from chat outside the
	// store (the config file). Nil removes it from the store only.
	DeleteDefinition func(ctx context.Context, name string) error
	// AutoResume continues tasks that a restart cut short as soon as dispatch
	// is back, instead of waiting for a message on their thread.
	AutoResume bool
	// ResumePrompt is what an auto-resumed session is told; empty uses
	// defaultResumePrompt.
	ResumePrompt string
	// AutoResumeWithin skips tasks last touched longer ago than this, so a
	// restart after a long stop does not relaunch stale work (default 12h).
	AutoResumeWithin time.Duration
	// MaxAutoResumes caps consecutive automatic resumes of one task, so a
	// task that keeps taking dispatch down cannot restart-loop (default 3).
	MaxAutoResumes int
	// Heartbeat is how often surfaces hear that a running turn is still
	// going (default 10s). Negative turns heartbeats off.
	Heartbeat time.Duration
	// Decider answers policy questions the rules alone answer bluntly (see
	// internal/decider). Nil is decider.Static: dispatch's own rules, which
	// is also what every failure falls back to.
	Decider decider.Decider
	// DeciderUses lists the question kinds Decider may answer; other kinds
	// are answered statically. Empty allows none, so turning a decider on
	// is always a deliberate, per-kind step.
	DeciderUses []string
	// DeciderTimeout bounds one question (default 15s). A decision never
	// blocks dispatch: on timeout the static answer wins.
	DeciderTimeout time.Duration
	// MaxDecisionsPerTask caps how many questions one task may cost before
	// it falls back to the rules for good (default 20).
	MaxDecisionsPerTask int
	// AutoAllow is the ceiling for permission decisions: tool patterns
	// ("Read", "Bash(go test:*)") a decider may approve without a human.
	// Empty — the default — means every prompt reaches a person, whatever
	// the decider thinks.
	AutoAllow []string
	// PlannerAgent is the definition that writes on-demand workflows
	// (`plan <what you want>`, see plan.go). Empty — the default — means
	// dispatch has no such word: composing a workflow with a model is a
	// deliberate, configured step, and without it dispatch runs only the
	// workflows somebody wrote down. A message beginning "plan" is then
	// an ordinary message and reaches the thread's agent unchanged;
	// `workflows` is where the unset feature says it exists.
	PlannerAgent string
	// SaveWorkflow persists a workflow made from chat outside the store
	// (the config file), so it survives a restart. Nil keeps it in
	// memory only, and says so when asked to save one.
	SaveWorkflow func(ctx context.Context, d workflow.Definition) error
	// AgentKinds are the agent kinds the executor has a driver for, in
	// agent.Kinds order. The add-agent wizard asks which one to use when
	// there is more than one; the first is the default.
	AgentKinds []agent.Kind
	// Workflows are the workflow definitions config seeded, startable by
	// name (see workflow.go — the runner) — and the built-ins the bare
	// end-of-thread words run as do not come from here (finish.go).
	Workflows []workflow.Definition

	drives sync.WaitGroup

	transports map[string]transport.Transport

	mu        sync.Mutex
	threads   map[transport.ThreadID]executor.TaskID     // live task per thread
	owner     map[executor.TaskID]string                 // task -> surface that started it
	pending   map[string]chan transport.Decision         // prompt base id -> waiter
	askText   map[transport.ThreadID]string              // thread -> prompt base id accepting a typed answer
	wizards   map[transport.ThreadID]context.CancelFunc  // open question flows (agent add/edit/delete, run picker)
	closed    map[transport.ThreadID]bool                // threads a human ended; projection of the store
	plans     map[transport.ThreadID]workflow.Definition // last plan made on a thread, for `workflow save` (plan.go)
	merging   map[transport.ThreadID]bool                // threads with a merge in flight (finish.go)
	workflows map[transport.ThreadID]*workflowRun        // named workflow runs (workflow.go)
	turnEnds  map[transport.ThreadID][]chan turnEnd      // waiters for a turn's end (turns.go)
	sinks     map[executor.TaskID]*taskSink              // live tasks, for follow-up heartbeats
	cutShort  map[executor.TaskID]bool                   // tasks the previous process left mid-turn (seedCutShort)
	marks     map[transport.ThreadID]string              // reaction currently on a thread's root message
	markMu    map[transport.ThreadID]*sync.Mutex         // serializes a thread's mark swap (see mark)
	outMu     map[transport.ThreadID]*sync.Mutex         // serializes render+send per thread (keyed messages need order)
	hosts     map[transport.ThreadID]string              // transport hosting a thread, once known (see threads.go)
	titles    map[transport.ThreadID]string              // first human line of a thread, once read
}

// New returns a Coordinator.
func New(st store.Store, ex executor.Executor, transports []transport.Transport, surfaces []surface.Surface, log *slog.Logger) *Coordinator {
	if log == nil {
		log = slog.Default()
	}
	c := &Coordinator{
		Store: st, Executor: ex, Transports: transports, Surfaces: surfaces, Log: log,
		transports: map[string]transport.Transport{},
		threads:    map[transport.ThreadID]executor.TaskID{},
		owner:      map[executor.TaskID]string{},
		pending:    map[string]chan transport.Decision{},
		askText:    map[transport.ThreadID]string{},
		wizards:    map[transport.ThreadID]context.CancelFunc{},
		closed:     map[transport.ThreadID]bool{},
		merging:    map[transport.ThreadID]bool{},
		workflows:  map[transport.ThreadID]*workflowRun{},
		turnEnds:   map[transport.ThreadID][]chan turnEnd{},
		sinks:      map[executor.TaskID]*taskSink{},
		cutShort:   map[executor.TaskID]bool{},
		marks:      map[transport.ThreadID]string{},
		markMu:     map[transport.ThreadID]*sync.Mutex{},
		outMu:      map[transport.ThreadID]*sync.Mutex{},
		hosts:      map[transport.ThreadID]string{},
		titles:     map[transport.ThreadID]string{},
		plans:      map[transport.ThreadID]workflow.Definition{},
	}
	for _, t := range transports {
		c.transports[t.Name()] = t
	}
	return c
}

// Run blocks until ctx is cancelled.
func (c *Coordinator) Run(ctx context.Context) error {
	for _, s := range c.Surfaces {
		if _, ok := c.transports[s.Transport()]; !ok {
			return fmt.Errorf("coordinator: surface %q bound to unknown transport %q", s.Name(), s.Transport())
		}
	}
	if err := c.loadClosed(ctx); err != nil {
		return err
	}
	if err := c.seedMarks(ctx); err != nil {
		return err
	}
	if err := c.seedCutShort(ctx); err != nil {
		return err
	}
	if err := c.recover(ctx); err != nil {
		return err
	}
	c.seedThreads(ctx)
	c.resumeFlows(ctx)
	c.resumeWorkflows(ctx)
	inbox := make(chan transport.Inbound, 64)
	var wg sync.WaitGroup
	for _, t := range c.Transports {
		wg.Add(1)
		go func(t transport.Transport) {
			defer wg.Done()
			if err := t.Run(ctx, inbox); err != nil && !errors.Is(err, context.Canceled) {
				c.Log.Error("transport stopped", "transport", t.Name(), "err", err)
			}
		}(t)
	}
	for {
		select {
		case <-ctx.Done():
			c.shutdown(ctx)
			wg.Wait()
			return ctx.Err()
		case in := <-inbox:
			c.handle(ctx, in)
		}
	}
}

// shutdown tells every live thread that dispatch is restarting, then waits
// (bounded) for executors to drain and persist their final state.
func (c *Coordinator) shutdown(ctx context.Context) {
	timeout := c.DrainTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout+15*time.Second)
	defer cancel()

	c.mu.Lock()
	live := make(map[transport.ThreadID]executor.TaskID, len(c.threads))
	for th, id := range c.threads {
		live[th] = id
	}
	c.mu.Unlock()
	for th, id := range live {
		st, err := c.Store.GetTask(sctx, id)
		if err != nil {
			continue
		}
		// A task whose turn is done is only holding its process open for a
		// follow-up; stopping it cuts nothing short.
		tail := "reply in this thread to resume"
		if c.AutoResume && st.Status != store.StatusIdle {
			tail = "it continues on its own when dispatch is back"
		}
		c.Log.Info("shutdown: notifying live task", "task", id, "thread", th, "status", st.Status)
		c.emitTo(sctx, st.Transport, surface.Event{Kind: surface.EventReply, Thread: th, TaskID: id, Task: &st,
			Text: "⏸️ dispatch is restarting — the agent finishes its current step and stops; " + tail})
	}

	done := make(chan struct{})
	go func() { c.drives.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout + 10*time.Second):
		c.Log.Warn("shutdown: tasks still running after drain timeout")
	}
}

// seedCutShort remembers which tasks the previous process left mid-turn,
// before recover() normalises them.
//
// It exists because a result record is not proof that a turn finished. An
// agent being drained reports one on its way out and it lands in the log
// like any other, so a window cannot tell "the turn answered" from "the
// turn was cut off mid-sentence" — and a workflow step graded on that
// window would be passed for work that never happened.
//
// The task row knows: drive persists `interrupted` for a turn the stop
// caught before it answered, and `idle` for one it did not. But recover()
// runs first and, when a task is not going to continue by itself, marks it
// idle — the knowledge is gone by the time a workflow is resumed. So it is
// read here, once, in the moment between the previous process and this
// one's. That is the same thing seedMarks does with the reactions the last
// process left on Slack.
func (c *Coordinator) seedCutShort(ctx context.Context) error {
	// running and waiting_permission are dispatch dying without writing
	// anything else down; interrupted is a stop that caught a live turn;
	// queued never started. None of them answered.
	for _, status := range []string{store.StatusRunning, store.StatusWaitingPermission, store.StatusQueued, store.StatusInterrupted} {
		tasks, err := c.Store.ListTasks(ctx, status)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			c.cutShort[t.ID] = true
		}
	}
	if len(c.cutShort) > 0 {
		c.Log.Info("tasks left mid-turn by the last process", "tasks", len(c.cutShort))
	}
	return nil
}

// wasCutShort reports whether the previous process left this task's turn
// unfinished. It is only meaningful for the first grading after a start;
// a task that has run since has a result of its own in the window.
func (c *Coordinator) wasCutShort(id executor.TaskID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cutShort[id]
}

// turnRan clears a task from the cut-short set: it has since run a turn of
// its own under this process, so the window speaks for itself.
func (c *Coordinator) turnRan(id executor.TaskID) {
	c.mu.Lock()
	delete(c.cutShort, id)
	c.mu.Unlock()
}

// seedThreads tells thread-tracking transports about every stored task
// thread, so replies in old threads are still forwarded after a restart.
func (c *Coordinator) seedThreads(ctx context.Context) {
	tasks, err := c.Store.ListTasks(ctx, "")
	if err != nil {
		c.Log.Error("seed threads", "err", err)
		return
	}
	n := 0
	for _, t := range tasks {
		if c.threadClosed(t.Thread) {
			continue // closed on purpose: stay deaf until someone speaks up
		}
		for name, tr := range c.transports {
			if t.Transport != "" && t.Transport != name {
				continue
			}
			if tt, ok := tr.(transport.ThreadTracker); ok {
				tt.Remember(t.Thread)
				n++
			}
		}
	}
	c.Log.Info("seeded transport threads", "threads", n)
}

// defaultResumePrompt is the turn given to a session that a restart cut
// short. It has to stand on its own: the agent sees it as the next user
// message of the resumed session.
const defaultResumePrompt = "dispatch restarted and cut your last turn short. Continue the work in progress from where it stopped, without waiting for further instructions. If the task was already finished, say so in one line instead of redoing it."

// recover picks up the tasks that were mid-execution when dispatch stopped:
// interrupted (the stop cut the turn short), running or waiting_permission
// (dispatch died before it could write anything else), and queued (never
// started). A task that had finished its turn is idle and is left alone —
// it was waiting for a human, not for dispatch. With AutoResume the picked-up
// tasks continue on their own — resumed from their session, or started
// again when they never got one — so nobody has to type in the thread; the
// rest are marked idle and resume with the next message.
func (c *Coordinator) recover(ctx context.Context) error {
	// Deciding happens before the transports are up, so it is dead time on
	// every start. One question is seconds; a crash that left twenty tasks
	// behind would be minutes of a silent bot. The whole of recovery gets
	// one budget, and the tasks past it are answered by the rules.
	dctx, cancelDecisions := context.WithTimeout(ctx, c.recoveryBudget())
	defer cancelDecisions()

	var resume []resumable
	for _, status := range []string{store.StatusRunning, store.StatusWaitingPermission, store.StatusQueued, store.StatusInterrupted} {
		tasks, err := c.Store.ListTasks(ctx, status)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if c.threadClosed(t.Thread) {
				// A closed thread has no listener: mark the task idle so it
				// is not reported as live, but neither resume nor announce it.
				t.Status = store.StatusIdle
				if err := c.Store.PutTask(ctx, t); err != nil {
					return err
				}
				c.Log.Info("recovered task on a closed thread", "task", t.ID, "thread", t.Thread)
				continue
			}
			if _, running := c.transports[t.Transport]; t.Transport != "" && !running {
				// Nobody could see or answer it: leave it for a dispatch that
				// runs its transport, marked idle so it is not reported live.
				t.Status = store.StatusIdle
				if err := c.Store.PutTask(ctx, t); err != nil {
					return err
				}
				c.Log.Warn("not resuming task: its transport is not configured", "task", t.ID, "thread", t.Thread, "transport", t.Transport)
				continue
			}
			v := decider.Verdict{Action: actionWait}
			if c.autoResumable(t) {
				// The rules say this one may continue; the decider chooses
				// what actually happens to it, and may word the resume.
				v = c.decide(dctx, decider.Question{
					Kind: kindResume, Task: string(t.ID), Thread: string(t.Thread),
					Options: []string{actionContinue, actionWait, actionAsk, actionAbandon},
					Facts:   c.factsForResume(ctx, t),
					Static:  decider.Verdict{Action: actionContinue, Prompt: c.resumePrompt()},
				})
			}
			switch {
			case v.Action == actionContinue:
				t.Status = store.StatusIdle
				t.Resumes++
			case v.Action == actionAbandon:
				t.Status = store.StatusCancelled
			case v.Action == actionAsk:
				t.Status = store.StatusIdle // the question needs it pick-up-able
			case t.Session == "":
				t.Status = store.StatusFailed
			default:
				t.Status = store.StatusIdle
			}
			if err := c.Store.PutTask(ctx, t); err != nil {
				return err
			}
			c.Log.Info("recovered task", "task", t.ID, "status", t.Status, "action", v.Action, "resumes", t.Resumes)
			if v.Action == actionContinue {
				resume = append(resume, resumable{task: t, prompt: v.Prompt}) // drive marks it working
				continue
			}
			// From here the thread waits on a human: say so on its root
			// message, then in the thread.
			marked := t.Status
			if v.Action == actionAsk {
				marked = store.StatusWaitingPermission // the question is a decision
			}
			c.mark(ctx, t.Transport, t.Thread, marked)
			switch {
			case v.Action == actionAsk:
				c.askAboutResume(ctx, t, v)
			case v.Action == actionAbandon:
				c.notice(ctx, t, "⏹️ dispatch is back — leaving this task: "+reasonOr(v.Reason, "it is no longer worth continuing")+
					". "+capitalize(pickUpHint(t)))
			case t.Status == store.StatusIdle:
				text := "▶️ dispatch is back — reply in this thread to continue where the agent left off"
				if v.Reason != "" {
					text = "▶️ dispatch is back — " + v.Reason + "; reply in this thread to continue"
				}
				c.notice(ctx, t, text)
			case t.Status == store.StatusFailed:
				c.notice(ctx, t, "⏹️ dispatch is back — this task never got going and cannot be resumed; "+pickUpHint(t))
			}
		}
	}
	for _, r := range resume {
		c.autoResume(ctx, r.task, r.prompt)
	}
	return nil
}

// recoveryBudget bounds the time all of recovery may spend on decisions:
// four questions' worth, so a handful of tasks each get their answer and a
// wedged CLI cannot hold the bot offline.
func (c *Coordinator) recoveryBudget() time.Duration {
	per := c.DeciderTimeout
	if per <= 0 {
		per = 15 * time.Second
	}
	return 4 * per
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// resumable is a task recover() decided to pick up, with the turn it is to
// be given (empty = the configured resume prompt).
type resumable struct {
	task   store.TaskState
	prompt string
}

// autoResumable reports whether a task cut short by a restart may continue
// by itself: AutoResume is on, there is something to continue from (a
// session, or the prompt of a task that never started), it was touched
// recently enough, and it has not already been resumed too many times.
func (c *Coordinator) autoResumable(t store.TaskState) bool {
	if !c.AutoResume {
		return false
	}
	if t.Session == "" && strings.TrimSpace(t.Prompt) == "" {
		return false
	}
	max := c.MaxAutoResumes
	if max <= 0 {
		max = 3
	}
	if t.Resumes >= max {
		c.Log.Warn("task not auto-resumed: too many restarts", "task", t.ID, "resumes", t.Resumes)
		return false
	}
	within := c.AutoResumeWithin
	if within <= 0 {
		within = 12 * time.Hour
	}
	if !t.UpdatedAt.IsZero() && time.Since(t.UpdatedAt) > within {
		c.Log.Info("task not auto-resumed: too old", "task", t.ID, "age", time.Since(t.UpdatedAt).Round(time.Minute))
		return false
	}
	return true
}

// autoResume drives one recovered task without waiting for a message.
func (c *Coordinator) autoResume(ctx context.Context, t store.TaskState, decided string) {
	prompt, note := c.resumePrompt(), "▶️ dispatch is back — picking up this task where the agent left off"
	if decided != "" {
		prompt = decided
	}
	if t.Session == "" {
		// The task never reached a session: run the original request again.
		prompt, note = t.Prompt, "▶️ dispatch is back — this task never started, running it again"
	}
	if t.Definition.Environment.Workdir == "" && c.WorkdirRoot != "" {
		t.Definition.Environment.Workdir = filepath.Join(c.WorkdirRoot, string(t.ID))
	}
	c.emitTo(ctx, t.Transport, surface.Event{Kind: surface.EventReply, Thread: t.Thread, TaskID: t.ID, Task: &t, Text: note})
	c.bind(t.Thread, t.ID, c.surfaceOn(t.Transport))
	c.broadcast(ctx, surface.Event{Kind: surface.EventResumed, Thread: t.Thread, TaskID: t.ID, Task: &t})
	c.Log.Info("auto-resuming task", "task", t.ID, "thread", t.Thread, "session", t.Session)
	c.drives.Add(1)
	go c.drive(ctx, t, prompt, nil)
}

func (c *Coordinator) resumePrompt() string {
	if strings.TrimSpace(c.ResumePrompt) != "" {
		return c.ResumePrompt
	}
	return defaultResumePrompt
}

// surfaceOn names a surface bound to a transport (for task ownership).
func (c *Coordinator) surfaceOn(transportName string) string {
	for _, s := range c.Surfaces {
		if transportName == "" || s.Transport() == transportName {
			return s.Name()
		}
	}
	return ""
}

// handle places an inbound message (see place), logs it, relays it to
// the transports following the thread, then offers it to the surfaces on
// its transport and executes the intents of the first one that claims
// it. A decision is offered to every surface: the prompt it answers was
// rendered by one surface and may have been shown on any transport.
func (c *Coordinator) handle(ctx context.Context, in transport.Inbound) {
	openedOn, ok := c.place(ctx, &in)
	if !ok {
		return
	}
	c.append(ctx, "", in.Thread, "inbound", in)
	c.relay(ctx, in, openedOn)
	for _, s := range c.Surfaces {
		if s.Transport() != in.Transport && in.Decision == nil {
			continue
		}
		intents, ok := s.Handle(ctx, in)
		if !ok {
			continue
		}
		for _, it := range intents {
			c.execute(ctx, s, in, it)
		}
		return
	}
	c.Log.Debug("unclaimed inbound", "transport", in.Transport, "thread", in.Thread)
}

func (c *Coordinator) execute(ctx context.Context, s surface.Surface, in transport.Inbound, it surface.Intent) {
	// Addressing the bot lifts the transport's tombstone before the
	// coordinator sees the message — that is how reopening works. If the
	// intent did not actually reopen the thread (`close` again, `status`,
	// a wizard step), put the tombstone back so plain replies stay ignored.
	defer func() {
		if in.Thread != "" && c.threadClosed(in.Thread) {
			c.forget(c.taskTransport(ctx, s, in.Thread), in.Thread)
		}
	}()
	switch it := it.(type) {
	case surface.Say:
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: it.Text}, s)
	case surface.Decide:
		c.resolveDecision(ctx, it)
	case surface.RunTask:
		c.reopenThread(ctx, s, it.Thread)
		c.runTask(ctx, s, it)
	case surface.FollowUp:
		c.reopenThread(ctx, s, it.Thread)
		_, _ = c.followUp(ctx, s, it)
	case surface.Status:
		st, err := c.Store.LatestTaskForThread(ctx, it.Thread)
		if err != nil {
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no task on this thread"}, s)
			return
		}
		text := fmt.Sprintf("task `%s` — agent *%s* — status *%s* — session `%s`", st.ID, st.Definition.Name, st.Status, st.Session)
		if v, ok := c.lastVerdict(ctx, st); ok && v.By != "" {
			text += fmt.Sprintf("\n· last decision: *%s* by %s", v.Action, v.By)
			if v.Reason != "" {
				text += " — " + v.Reason
			}
		}
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, TaskID: st.ID, Task: &st, Text: text, Work: c.overview(ctx, it.Thread), Workflow: c.workflowOf(it.Thread)}, s)
	case surface.Cancel:
		if c.cancelWizard(it.Thread) {
			return
		}
		if c.stopWorkflow(ctx, it.Thread, "cancelled from the thread") {
			// The run's step, if a turn is in flight, goes too.
			if id, ok := c.lookup(it.Thread); ok {
				if err := c.Executor.Cancel(ctx, id); err != nil && !errors.Is(err, execlocal.ErrNotRunning) {
					c.Log.Warn("cancel: stopping the workflow's step", "task", id, "err", err)
				}
			}
			return
		}
		id, ok := c.lookup(it.Thread)
		if !ok {
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "nothing running on this thread"}, s)
			return
		}
		if err := c.Executor.Cancel(ctx, id); err != nil {
			c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, TaskID: id, Text: "cancel: " + err.Error()}, s)
		}
	case surface.CloseThread:
		c.closeThread(ctx, s, it)
	case surface.ReviewPR:
		// Both reopen the thread themselves, once they know they are
		// going to act: every other path out of them is a refusal, and a
		// refusal must leave a closed thread closed (see the defer above).
		c.reviewPR(ctx, s, it)
	case surface.MergePR:
		c.mergePR(ctx, s, it)
	case surface.RunWorkflow:
		c.startWorkflow(ctx, s, it)
	case surface.StopWorkflow:
		if !c.stopWorkflow(ctx, it.Thread, "stopped from the thread") {
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no workflow running on this thread"}, s)
		}
	case surface.PlanWorkflow:
		c.planWorkflow(ctx, s, it)
	case surface.SaveWorkflow:
		c.savePlan(ctx, s, it.Thread, it.Name)
	case surface.ListWorkflows:
		c.listWorkflows(ctx, s, it)
	case surface.ListAgents:
		defs, err := c.Store.ListDefinitions(ctx)
		if err != nil || len(defs) == 0 {
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no agent definitions"}, s)
			return
		}
		var b strings.Builder
		for _, d := range defs {
			fmt.Fprintf(&b, "• *%s* — %s", d.Name, describeDefinition(d))
			if d.Name == c.defaultAgent(ctx, s, it.Thread) {
				b.WriteString(" · _default here_")
			}
			b.WriteString("\n")
		}
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: strings.TrimRight(b.String(), "\n")}, s)
	case surface.ListCommands:
		c.listCommands(ctx, s, it)
	case surface.AddAgent:
		c.addAgent(ctx, s, it)
	case surface.EditAgent:
		c.editAgent(ctx, s, it)
	case surface.DeleteAgent:
		c.deleteAgent(ctx, s, it)
	case surface.SetDefault:
		c.setDefault(ctx, s, it)
	}
}

func describeDefinition(d agent.Definition) string {
	return fmt.Sprintf("%s/%s, env %s, mode %s", d.Kind, d.Model, d.Environment.Kind, d.PermissionMode)
}

// channelOf returns the channel part of a thread id ("C123/1.0" → "C123").
func channelOf(th transport.ThreadID) string {
	ch, _, _ := strings.Cut(string(th), "/")
	return ch
}

// channelKey names the channel a thread is in for ChannelAgents: the
// transport hosting the thread — the human may be writing from another
// one — and the channel part of the id.
func (c *Coordinator) channelKey(ctx context.Context, s surface.Surface, th transport.ThreadID) string {
	return c.taskTransport(ctx, s, th) + "/" + channelOf(th)
}

// taskTransport is the transport a task on th is recorded under: the
// one hosting the thread, else the surface's own.
func (c *Coordinator) taskTransport(ctx context.Context, s surface.Surface, th transport.ThreadID) string {
	if host := c.hostOf(ctx, th); host != "" {
		return host
	}
	return s.Transport()
}

// loadClosed reads the closed threads into memory once at startup; every
// later change goes through closeThread/reopenThread, which write both.
func (c *Coordinator) loadClosed(ctx context.Context) error {
	threads, err := c.Store.ClosedThreads(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	for _, th := range threads {
		c.closed[th] = true
	}
	n := len(c.closed)
	c.mu.Unlock()
	c.Log.Info("loaded closed threads", "threads", n)
	return nil
}

func (c *Coordinator) threadClosed(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed[th]
}

func (c *Coordinator) setClosed(th transport.ThreadID, closed bool) {
	c.mu.Lock()
	if closed {
		c.closed[th] = true
	} else {
		delete(c.closed, th)
	}
	c.mu.Unlock()
}

// closeThread ends the conversation on a thread: the open wizard and the
// running task are stopped, the thread is marked closed, and the transport
// is told to stop following it. The order matters — the notice and the
// reaction go out while the transport still follows the thread.
func (c *Coordinator) closeThread(ctx context.Context, s surface.Surface, it surface.CloseThread) {
	if c.threadClosed(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "this thread is already closed"}, s)
		return
	}
	c.cancelWizard(it.Thread)
	c.stopWorkflow(ctx, it.Thread, "the thread was closed")
	id, running := c.lookup(it.Thread)
	if running {
		if err := c.Executor.Cancel(ctx, id); err != nil && !errors.Is(err, execlocal.ErrNotRunning) {
			c.Log.Warn("close: cancel failed", "task", id, "err", err)
		}
		c.awaitStopped(it.Thread, id)
	}
	if err := c.Store.SetThreadClosed(ctx, it.Thread, true); err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: "close: " + err.Error()}, s)
		return
	}
	c.setClosed(it.Thread, true)
	c.append(ctx, id, it.Thread, "closed", map[string]any{"thread": it.Thread, "task": id})
	c.Log.Info("thread closed", "thread", it.Thread, "task", id, "was_running", running)
	c.emit(ctx, surface.Event{Kind: surface.EventClosed, Thread: it.Thread, TaskID: id}, s)
	host := c.taskTransport(ctx, s, it.Thread) // the thread's own transport, wherever `close` was typed
	c.mark(ctx, host, it.Thread, "")           // closed now: the ✅ wins over whatever the task is in
	c.forget(host, it.Thread)
}

// awaitStopped waits (briefly) for the cancelled task to let go of the
// thread. Without it a message that reopens the thread right away would be
// sent into the dying process, and the unbind the finishing task does last
// would drop the reopened conversation's own binding.
func (c *Coordinator) awaitStopped(th transport.ThreadID, id executor.TaskID) {
	deadline := time.Now().Add(closeDrainTimeout)
	for time.Now().Before(deadline) {
		if cur, ok := c.lookup(th); !ok || cur != id {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.Log.Warn("close: task did not stop in time, unbinding it", "task", id, "thread", th)
	c.unbind(th, id)
}

// closeDrainTimeout bounds how long close waits for a cancelled task.
const closeDrainTimeout = 3 * time.Second

// reopenThread lifts the closed mark when a human brings work back to the
// thread. Nothing happens on a thread that was never closed.
func (c *Coordinator) reopenThread(ctx context.Context, s surface.Surface, th transport.ThreadID) {
	if !c.threadClosed(th) {
		return
	}
	if err := c.Store.SetThreadClosed(ctx, th, false); err != nil {
		c.Log.Error("reopen thread", "thread", th, "err", err)
		return
	}
	c.setClosed(th, false)
	// Reopened from another transport (the web UI), the host transport
	// never saw the human address the bot there; lift its tombstone too.
	host := c.taskTransport(ctx, s, th)
	if tc, ok := c.transports[host].(transport.ThreadCloser); ok {
		tc.Follow(th)
	}
	// The ✅ comes off and the latest task says what waits there now, until
	// the message that reopened the thread starts a turn and marks it working.
	status := ""
	if st, err := c.Store.LatestTaskForThread(ctx, th); err == nil {
		status = st.Status
	}
	c.mark(ctx, host, th, status)
	c.Log.Info("thread reopened", "thread", th)
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "♻️ thread reopened"}, s)
}

// forget tells a thread-tracking transport to stop following a thread.
func (c *Coordinator) forget(transportName string, th transport.ThreadID) {
	if tc, ok := c.transports[transportName].(transport.ThreadCloser); ok {
		tc.Forget(th)
	}
}

// closedReaction marks a closed thread's root message: handled, nothing
// waits there any more.
const closedReaction = "white_check_mark"

// defaultAgent is the definition used on th when none is named: the
// channel's default if one is set, else DefaultDefinition.
func (c *Coordinator) defaultAgent(ctx context.Context, s surface.Surface, th transport.ThreadID) string {
	key := c.channelKey(ctx, s, th)
	c.mu.Lock()
	name, ok := c.ChannelAgents[key]
	c.mu.Unlock()
	if ok && name != "" {
		return name
	}
	return c.DefaultDefinition
}

// setDefault shows or changes the default agent of a channel.
func (c *Coordinator) setDefault(ctx context.Context, s surface.Surface, it surface.SetDefault) {
	key := c.channelKey(ctx, s, it.Thread)
	if it.Agent == "" {
		c.mu.Lock()
		name, ok := c.ChannelAgents[key]
		c.mu.Unlock()
		switch {
		case ok && name != "":
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("default agent on this channel: *%s* — change it with `default <agent>`", name)}, s)
		case c.DefaultDefinition != "":
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("no default set for this channel; the global default *%s* is used — set one with `default <agent>`", c.DefaultDefinition)}, s)
		default:
			c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no default agent — set one with `default <agent>`"}, s)
		}
		return
	}
	def, err := c.Store.GetDefinition(ctx, it.Agent)
	if err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: fmt.Sprintf("unknown agent %q — try `agents`", it.Agent)}, s)
		return
	}
	if c.SaveChannelAgent != nil {
		if err := c.SaveChannelAgent(ctx, c.taskTransport(ctx, s, it.Thread), channelOf(it.Thread), def.Name); err != nil {
			c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: "writing config: " + err.Error()}, s)
			return
		}
	}
	c.mu.Lock()
	if c.ChannelAgents == nil {
		c.ChannelAgents = map[string]string{}
	}
	c.ChannelAgents[key] = def.Name
	c.mu.Unlock()
	c.Log.Info("channel default set from chat", "channel", key, "agent", def.Name)
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: fmt.Sprintf("✅ default agent on this channel is now *%s* — plain messages here run it", def.Name)}, s)
}

func (c *Coordinator) runTask(ctx context.Context, s surface.Surface, it surface.RunTask) {
	c.runTaskModel(ctx, s, it, "")
}

// runTaskModel starts a task, with a model override for the one turn that
// asked for it — a workflow step's Model, which lands on the definition so
// the first (and only) turn of the task runs it.
func (c *Coordinator) runTaskModel(ctx context.Context, s surface.Surface, it surface.RunTask, model string) {
	if _, busy := c.lookup(it.Thread); busy {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "a task is already running on this thread — `cancel` it first or reply to it"}, s)
		return
	}
	if c.wizardOpen(it.Thread) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "finish or `cancel` the questions on this thread first"}, s)
		return
	}
	if it.Agent == "" && strings.TrimSpace(it.Prompt) == "" {
		// Bare `run`: ask for the agent and the prompt on the thread.
		c.dropFiles(ctx, s, it.Thread, it.Files)
		c.startPick(ctx, s, it.Thread, "", it.User)
		return
	}
	def, err := c.Store.GetDefinition(ctx, it.Agent)
	prompt := it.Prompt
	if fallback := c.defaultAgent(ctx, s, it.Thread); errors.Is(err, store.ErrNotFound) && fallback != "" {
		def, err = c.Store.GetDefinition(ctx, fallback)
		prompt = strings.TrimSpace(it.Agent + " " + it.Prompt)
	}
	if err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: fmt.Sprintf("unknown agent %q — try `agents`", it.Agent)}, s)
		return
	}
	if model != "" {
		def.Model = model
	}
	if strings.TrimSpace(prompt) == "" && len(it.Files) == 0 {
		// `run <agent>` without a prompt: ask for it. With attachments
		// the files are the prompt.
		c.startPick(ctx, s, it.Thread, def.Name, it.User)
		return
	}
	id := executor.TaskID(newID())
	if def.Environment.Kind == "" {
		def.Environment.Kind = environment.KindLocal
	}
	def.Environment = c.resolveEnv(def.Environment, def.Name, string(it.Thread), string(id))
	st := store.TaskState{ID: id, Transport: c.taskTransport(ctx, s, it.Thread), Thread: it.Thread, Definition: def, Requester: it.User, Asker: it.User, Status: store.StatusQueued}
	if err := c.Store.PutTask(ctx, st); err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, Text: "store: " + err.Error()}, s)
		return
	}
	c.bind(it.Thread, id, s.Name())
	c.broadcast(ctx, surface.Event{Kind: surface.EventStarted, Thread: it.Thread, TaskID: id, Task: &st})
	c.drives.Add(1)
	go c.drive(ctx, st, prompt, attachments(it.Files))
}

// attachments turns what a transport received into what an executor
// copies into the environment.
func attachments(files []transport.File) []agent.File {
	var out []agent.File
	for _, f := range files {
		out = append(out, agent.File{Name: f.Name, Data: f.Data})
	}
	return out
}

// dropFiles tells the thread that attachments sent with a message that
// opens a wizard (a bare `run`) went nowhere: the prompt comes later, as
// text, and files cannot be sent with it.
func (c *Coordinator) dropFiles(ctx context.Context, s surface.Surface, th transport.ThreadID, files []transport.File) {
	if len(files) == 0 {
		return
	}
	c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "📎 attachments are dropped here — send them together with the prompt"}, s)
}

// resolveEnv fills in the parts of a Spec that only the coordinator knows:
// which environment a task shares, and where its working directory goes.
//
// A reused environment has to keep the same workdir as well as the same
// container — the bind mount is fixed when the container is created — so the
// scope decides the directory name too.
func (c *Coordinator) resolveEnv(spec environment.Spec, agentName, thread, taskID string) environment.Spec {
	switch spec.Reuse {
	case environment.ReuseThread:
		spec.ReuseKey = thread
	case environment.ReuseDefinition:
		spec.ReuseKey = agentName
	default:
		spec.ReuseKey = ""
	}
	if spec.Workdir == "" && c.WorkdirRoot != "" {
		name := taskID
		if spec.ReuseKey != "" {
			name = dirName(spec.ReuseKey)
		}
		spec.Workdir = filepath.Join(c.WorkdirRoot, name)
	}
	return spec
}

// dirName turns a reuse key (a Slack "C123/1700.5" thread id, an agent name)
// into one path-safe directory name.
func dirName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "shared"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// followUp routes a plain message to the thread's task, resuming if needed.
// While a question is open on the thread, the text answers it instead.
//
// started says a turn is now running because of this message, and task is
// the task running it — which is what `merge` waits on (finish.go), by
// the task rather than by the thread, because a thread's previous turn
// can end between the two. An answered question, a thread with no
// resumable task and every reported failure are all false: nothing to
// wait for.
func (c *Coordinator) followUp(ctx context.Context, s surface.Surface, it surface.FollowUp) (task executor.TaskID, started bool) {
	c.mu.Lock()
	base, asking := c.askText[it.Thread]
	c.mu.Unlock()
	if asking {
		c.deliver(base, transport.Decision{PromptID: base, Choice: it.Text})
		return
	}
	id, ok := c.lookup(it.Thread)
	if ok {
		seq := int64(-1)
		undo := func() {}
		if sink := c.sink(id); sink != nil {
			// Before the send: the turn it starts can end at once, and its
			// closing line must address whoever wrote this.
			seq, undo = sink.setAsker(ctx, it.User)
		}
		if err := c.Executor.Send(ctx, id, it.Text, attachments(it.Files)); err == nil {
			c.wake(ctx, id, seq)
			return id, true
		} else if undo(); !errors.Is(err, execlocal.ErrNotRunning) {
			c.emit(ctx, surface.Event{Kind: surface.EventError, Thread: it.Thread, TaskID: id, Text: "send: " + err.Error()}, s)
			return
		}
	}
	st, err := c.Store.LatestTaskForThread(ctx, it.Thread)
	if def := c.defaultAgent(ctx, s, it.Thread); errors.Is(err, store.ErrNotFound) && def != "" {
		// A fresh thread with plain text: start a task with the channel's default agent.
		c.runTask(ctx, s, surface.RunTask{Thread: it.Thread, Agent: def, Prompt: it.Text, User: it.User, Files: it.Files})
		if id, ok := c.lookup(it.Thread); ok {
			return id, true
		}
		return "", false
	}
	if err != nil {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "no task on this thread — say `help`"}, s)
		return
	}
	if st.Session == "" || st.Status == store.StatusRunning {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: it.Thread, Text: "task cannot be resumed — start a new one with `run`"}, s)
		return
	}
	if st.Transport == "" {
		st.Transport = c.taskTransport(ctx, s, it.Thread)
	}
	if it.User != "" {
		st.Asker = it.User
	}
	c.bind(it.Thread, st.ID, s.Name())
	c.broadcast(ctx, surface.Event{Kind: surface.EventResumed, Thread: it.Thread, TaskID: st.ID, Task: &st})
	c.drives.Add(1)
	go c.drive(ctx, st, it.Text, attachments(it.Files))
	return st.ID, true
}

// drive runs one executor turn-loop for a task and records the outcome.
// files are the attachments that came with prompt; a restart's re-run has
// none (they were copied into the environment when the turn first ran).
func (c *Coordinator) drive(ctx context.Context, st store.TaskState, prompt string, files []agent.File) {
	defer c.drives.Done()
	st.Status = store.StatusRunning
	st.Prompt = prompt
	_ = c.Store.PutTask(ctx, st)
	// This process is running a turn for it now, so whatever the last one
	// left mid-flight is no longer what the log's last word is about.
	c.turnRan(st.ID)
	sink := &taskSink{c: c, state: st}
	c.mu.Lock()
	c.sinks[st.ID] = sink
	c.mu.Unlock()
	c.mark(ctx, st.Transport, st.Thread, store.StatusRunning)
	stopBeat := c.beat(ctx, sink)
	// The definition says which model to ask for; a human who sent the
	// agent "/model …" has said otherwise, and that choice would be lost
	// on this resume (the CLI keeps it only for its own process).
	def := st.Definition
	if st.ModelPin != "" {
		def.Model = st.ModelPin
	}
	err := c.Executor.Run(ctx, executor.Task{ID: st.ID, Definition: def, Prompt: prompt, Session: st.Session, Files: files}, sink)
	stopBeat()
	c.mu.Lock()
	delete(c.sinks, st.ID)
	c.mu.Unlock()
	final := sink.snapshot()
	// The run may have ended because ctx was cancelled (shutdown); persist
	// and notify with a context that still works.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	switch {
	case err != nil && errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// Shutdown. Only a turn that was still going is "interrupted" —
		// that is what recover() picks up again. A process that had
		// finished its turn and was only being kept alive for a follow-up
		// stays idle: nothing was cut short, so nothing has to continue.
		switch {
		case final.Session == "":
			final.Status = store.StatusFailed
		case sink.turnFinished():
			final.Status = store.StatusIdle
		default:
			final.Status = store.StatusInterrupted
		}
	case err != nil && errors.Is(err, context.Canceled):
		final.Status = store.StatusCancelled
	case err != nil:
		final.Status = store.StatusFailed
		c.broadcast(pctx, surface.Event{Kind: surface.EventError, Thread: st.Thread, TaskID: st.ID, Task: &final, Text: err.Error()})
	case final.Session != "":
		final.Status = store.StatusIdle
	default:
		final.Status = store.StatusDone
	}
	if err := c.Store.PutTask(pctx, final); err != nil {
		c.Log.Error("persist final task state", "task", st.ID, "err", err)
	}
	if cur, bound := c.lookup(st.Thread); !bound || cur == st.ID {
		// Not when close gave up waiting and the thread already runs another task.
		c.mark(pctx, st.Transport, st.Thread, final.Status)
	}
	// The last heartbeat takes down whatever liveness display a surface
	// still has up — on a shutdown there is no finished event to do it.
	c.broadcast(pctx, heartbeat(final))
	c.unbind(st.Thread, st.ID)
	if ctx.Err() == nil {
		c.broadcast(pctx, surface.Event{Kind: surface.EventFinished, Thread: st.Thread, TaskID: st.ID, Task: &final})
	}
	// A turn that never reached the agent at all (the environment would
	// not come up) emits no result for the sink to wake waiters on, and a
	// waiter would sit here until its own timeout. Done says the process
	// is gone, so there is certainly no turn left to wait for — which is
	// what settles a waiter whose turn produced no record to outrank its
	// floor.
	c.turnEnded(st.Thread, turnEnd{Task: st.ID, Seq: final.LastSeq, Done: true})
}

// heartbeat is the event that says where a task stands right now.
func heartbeat(st store.TaskState) surface.Event {
	return surface.Event{Kind: surface.EventHeartbeat, Thread: st.Thread, TaskID: st.ID, Task: &st}
}

// beat broadcasts a heartbeat every Heartbeat while the task is running,
// until the returned stop is called.
func (c *Coordinator) beat(ctx context.Context, sink *taskSink) (stop func()) {
	every := c.Heartbeat
	if every == 0 {
		every = defaultHeartbeat
	}
	if every < 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if st := sink.snapshot(); st.Status == store.StatusRunning {
					c.broadcast(ctx, heartbeat(st))
				}
			}
		}
	}()
	return func() { close(done) }
}

// defaultHeartbeat is the heartbeat period when Heartbeat is unset.
const defaultHeartbeat = 10 * time.Second

// wake records that a live task got a follow-up: it is running again
// before its first event says so, and surfaces hear it right away. seq is
// the log position before the follow-up went out: once the agent has spoken
// since — a quick turn can be over already — its word stands.
func (c *Coordinator) wake(ctx context.Context, id executor.TaskID, seq int64) {
	sink := c.sink(id)
	if sink == nil {
		return
	}
	st, changed := sink.setStatus(ctx, store.StatusIdle, store.StatusRunning, seq)
	if !changed {
		return
	}
	c.mark(ctx, st.Transport, st.Thread, st.Status)
	c.broadcast(ctx, heartbeat(st))
}

func (c *Coordinator) sink(id executor.TaskID) *taskSink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sinks[id]
}

// mark shows where a thread stands on its root message: ⏳ while the agent
// works, ✋ while it waits for a decision, 📬 once the turn is over and the
// thread waits for its next message, ❌ when the task failed, ✅ once the
// thread is closed. A task thread is never left bare: it is being worked
// on, waiting on a human, or closed. Best effort, on transports that can
// react; a mark is only touched when it changes, and the closed mark wins
// over whatever status the task is in.
func (c *Coordinator) mark(ctx context.Context, transportName string, th transport.ThreadID, status string) {
	r, ok := c.transports[transportName].(transport.Reactor)
	if !ok {
		return
	}
	// One swap at a time per thread: two transitions in flight at once (a
	// follow-up's wake against the turn's result) would interleave their
	// removes and adds and leave a reaction nobody remembers.
	mu := c.markLock(th)
	mu.Lock()
	defer mu.Unlock()
	c.mu.Lock()
	want := c.wantMark(th, status)
	have := c.marks[th]
	c.mu.Unlock()
	if have == want {
		return
	}
	// A stop's fallout arrives on a cancelled context; the mark still has
	// to reach the transport.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if have != "" {
		if err := r.Unreact(ctx, th, have); err != nil {
			c.Log.Warn("mark: removing reaction failed", "thread", th, "emoji", have, "err", err)
		}
	}
	if want != "" {
		if err := r.React(ctx, th, want); err != nil {
			c.Log.Warn("mark: reaction failed (needs reactions:write scope?)", "thread", th, "emoji", want, "err", err)
			want = "" // nothing is on; the next mark, even for the same state, tries again
		}
	}
	c.mu.Lock()
	c.marks[th] = want
	c.mu.Unlock()
}

// wantMark is the mark a thread should carry for a task status; the caller
// holds c.mu. Closed wins over any status.
func (c *Coordinator) wantMark(th transport.ThreadID, status string) string {
	if c.closed[th] {
		return closedReaction
	}
	return reactionFor(status)
}

func (c *Coordinator) markLock(th transport.ThreadID) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	mu, ok := c.markMu[th]
	if !ok {
		mu = &sync.Mutex{}
		c.markMu[th] = mu
	}
	return mu
}

// seedMarks records the mark each thread's root message is carrying from
// the previous process — its latest task's state, or closed — so the first
// change after a restart takes that reaction down instead of piling a new
// one next to it. Nothing is sent: reactions outlive the process on Slack.
// It runs before recover(), whose marks then swap the right one.
func (c *Coordinator) seedMarks(ctx context.Context) error {
	tasks, err := c.Store.ListTasks(ctx, "")
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for th, t := range latestByThread(tasks) {
		c.marks[th] = c.wantMark(th, t.Status)
	}
	for th := range c.closed {
		c.marks[th] = closedReaction // a thread closed before it ever had a task
	}
	c.Log.Info("seeded thread marks", "threads", len(c.marks))
	return nil
}

// reactionFor is the mark a task's status earns its thread. An interrupted
// task is still being worked on: the next start picks it up, or marks it
// idle and says so. Every other status past the live ones means the thread
// waits for a human — an idle or done task for its next message, a
// cancelled one for what to do instead.
func reactionFor(status string) string {
	switch status {
	case "":
		return ""
	case store.StatusQueued, store.StatusRunning, store.StatusInterrupted:
		return workingReaction
	case store.StatusWaitingPermission:
		return waitingReaction
	case store.StatusFailed:
		return failedReaction
	default:
		return answeredReaction
	}
}

const (
	workingReaction  = "hourglass_flowing_sand" // the agent is working
	waitingReaction  = "raised_hand"            // the agent waits for a decision
	answeredReaction = "mailbox_with_mail"      // the agent answered; the thread waits for its next message, or close
	failedReaction   = "x"                      // the task failed; the thread waits for what to do about it
)

// taskSink implements executor.Sink for one task.
type taskSink struct {
	c *Coordinator

	// putMu orders writes of the projection: it is taken before mu by
	// everything that changes state and held until the change is stored,
	// so a change made later cannot be written earlier. OnEvent runs on
	// the executor's goroutine, setStatus on whichever delivered the
	// follow-up or decision.
	putMu sync.Mutex
	mu    sync.Mutex
	state store.TaskState
	// answered is true while the agent's last word was a completed turn
	// that arrived before any shutdown. Events seen while the context is
	// already cancelled are the stop's own fallout (an agent that is being
	// drained still reports a result as it exits) and do not count, which
	// is what keeps a cut-short turn from looking finished.
	answered bool
}

func (s *taskSink) snapshot() store.TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// setStatus records a status change that did not come from the agent (a
// follow-up sent, a decision delivered) and persists it. It only applies
// when the task is still in the state from — the agent may already have
// moved on (a denied tool can end the turn at once), and its word wins —
// and, when seq is not negative, only while the agent has said nothing
// since that log position.
func (s *taskSink) setStatus(ctx context.Context, from, to string, seq int64) (st store.TaskState, changed bool) {
	s.putMu.Lock()
	defer s.putMu.Unlock()
	s.mu.Lock()
	if s.state.Status != from || (seq >= 0 && s.state.LastSeq != seq) {
		st = s.state
		s.mu.Unlock()
		return st, false
	}
	s.state.Status = to
	if to == store.StatusRunning {
		s.answered = false
	}
	st = s.state
	s.mu.Unlock()
	s.persist(ctx, st)
	return st, true
}

// persist writes the projection; the caller holds putMu.
func (s *taskSink) persist(ctx context.Context, st store.TaskState) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = s.c.Store.PutTask(pctx, st)
}

// turnFinished reports whether the agent completed its turn before the stop.
func (s *taskSink) turnFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answered
}

// setAsker records who wrote the message about to be sent to the running
// agent, so the turn that answers it addresses them. It returns the log
// position the state was at beforehand (what wake compares against) and
// an undo for a send that failed. An empty user — a message nobody typed —
// changes nothing.
//
// The writer becomes the asker at once, even while a turn is still going.
// Which turn answers a mid-turn message is the driver's business, not
// something the events say: Codex steers it into the turn in progress
// (turn/steer), whose closing line then answers them too. A queue of
// askers waiting for "their" turn would wait for one that never comes.
func (s *taskSink) setAsker(ctx context.Context, user string) (seq int64, undo func()) {
	s.putMu.Lock()
	defer s.putMu.Unlock()
	s.mu.Lock()
	seq, undo = s.state.LastSeq, func() {}
	prev := s.state.Asker
	if user == "" || prev == user {
		s.mu.Unlock()
		return seq, undo
	}
	s.state.Asker = user
	st := s.state
	s.mu.Unlock()
	s.persist(ctx, st)
	return seq, func() {
		s.putMu.Lock()
		defer s.putMu.Unlock()
		s.mu.Lock()
		if s.state.Asker != user {
			s.mu.Unlock()
			return
		}
		s.state.Asker = prev
		st := s.state
		s.mu.Unlock()
		s.persist(ctx, st)
	}
}

// setPin changes the task's ModelPin from outside the agent's event loop (a
// workflow step's model override) and returns what it was. It goes through
// putMu like every other state change, so a later OnEvent cannot persist a
// copy from before it.
func (s *taskSink) setPin(ctx context.Context, model string) string {
	s.putMu.Lock()
	defer s.putMu.Unlock()
	s.mu.Lock()
	prev := s.state.ModelPin
	s.state.ModelPin = model
	st := s.state
	s.mu.Unlock()
	s.persist(ctx, st)
	return prev
}

func (s *taskSink) OnEvent(ctx context.Context, id executor.TaskID, ev agent.Event) {
	seq := s.c.append(ctx, id, s.state.Thread, "agent", ev)
	s.putMu.Lock()
	s.mu.Lock()
	s.state.LastSeq = seq
	if ev.Session != "" {
		s.state.Session = ev.Session
	}
	if ev.Type == agent.EventInit && ev.Model != "" {
		s.state.Model = ev.Model
	}
	if ev.Type == agent.EventResult && ev.Model != "" {
		// The turn switched the session's model (a human sent the agent
		// its own "/model …"). The agent process would forget it; the
		// resume after the next idle timeout asks for it again.
		s.state.ModelPin = ev.Model
	}
	switch ev.Type {
	case agent.EventNeedsPermission, agent.EventQuestion:
		s.state.Status = store.StatusWaitingPermission
		s.answered = false
	case agent.EventResult, agent.EventError:
		s.state.Status = store.StatusIdle
		s.answered = ctx.Err() == nil // false while draining a stop
		if s.answered {
			s.state.Resumes = 0 // got through a turn; not a restart loop
		}
	case agent.EventUsage:
		// Follows a result; the turn is over and stays over.
	default:
		s.state.Status = store.StatusRunning
		s.answered = false
	}
	st := s.state
	s.mu.Unlock()
	s.persist(ctx, st)
	s.putMu.Unlock()
	s.c.mark(ctx, st.Transport, st.Thread, st.Status)
	if ev.Type == agent.EventNeedsPermission {
		return // rendered by AwaitDecision with its prompt id
	}
	e := ev
	out := surface.Event{Kind: surface.EventAgent, Thread: st.Thread, TaskID: id, Task: &st, Agent: &e}
	if ev.Type == agent.EventResult || ev.Type == agent.EventError {
		// The end of a turn is when a human decides whether to go and
		// look, so the closing line carries what there is to look at.
		// A turn that failed ends here too, and the half-finished pull
		// request it left behind is the thing someone has to deal with.
		out.Work = s.c.overview(ctx, st.Thread)
		// `merge` waits here: it asked the agent to make the branch
		// mergeable and must not read the log until that turn is over.
		//
		// The placement is a balance, and both sides of it are real. The
		// status above already reads as idle, so mergePR's turnRunning
		// goes false from there — and a `merge` accepted between the two
		// registers its waiter in time to be woken by *this* turn, which
		// on the warm path is the same task and so cannot be told apart
		// by id. Announcing before the mark and the overview keeps the
		// reaction call and a whole-log scan out of that gap. Announcing
		// before the broadcast would put it ahead of the turn's own
		// closing line, and a thread that reads "🔒 thread closed" above
		// "✅ done" has been told the story backwards. So: after the
		// scan, before the send.
		s.c.turnEnded(st.Thread, turnEnd{Task: id, Seq: seq})
	}
	s.c.broadcast(ctx, out)
}

func (s *taskSink) AwaitDecision(ctx context.Context, id executor.TaskID, ev agent.Event) (agent.PermissionDecision, error) {
	if ev.Type == agent.EventQuestion {
		return s.awaitAnswers(ctx, id, ev)
	}
	st0 := s.snapshot()
	if v, ok := s.c.decidePermission(ctx, st0, ev); ok {
		s.c.noteAutoAllowed(ctx, st0, ev, v)
		return agent.PermissionDecision{ToolID: ev.ToolID, Allow: true, Reason: "decider: " + v.Reason}, nil
	}
	base := string(id) + ":" + ev.ToolID
	ch := make(chan transport.Decision, 1)
	s.c.mu.Lock()
	s.c.pending[base] = ch
	s.c.mu.Unlock()
	defer func() {
		s.c.mu.Lock()
		delete(s.c.pending, base)
		s.c.mu.Unlock()
	}()
	st := s.snapshot()
	e := ev
	s.c.broadcast(ctx, surface.Event{Kind: surface.EventPermission, Thread: st.Thread, TaskID: id, Task: &st, Agent: &e, PromptID: base})

	select {
	case d := <-ch:
		s.c.append(ctx, id, st.Thread, "decision", d)
		s.resume(ctx)
		return agent.PermissionDecision{ToolID: ev.ToolID, Allow: d.Choice == "allow", Reason: "operator chose " + d.Choice}, nil
	case <-ctx.Done():
		return agent.PermissionDecision{}, ctx.Err()
	}
}

// resume records that the human answered and the agent is working again,
// and tells surfaces so their liveness display comes back.
func (s *taskSink) resume(ctx context.Context) {
	st, changed := s.setStatus(ctx, store.StatusWaitingPermission, store.StatusRunning, -1)
	if !changed {
		return
	}
	s.c.mark(ctx, st.Transport, st.Thread, st.Status)
	s.c.broadcast(ctx, heartbeat(st))
}

// awaitAnswers asks each question in turn and returns the answers as an
// allow decision. A button click (option value) or a typed reply answers.
func (s *taskSink) awaitAnswers(ctx context.Context, id executor.TaskID, ev agent.Event) (agent.PermissionDecision, error) {
	st := s.snapshot()
	answers := map[string]string{}
	for i := range ev.Questions {
		q := ev.Questions[i]
		base := fmt.Sprintf("%s:%s#%d", id, ev.ToolID, i)
		ch := make(chan transport.Decision, 1)
		s.c.mu.Lock()
		s.c.pending[base] = ch
		s.c.askText[st.Thread] = base
		s.c.mu.Unlock()

		e := ev
		s.c.broadcast(ctx, surface.Event{Kind: surface.EventQuestion, Thread: st.Thread, TaskID: id, Task: &st, Agent: &e, Question: &q, PromptID: base})

		var d transport.Decision
		select {
		case d = <-ch:
		case <-ctx.Done():
			s.c.clearAsk(st.Thread, base)
			return agent.PermissionDecision{}, ctx.Err()
		}
		s.c.clearAsk(st.Thread, base)
		s.c.append(ctx, id, st.Thread, "decision", d)
		answers[q.Text] = d.Choice
	}
	s.resume(ctx)
	return agent.PermissionDecision{ToolID: ev.ToolID, Allow: true, Answers: answers}, nil
}

func (c *Coordinator) clearAsk(th transport.ThreadID, base string) {
	c.mu.Lock()
	delete(c.pending, base)
	if c.askText[th] == base {
		delete(c.askText, th)
	}
	c.mu.Unlock()
}

// resolveDecision delivers a Decide intent. Prompt ids are
// "<surface>:<task>:<tool>"; the surface prefix is stripped so any surface
// that rendered the prompt can answer it.
func (c *Coordinator) resolveDecision(ctx context.Context, d surface.Decide) {
	base := d.PromptID
	if i := strings.Index(base, ":"); i >= 0 {
		base = base[i+1:]
	}
	c.deliver(base, transport.Decision{PromptID: d.PromptID, Choice: d.Choice})
}

// deliver hands a decision to the waiter registered under base.
func (c *Coordinator) deliver(base string, d transport.Decision) {
	c.mu.Lock()
	ch, ok := c.pending[base]
	c.mu.Unlock()
	if !ok {
		c.Log.Warn("decision for unknown prompt", "prompt", d.PromptID)
		return
	}
	select {
	case ch <- d:
	default:
	}
}

// emit renders an event through one surface and sends the result to the
// surface's transport and to every observer (a transport following every
// thread). Render and send happen under the thread's lock: a surface that
// edits a keyed message in place relies on its messages reaching the
// transport in the order it rendered them, and a heartbeat and an agent
// event can arrive from different goroutines.
func (c *Coordinator) emit(ctx context.Context, ev surface.Event, s surface.Surface) {
	mu := c.threadLock(ev.Thread)
	mu.Lock()
	defer mu.Unlock()
	t := c.transports[s.Transport()]
	observers := c.observersBesides(s.Transport())
	for _, out := range s.Render(ev) {
		if ev.Kind != surface.EventHeartbeat {
			c.append(ctx, ev.TaskID, out.Thread, "outbound", out)
		}
		if err := t.Send(ctx, out); err != nil {
			c.Log.Error("send failed", "transport", t.Name(), "surface", s.Name(), "err", err)
		}
		if out.Thread != ev.Thread {
			continue // a surface's own place (the feed channel), not the conversation
		}
		for _, o := range observers {
			if err := o.Send(ctx, out); err != nil {
				c.Log.Error("send failed", "transport", o.Name(), "surface", s.Name(), "err", err)
			}
		}
	}
}

// threadLock returns the lock that orders output on th.
func (c *Coordinator) threadLock(th transport.ThreadID) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	mu, ok := c.outMu[th]
	if !ok {
		mu = &sync.Mutex{}
		c.outMu[th] = mu
	}
	return mu
}

// emitTo renders an event through every surface bound to a transport
// (all surfaces when the transport is unknown).
func (c *Coordinator) emitTo(ctx context.Context, transportName string, ev surface.Event) {
	for _, s := range c.Surfaces {
		if transportName == "" || s.Transport() == transportName {
			c.emit(ctx, ev, s)
		}
	}
}

// broadcast renders an event through every surface.
func (c *Coordinator) broadcast(ctx context.Context, ev surface.Event) {
	for _, s := range c.Surfaces {
		c.emit(ctx, ev, s)
	}
}

// notice tells t's thread that a restart left the task for its asker
// to act on. Every notice carries its task, so surfaces can address them.
func (c *Coordinator) notice(ctx context.Context, t store.TaskState, text string) {
	c.emitTo(ctx, t.Transport, surface.Event{Kind: surface.EventNotice, Thread: t.Thread, TaskID: t.ID, Task: &t, Text: text})
}

// append writes a record to the log. It must not be lost during shutdown,
// so it ignores cancellation of ctx.
func (c *Coordinator) append(ctx context.Context, id executor.TaskID, th transport.ThreadID, kind string, v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	seq, err := c.Store.Append(ctx, store.Record{At: time.Now(), Task: id, Thread: th, Kind: kind, Payload: b})
	if err != nil {
		c.Log.Error("append failed", "err", err)
	}
	return seq
}

func (c *Coordinator) bind(th transport.ThreadID, id executor.TaskID, surfaceName string) {
	c.mu.Lock()
	c.threads[th] = id
	c.owner[id] = surfaceName
	c.mu.Unlock()
}

func (c *Coordinator) unbind(th transport.ThreadID, id executor.TaskID) {
	c.mu.Lock()
	if c.threads[th] == id {
		delete(c.threads, th)
	}
	delete(c.owner, id)
	c.mu.Unlock()
}

func (c *Coordinator) wizardOpen(th transport.ThreadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.wizards[th]
	return ok
}

func (c *Coordinator) lookup(th transport.ThreadID) (executor.TaskID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.threads[th]
	return id, ok
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
