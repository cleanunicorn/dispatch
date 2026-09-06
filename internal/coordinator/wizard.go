package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// The "agent add", "agent edit" and "agent delete" flows ask for one
// setting at a time on the thread that requested them, reusing the question
// machinery agents use: every step is an EventQuestion with a prompt id,
// answered by a button or a typed reply.

// wizardTimeout is how long one question waits for an answer.
const wizardTimeout = 15 * time.Minute

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var errWizardCancelled = errors.New("cancelled")

// toolPresets are the button choices of the tools step. The tools are
// written in the canonical vocabulary (agent.Tool*), because that is what
// allowed_tools is matched against whatever agent ends up running the
// definition; Glob and Grep have no canonical name and keep the vendor's.
var toolPresets = []struct {
	label, desc string
	tools       []string
}{
	{"Read-only", "Read, Glob, Grep", []string{agent.ToolRead, "Glob", "Grep"}},
	{"Edit files", "Read, Glob, Grep, Edit, Write", []string{agent.ToolRead, "Glob", "Grep", agent.ToolEdit, agent.ToolWrite}},
	{"Edit + git", "…plus Bash(git:*)", []string{agent.ToolRead, "Glob", "Grep", agent.ToolEdit, agent.ToolWrite, agent.ToolBash + "(git:*)"}},
	{"None", "ask for every tool", nil},
}

// threadFree reports whether a flow of the given kind may start on th,
// telling the thread why not otherwise.
func (c *Coordinator) threadFree(ctx context.Context, s surface.Surface, th transport.ThreadID, kind string) bool {
	if _, busy := c.lookup(th); busy {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: fmt.Sprintf("a task is running on this thread — start `%s` in a new thread", flowLabel(kind))}, s)
		return false
	}
	if c.wizardOpen(th) {
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: th, Text: "questions are already open on this thread — answer the one above or `cancel`"}, s)
		return false
	}
	return true
}

// addAgent starts the flow on a thread.
func (c *Coordinator) addAgent(ctx context.Context, s surface.Surface, it surface.AddAgent) {
	if !c.threadFree(ctx, s, it.Thread, flowAddAgent) {
		return
	}
	c.startWizard(ctx, s, it.Thread, nil)
}

// editAgent changes one definition field at a time until the user saves.
// Tasks already running keep the settings they started with.
func (c *Coordinator) editAgent(ctx context.Context, s surface.Surface, it surface.EditAgent) {
	if !c.threadFree(ctx, s, it.Thread, flowEditAgent) {
		return
	}
	c.startFlow(ctx, s, it.Thread, flowEditAgent, nil, func(ctx context.Context, w *wizard) (string, error) {
		def, err := w.pickAgent(ctx, it.Agent, "Edit", "Which agent do you want to edit?")
		if err != nil {
			return "", err
		}
		changed, err := w.edit(ctx, &def)
		if err != nil {
			return "", err
		}
		if !changed {
			return fmt.Sprintf("nothing changed on *%s*", def.Name), nil
		}
		if w.c.UpdateDefinition != nil {
			if err := w.c.UpdateDefinition(ctx, def); err != nil {
				return "", fmt.Errorf("writing config: %w", err)
			}
		}
		if err := w.c.Store.PutDefinition(ctx, def); err != nil {
			return "", fmt.Errorf("store: %w", err)
		}
		w.c.Log.Info("definition updated from chat", "name", def.Name, "thread", w.thread)
		return fmt.Sprintf("✅ agent *%s* updated — new tasks use the new settings; running ones keep the old", def.Name), nil
	})
}

// deleteAgent removes a definition after a confirmation. A definition that
// is the global or a channel default is refused: point the default elsewhere first.
func (c *Coordinator) deleteAgent(ctx context.Context, s surface.Surface, it surface.DeleteAgent) {
	if !c.threadFree(ctx, s, it.Thread, flowDeleteAgent) {
		return
	}
	c.startFlow(ctx, s, it.Thread, flowDeleteAgent, nil, func(ctx context.Context, w *wizard) (string, error) {
		def, err := w.pickAgent(ctx, it.Agent, "Delete", "Which agent do you want to delete?")
		if err != nil {
			return "", err
		}
		if why := w.c.inUse(def.Name); why != "" {
			return "", errors.New(why)
		}
		ok, err := w.confirm(ctx, "Delete?", fmt.Sprintf("Delete agent *%s* (%s)? It is removed from the config; running tasks are not affected.", def.Name, describeDefinition(def)), "Delete", "remove it")
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errWizardCancelled
		}
		if w.c.DeleteDefinition != nil {
			if err := w.c.DeleteDefinition(ctx, def.Name); err != nil {
				return "", fmt.Errorf("writing config: %w", err)
			}
		}
		if err := w.c.Store.DeleteDefinition(ctx, def.Name); err != nil {
			return "", fmt.Errorf("store: %w", err)
		}
		w.c.Log.Info("definition deleted from chat", "name", def.Name, "thread", w.thread)
		return fmt.Sprintf("🗑️ agent *%s* deleted", def.Name), nil
	})
}

// inUse explains why name cannot be deleted, or returns "".
func (c *Coordinator) inUse(name string) string {
	if name == c.DefaultDefinition {
		return fmt.Sprintf("*%s* is the global default agent (`default_agent` in config.toml) — change that first", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var channels []string
	for key, a := range c.ChannelAgents {
		if a == name {
			channels = append(channels, key)
		}
	}
	if len(channels) > 0 {
		sort.Strings(channels)
		return fmt.Sprintf("*%s* is the default agent on %s — say `default <agent>` there first", name, strings.Join(channels, ", "))
	}
	return ""
}

// startPick runs the `run` picker on a thread: choose the agent (unless
// given), then type the prompt, then start the task on behalf of user.
func (c *Coordinator) startPick(ctx context.Context, s surface.Surface, th transport.ThreadID, agentName, user string) {
	c.startFlow(ctx, s, th, flowRun, nil, func(ctx context.Context, w *wizard) (string, error) {
		def, err := w.pickAgent(ctx, agentName, "Run", "Which agent?")
		if err != nil {
			return "", err
		}
		name := def.Name
		prompt, err := w.askUntil(ctx, agent.Question{Header: "Prompt", Text: fmt.Sprintf("What should *%s* do? Reply in this thread.", name)}, nonEmpty("prompt"))
		if err != nil {
			return "", err
		}
		w.then = func(ctx context.Context) {
			w.c.runTask(ctx, w.s, surface.RunTask{Thread: th, Agent: name, Prompt: prompt, User: user})
		}
		return "", nil
	})
}

// resumeFlows restarts flows that were open before a restart: the saved
// answers are replayed silently and the next question is asked again.
func (c *Coordinator) resumeFlows(ctx context.Context) {
	flows, err := c.Store.ListFlows(ctx)
	if err != nil {
		c.Log.Error("list flows", "err", err)
		return
	}
	for _, f := range flows {
		if f.Kind == "add_agent" { // saved by versions before the `agent` namespace
			f.Kind = flowAddAgent
		}
		var s surface.Surface
		for _, cand := range c.Surfaces {
			if cand.Name() == f.Surface {
				s = cand
			}
		}
		if s == nil || f.Kind != flowAddAgent {
			c.Log.Warn("dropping flow without a surface", "thread", f.Thread, "surface", f.Surface, "kind", f.Kind)
			_ = c.Store.DeleteFlow(ctx, f.Thread)
			continue
		}
		c.Log.Info("resuming flow", "thread", f.Thread, "answers", len(f.Answers))
		c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: f.Thread, Text: "▶️ dispatch is back — continuing with the agent questions"}, s)
		c.startWizard(ctx, s, f.Thread, f.Answers)
	}
}

// Flow kinds. Only agent_add is persisted and resumed after a restart; the
// others are short-lived and simply asked again.
const (
	flowAddAgent    = "agent_add"
	flowEditAgent   = "agent_edit"
	flowDeleteAgent = "agent_delete"
	flowRun         = "run"
)

// flowLabel names a flow in messages.
func flowLabel(kind string) string {
	return strings.ReplaceAll(kind, "_", " ")
}

// startWizard resumes an add-agent flow with saved answers.
func (c *Coordinator) startWizard(ctx context.Context, s surface.Surface, th transport.ThreadID, answers []string) {
	c.startFlow(ctx, s, th, flowAddAgent, answers, func(ctx context.Context, w *wizard) (string, error) {
		def, err := w.run(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("✅ agent *%s* saved — try `run %s <prompt>`", def.Name, def.Name), nil
	})
}

// startFlow runs a question flow in its own goroutine; one per thread.
// answers are replayed before any new question is asked. run returns the
// success message (empty posts nothing); w.then, if set by run, executes
// after the flow is unregistered from the thread.
func (c *Coordinator) startFlow(ctx context.Context, s surface.Surface, th transport.ThreadID, kind string, answers []string, run func(context.Context, *wizard) (string, error)) {
	wctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.wizards[th] = cancel
	c.mu.Unlock()

	w := &wizard{c: c, s: s, thread: th, kind: kind, id: newID(), parent: ctx, answers: answers}
	c.drives.Add(1)
	go func() {
		defer c.drives.Done()
		done, err := run(wctx, w)
		cancel()
		c.mu.Lock()
		delete(c.wizards, th)
		c.mu.Unlock()
		// Report with a context that survives shutdown.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rcancel()
		label := flowLabel(kind)
		if ctx.Err() != nil {
			if kind == flowAddAgent {
				// Shutdown: keep the saved answers; resumeFlows continues after restart.
				w.say(rctx, "⏸️ dispatch is restarting — the agent questions continue when it is back")
			} else {
				w.say(rctx, fmt.Sprintf("⏸️ dispatch is restarting — say `%s` again when it is back", label))
			}
			return
		}
		if kind == flowAddAgent {
			_ = c.Store.DeleteFlow(rctx, th)
		}
		switch {
		case errors.Is(err, errWizardCancelled):
			w.say(rctx, fmt.Sprintf("⏹️ %s cancelled", label))
		case errors.Is(err, context.DeadlineExceeded):
			w.say(rctx, fmt.Sprintf("⌛ no answer — %s abandoned; say `%s` to start over", label, label))
		case err != nil:
			w.say(rctx, fmt.Sprintf("❌ %s: %s", label, err.Error()))
		default:
			if done != "" {
				w.say(rctx, done)
			}
			if w.then != nil {
				w.then(ctx)
			}
		}
	}()
}

// cancelWizard stops an open flow on th. Returns false if none.
func (c *Coordinator) cancelWizard(th transport.ThreadID) bool {
	c.mu.Lock()
	cancel, ok := c.wizards[th]
	c.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

type wizard struct {
	c      *Coordinator
	s      surface.Surface
	thread transport.ThreadID
	kind   string
	id     string
	parent context.Context // the coordinator's context; ends only on shutdown
	step   int
	then   func(context.Context) // runs after a successful flow, once the thread is free

	answers []string // every answer so far; persisted after each one
	next    int      // answers[next:] are still to be replayed
}

func (w *wizard) say(ctx context.Context, text string) {
	w.c.emit(ctx, surface.Event{Kind: surface.EventReply, Thread: w.thread, Text: text}, w.s)
}

// ask posts one question and waits for its answer (button value or typed
// text). Questions are rendered only on the surface that started the flow.
// While saved answers remain they are returned instead of asking.
func (w *wizard) ask(ctx context.Context, q agent.Question) (string, error) {
	w.step++
	if w.next < len(w.answers) {
		a := w.answers[w.next]
		w.next++
		return a, nil
	}
	base := fmt.Sprintf("%s-%s#%d", w.kind, w.id, w.step)
	ch := make(chan transport.Decision, 1)
	w.c.mu.Lock()
	w.c.pending[base] = ch
	w.c.askText[w.thread] = base
	w.c.mu.Unlock()
	defer w.c.clearAsk(w.thread, base)

	w.c.emit(ctx, surface.Event{Kind: surface.EventQuestion, Thread: w.thread, Question: &q, PromptID: base}, w.s)

	select {
	case d := <-ch:
		w.c.append(ctx, "", w.thread, "decision", d)
		a := unquote(d.Choice)
		w.answers = append(w.answers, a)
		w.next = len(w.answers)
		if w.kind == flowAddAgent {
			if err := w.c.Store.PutFlow(ctx, store.FlowState{Thread: w.thread, Transport: w.s.Transport(), Surface: w.s.Name(), Kind: w.kind, Answers: w.answers}); err != nil {
				w.c.Log.Error("persist flow", "thread", w.thread, "err", err)
			}
		}
		return a, nil
	case <-time.After(wizardTimeout):
		return "", context.DeadlineExceeded
	case <-ctx.Done():
		if w.parent.Err() != nil {
			return "", ctx.Err() // shutdown
		}
		return "", errWizardCancelled
	}
}

// askUntil repeats a question until validate accepts the answer.
func (w *wizard) askUntil(ctx context.Context, q agent.Question, validate func(string) (string, error)) (string, error) {
	orig := q.Text
	for {
		a, err := w.ask(ctx, q)
		if err != nil {
			return "", err
		}
		v, verr := validate(a)
		if verr == nil {
			return v, nil
		}
		q.Text = "⚠️ " + verr.Error() + "\n" + orig
	}
}

// unquote strips the backticks or quotes people wrap answers in. Slack in
// particular refuses to send a message that starts with "/" (it looks like
// a slash command), so paths arrive as `/home/me/app`.
func unquote(a string) string {
	a = strings.TrimSpace(a)
	for _, q := range []string{"`", "\"", "'"} {
		if len(a) >= 2 && strings.HasPrefix(a, q) && strings.HasSuffix(a, q) {
			return strings.TrimSpace(a[1 : len(a)-1])
		}
	}
	return a
}

// pathHint tells Slack users how to send a path.
const pathHint = " Wrap it in backticks (Slack drops messages that start with `/`)."

func options(pairs ...string) []agent.Option {
	var out []agent.Option
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, agent.Option{Label: pairs[i], Description: pairs[i+1]})
	}
	return out
}

func isNone(a string) bool {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "", "none", "no", "skip", "-":
		return true
	}
	return false
}

// pickAgent resolves name to a stored definition, asking with a list of
// all definitions when name is empty.
func (w *wizard) pickAgent(ctx context.Context, name, header, text string) (agent.Definition, error) {
	if name != "" {
		def, err := w.c.Store.GetDefinition(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return def, fmt.Errorf("unknown agent %q — try `agents`", name)
		}
		return def, err
	}
	defs, err := w.c.Store.ListDefinitions(ctx)
	if err != nil {
		return agent.Definition{}, err
	}
	if len(defs) == 0 {
		return agent.Definition{}, errors.New("no agent definitions — say `agent add` first")
	}
	q := agent.Question{Header: header, Text: text}
	def := w.c.defaultAgent(ctx, w.s, w.thread)
	for _, d := range defs {
		desc := describeDefinition(d)
		if d.Name == def {
			desc = "default here · " + desc
		}
		q.Options = append(q.Options, agent.Option{Label: d.Name, Description: desc})
	}
	var picked agent.Definition
	_, err = w.askUntil(ctx, q, func(a string) (string, error) {
		for _, d := range defs {
			if strings.EqualFold(a, d.Name) {
				picked = d
				return d.Name, nil
			}
		}
		return "", fmt.Errorf("no agent named %q", a)
	})
	return picked, err
}

// confirm asks a yes/no question; yes is the label of the affirmative button.
func (w *wizard) confirm(ctx context.Context, header, text, yes, yesDesc string) (bool, error) {
	a, err := w.askUntil(ctx, agent.Question{Header: header, Text: text, Options: options(yes, yesDesc, "Cancel", "keep everything as it is")},
		func(a string) (string, error) {
			switch strings.ToLower(a) {
			case strings.ToLower(yes), "yes", "y", "ok":
				return "yes", nil
			case "cancel", "no", "n", "discard", "keep":
				return "no", nil
			}
			return "", fmt.Errorf("answer %s or Cancel", yes)
		})
	return a == "yes", err
}

// run drives the add-agent questions and saves the result.
func (w *wizard) run(ctx context.Context) (agent.Definition, error) {
	def := agent.Definition{Kind: w.defaultKind()}
	var err error

	def.Name, err = w.askUntil(ctx, agent.Question{Header: "New agent", Text: "Name for the new agent? (letters, digits, `.`, `_`, `-`)"}, func(a string) (string, error) {
		if !nameRE.MatchString(a) {
			return "", fmt.Errorf("%q is not a valid name", a)
		}
		if _, err := w.c.Store.GetDefinition(ctx, a); err == nil {
			return "", fmt.Errorf("an agent named *%s* already exists", a)
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		return a, nil
	})
	if err != nil {
		return def, err
	}
	if err := w.askKind(ctx, &def); err != nil {
		return def, err
	}
	for _, f := range editFields {
		if err := f.ask(w, ctx, &def); err != nil {
			return def, err
		}
	}

	ok, err := w.confirm(ctx, "Save?", summarize("Save this agent?", def), "Save", "write it to the config and enable it now")
	if err != nil {
		return def, err
	}
	if !ok {
		return def, errWizardCancelled
	}

	if w.c.SaveDefinition != nil {
		if err := w.c.SaveDefinition(ctx, def); err != nil {
			return def, fmt.Errorf("writing config: %w", err)
		}
	}
	if err := w.c.Store.PutDefinition(ctx, def); err != nil {
		return def, fmt.Errorf("store: %w", err)
	}
	w.c.Log.Info("definition added from chat", "name", def.Name, "thread", w.thread)
	return def, nil
}

// editFields are the choices of the edit menu, in order.
var editFields = []struct {
	label string
	ask   func(*wizard, context.Context, *agent.Definition) error
	show  func(agent.Definition) string
}{
	{"Model", (*wizard).askModel, func(d agent.Definition) string { return d.Model }},
	{"Environment", (*wizard).askEnvironment, describeEnvironment},
	{"Permissions", (*wizard).askPermissions, func(d agent.Definition) string { return string(d.PermissionMode) }},
	{"Tools", (*wizard).askTools, func(d agent.Definition) string {
		if len(d.AllowedTools) == 0 {
			return "none"
		}
		return strings.Join(d.AllowedTools, ", ")
	}},
	{"System prompt", (*wizard).askSystemPrompt, func(d agent.Definition) string {
		if d.SystemPrompt == "" {
			return "none"
		}
		return truncate(d.SystemPrompt, 60)
	}},
}

// edit shows the definition and a menu of fields; each pick re-asks that
// field's question(s), until Save (or Cancel, which returns
// errWizardCancelled). def is updated in place; nothing is persisted here.
// changed is false when Save was picked without touching a field.
func (w *wizard) edit(ctx context.Context, def *agent.Definition) (changed bool, err error) {
	for {
		q := agent.Question{Header: "Edit " + def.Name, Text: summarize("What do you want to change?", *def)}
		for _, f := range editFields {
			q.Options = append(q.Options, agent.Option{Label: f.label, Description: truncate(f.show(*def), 70)})
		}
		q.Options = append(q.Options,
			agent.Option{Label: "Save", Description: "write the changes to the config and use them from now on"},
			agent.Option{Label: "Cancel", Description: "discard the changes"})
		pick, err := w.askUntil(ctx, q, func(a string) (string, error) {
			for _, o := range q.Options {
				if strings.EqualFold(a, o.Label) {
					return o.Label, nil
				}
			}
			return "", fmt.Errorf("pick one of the listed fields, Save or Cancel")
		})
		if err != nil {
			return false, err
		}
		switch pick {
		case "Save":
			return changed, nil
		case "Cancel":
			return false, errWizardCancelled
		}
		for _, f := range editFields {
			if f.label == pick {
				if err := f.ask(w, ctx, def); err != nil {
					return false, err
				}
				changed = true
			}
		}
	}
}

// kindChoices describes each agent kind on the wizard's buttons.
var kindChoices = map[agent.Kind]string{
	agent.KindClaude:   "Claude Code",
	agent.KindCodex:    "OpenAI Codex",
	agent.KindOpenCode: "OpenCode — any provider (GLM, DeepSeek, Kimi, …)",
}

// defaultKind is the agent a new definition gets unless asked: the first
// registered driver, claude when the coordinator was not told any.
func (w *wizard) defaultKind() agent.Kind {
	if len(w.c.AgentKinds) > 0 {
		return w.c.AgentKinds[0]
	}
	return agent.KindClaude
}

// askKind asks which agent runs the definition, when this build has more
// than one driver; with a single one there is nothing to choose.
func (w *wizard) askKind(ctx context.Context, def *agent.Definition) error {
	kinds := w.c.AgentKinds
	if len(kinds) < 2 {
		return nil
	}
	var opts []string
	for _, k := range kinds {
		desc := kindChoices[k]
		if desc == "" {
			desc = string(k)
		}
		opts = append(opts, string(k), desc)
	}
	a, err := w.askUntil(ctx, agent.Question{Header: "Agent", Text: "Which agent runs it?", Options: options(opts...)}, func(a string) (string, error) {
		for _, k := range kinds {
			if strings.EqualFold(a, string(k)) {
				return string(k), nil
			}
		}
		return "", fmt.Errorf("pick one of the listed agents")
	})
	if err != nil {
		return err
	}
	def.Kind = agent.Kind(a)
	return nil
}

// askModel asks for the model in the vocabulary of the definition's kind:
// claude has aliases every install can name, codex wants its own model
// id (real ones, read off this host's Codex CLI where there is one:
// models.go), opencode a provider/model pair. Every kind's answer is free
// text over its options — the options are the models worth naming, not the
// models allowed.
func (w *wizard) askModel(ctx context.Context, def *agent.Definition) error {
	q := agent.Question{Header: "Model", Text: "Which model? Pick one or type a full model id.",
		Options: options("sonnet", "balanced", "opus", "frontier default", "fable", "most capable", "haiku", "fastest")}
	switch def.Kind {
	case agent.KindCodex:
		q = agent.Question{Header: "Model", Text: "Which model? Pick one or type a Codex model id.", Options: codexModels()}
	case agent.KindOpenCode:
		q = agent.Question{Header: "Model", Text: "Which model? Pick one or type it as `provider/model`.", Options: opencodeModels}
	}
	var err error
	def.Model, err = w.askUntil(ctx, q, nonEmpty("model"))
	return err
}

func (w *wizard) askEnvironment(ctx context.Context, def *agent.Definition) error {
	kind, err := w.askUntil(ctx, agent.Question{Header: "Environment", Text: "Where does it run?",
		Options: options("local", "on the dispatch host", "docker", "a container per task", "ssh", "on a remote host")}, func(a string) (string, error) {
		switch k := strings.ToLower(a); environment.Kind(k) {
		case environment.KindLocal, environment.KindDocker, environment.KindSSH:
			return k, nil
		}
		return "", fmt.Errorf("pick local, docker or ssh")
	})
	if err != nil {
		return err
	}
	// Settings the questions do not cover (ssh key, container env) only
	// make sense for the kind they were written for.
	env := environment.Spec{Kind: environment.Kind(kind)}
	sameKind := env.Kind == def.Environment.Kind
	if sameKind {
		env.KeyPath, env.Env = def.Environment.KeyPath, def.Environment.Env
		env.Provision = def.Environment.Provision
	}
	switch env.Kind {
	case environment.KindLocal:
		env.Workdir, err = w.askUntil(ctx, agent.Question{Header: "Working directory", Text: "Absolute path of the working directory on the dispatch host? `none` = a fresh directory per task." + pathHint,
			Options: options("none", "fresh directory per task")}, validateLocalDir)
	case environment.KindDocker:
		env.Image, err = w.askUntil(ctx, agent.Question{Header: "Image", Text: "Docker image? A plain base image is fine — dispatch installs git, node and the agent CLI into it the first time.",
			Options: options("ubuntu:24.04", "plain ubuntu", "debian:bookworm", "plain debian", "alpine:3.20", "small")}, nonEmpty("image"))
		if !sameKind {
			// A container dispatch creates is provisioned by default: the
			// wizard offers plain base images, so something has to put
			// the agent CLI in them.
			env.Provision = &environment.Provision{Agents: []string{agentKind(def.Kind)}}
		}
		if err == nil {
			env.Reuse, err = w.askReuse(ctx)
		}
		if err == nil {
			env.Workdir, err = w.askUntil(ctx, agent.Question{Header: "Working directory", Text: "Host directory to mount at `/work`? `none` = a directory dispatch manages." + pathHint,
				Options: options("none", "dispatch manages it")}, validateLocalDir)
		}
	case environment.KindSSH:
		env.Host, err = w.askUntil(ctx, agent.Question{Header: "Host", Text: "SSH host? `user@host` or an alias from `~/.ssh/config`."}, nonEmpty("host"))
		if err == nil {
			env.Workdir, err = w.askUntil(ctx, agent.Question{Header: "Working directory", Text: "Remote working directory? `none` = a fresh directory per task." + pathHint,
				Options: options("none", "fresh directory per task")}, func(a string) (string, error) {
				if isNone(a) {
					return "", nil
				}
				return a, nil
			})
		}
	}
	if err != nil {
		return err
	}
	def.Environment = env
	return nil
}

// agentKind is the CLI provisioning must install for a definition.
func agentKind(k agent.Kind) string {
	if k == "" {
		return string(agent.KindClaude)
	}
	return string(k)
}

// askReuse decides how long the container lives. Reuse is what makes a
// container feel like a machine: the same box, the same installed tools and
// the same agent session across every message in a thread.
func (w *wizard) askReuse(ctx context.Context) (environment.Reuse, error) {
	a, err := w.askUntil(ctx, agent.Question{Header: "Container", Text: "How long should the container live?",
		Options: options(
			string(environment.ReuseThread), "one per conversation, kept warm between messages",
			string(environment.ReuseTask), "a fresh container per task",
			string(environment.ReuseDefinition), "one shared by every conversation",
		)}, func(a string) (string, error) {
		switch r := environment.Reuse(strings.ToLower(a)); r {
		case environment.ReuseTask, environment.ReuseThread, environment.ReuseDefinition:
			return string(r), nil
		}
		return "", fmt.Errorf("pick task, thread or definition")
	})
	return environment.Reuse(a), err
}

func (w *wizard) askPermissions(ctx context.Context, def *agent.Definition) error {
	mode, err := w.askUntil(ctx, agent.Question{Header: "Permissions", Text: "Permission mode?",
		Options: options(
			string(agent.PermissionManual), "ask before every tool not pre-approved",
			string(agent.PermissionAcceptEdits), "file edits without asking, ask for the rest",
			string(agent.PermissionAuto), "let Claude decide",
			string(agent.PermissionBypass), "never ask (trusted sandboxes only)",
		)}, func(a string) (string, error) {
		for _, m := range []agent.PermissionMode{agent.PermissionManual, agent.PermissionAcceptEdits, agent.PermissionAuto, agent.PermissionBypass} {
			if strings.EqualFold(a, string(m)) {
				return string(m), nil
			}
		}
		return "", fmt.Errorf("pick one of the listed modes")
	})
	if err != nil {
		return err
	}
	def.PermissionMode = agent.PermissionMode(mode)
	return nil
}

func (w *wizard) askTools(ctx context.Context, def *agent.Definition) error {
	var toolOpts []string
	for _, p := range toolPresets {
		toolOpts = append(toolOpts, p.label, p.desc)
	}
	tools, err := w.askUntil(ctx, agent.Question{Header: "Tools", Text: "Pre-approved tools? Pick a preset or type a comma-separated list (e.g. `Read,Edit,Bash(npm test:*)`).",
		Options: options(toolOpts...)}, func(a string) (string, error) { return a, nil })
	if err != nil {
		return err
	}
	def.AllowedTools = parseTools(tools)
	return nil
}

func (w *wizard) askSystemPrompt(ctx context.Context, def *agent.Definition) error {
	var err error
	def.SystemPrompt, err = w.askUntil(ctx, agent.Question{Header: "System prompt", Text: "Extra instructions for the agent? Type them, or skip.",
		Options: options("Skip", "no extra instructions")}, func(a string) (string, error) {
		if isNone(a) {
			return "", nil
		}
		return a, nil
	})
	return err
}

func nonEmpty(what string) func(string) (string, error) {
	return func(a string) (string, error) {
		if isNone(a) {
			return "", fmt.Errorf("%s cannot be empty", what)
		}
		return a, nil
	}
}

func validateLocalDir(a string) (string, error) {
	if isNone(a) {
		return "", nil
	}
	if strings.HasPrefix(a, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			a = filepath.Join(home, a[2:])
		}
	}
	if !filepath.IsAbs(a) {
		return "", fmt.Errorf("%q is not an absolute path", a)
	}
	if fi, err := os.Stat(a); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%q is not a directory on this host", a)
	}
	return filepath.Clean(a), nil
}

// parseTools maps a preset label or a comma-separated list to tool names.
func parseTools(a string) []string {
	for _, p := range toolPresets {
		if strings.EqualFold(a, p.label) {
			return append([]string(nil), p.tools...)
		}
	}
	var out []string
	for _, t := range strings.Split(a, ",") {
		if t = strings.TrimSpace(t); t != "" && !isNone(t) {
			out = append(out, t)
		}
	}
	return out
}

// describeEnvironment renders the environment on one line.
func describeEnvironment(d agent.Definition) string {
	var b strings.Builder
	b.WriteString(d.Environment.String())
	if d.Environment.Kind == environment.KindDocker {
		if d.Environment.Provision != nil {
			b.WriteString(" · provisioned")
		}
		switch d.Environment.Reuse {
		case environment.ReuseThread:
			b.WriteString(" · container per thread")
		case environment.ReuseDefinition:
			b.WriteString(" · one shared container")
		}
	}
	switch {
	case d.Environment.Workdir != "":
	case d.Environment.ReuseKey != "" || d.Environment.Reuse == environment.ReuseThread || d.Environment.Reuse == environment.ReuseDefinition:
		b.WriteString(" · directory dispatch manages")
	default:
		b.WriteString(" · fresh directory per task")
	}
	if d.Environment.KeyPath != "" {
		fmt.Fprintf(&b, " · key `%s`", d.Environment.KeyPath)
	}
	if len(d.Environment.Env) > 0 {
		keys := make([]string, 0, len(d.Environment.Env))
		for k := range d.Environment.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, " · env %s", strings.Join(keys, ", "))
	}
	return b.String()
}

func summarize(title string, d agent.Definition) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n• name: *%s*\n• agent: %s\n• model: %s\n• environment: %s", title, d.Name, agentKind(d.Kind), d.Model, describeEnvironment(d))
	fmt.Fprintf(&b, "\n• permission mode: %s", d.PermissionMode)
	if len(d.AllowedTools) > 0 {
		fmt.Fprintf(&b, "\n• pre-approved tools: %s", strings.Join(d.AllowedTools, ", "))
	} else {
		b.WriteString("\n• pre-approved tools: none")
	}
	if d.SystemPrompt != "" {
		fmt.Fprintf(&b, "\n• system prompt: %s", truncate(d.SystemPrompt, 200))
	}
	return b.String()
}

// truncate cuts at a rune boundary — the result is posted to a chat thread
// and written to the event log, and neither wants half a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
