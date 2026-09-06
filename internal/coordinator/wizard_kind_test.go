package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	envlocal "github.com/cleanunicorn/dispatch/internal/environment/local"
	execlocal "github.com/cleanunicorn/dispatch/internal/executor/local"
	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	"github.com/cleanunicorn/dispatch/internal/surface"
	"github.com/cleanunicorn/dispatch/internal/surface/chat"
	"github.com/cleanunicorn/dispatch/internal/transport"
)

// TestAddAgentAsksKindWhenThereIsAChoice: with more than one driver
// registered the wizard asks which agent runs the definition, right after
// the name, and the answer is what gets stored; with one driver (the other
// tests) the question is skipped.
func TestAddAgentAsksKindWhenThereIsAChoice(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ex := execlocal.New(map[agent.Kind]agent.Agent{"fake": fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.AgentKinds = []agent.Kind{agent.KindClaude, "fake"}
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/30.0")
	tr.say(th, "agent add")
	tr.waitFor(t, th, "Name for the new agent")
	tr.say(th, "picker")
	kind := tr.waitFor(t, th, "Which agent runs it?")
	if kind.Prompt == nil || len(kind.Prompt.Options) != 2 || kind.Prompt.Options[0].Label != "claude" || kind.Prompt.Options[0].Description != "Claude Code" || kind.Prompt.Options[1].Label != "fake" {
		t.Fatalf("kind prompt = %+v", kind.Prompt)
	}
	tr.say(th, "gemini")
	tr.waitFor(t, th, "pick one of the listed agents")
	tr.say(th, "FAKE")
	tr.waitFor(t, th, "Which model")
	tr.say(th, "m1")
	tr.waitFor(t, th, "Where does it run")
	tr.say(th, "local")
	tr.waitFor(t, th, "Absolute path")
	tr.say(th, "none")
	tr.waitFor(t, th, "Permission mode?")
	tr.say(th, "manual")
	tr.waitFor(t, th, "Pre-approved tools")
	tr.say(th, "Read-only")
	tr.waitFor(t, th, "Extra instructions")
	tr.say(th, "skip")
	summary := tr.waitFor(t, th, "Save this agent?")
	if !strings.Contains(summary.Text, "• agent: fake") {
		t.Fatalf("summary does not name the agent:\n%s", summary.Text)
	}
	tr.say(th, "save")
	tr.waitFor(t, th, "agent *picker* saved")

	def, err := st.GetDefinition(ctx, "picker")
	if err != nil {
		t.Fatal(err)
	}
	if def.Kind != "fake" || def.Model != "m1" {
		t.Fatalf("stored definition = %+v", def)
	}
}

// TestAddCodexAgentOffersRealModels: the Model question for a codex
// definition is a pick list of ids that exist on this host, not a blank
// "type a model id" — and typing one anyway still wins.
func TestAddCodexAgentOffersRealModels(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "models_cache.json"), `{"models":[
		{"slug":"gpt-9-new","display_name":"GPT-9-New","description":"the newest","visibility":"list"},
		{"slug":"gpt-8-old","display_name":"GPT-8-Old","description":"the older one","visibility":"list"}
	]}`)
	t.Setenv("CODEX_HOME", home)

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ex := execlocal.New(map[agent.Kind]agent.Agent{agent.KindCodex: fakeAgent{}}, map[environment.Kind]environment.Factory{environment.KindLocal: envlocal.Factory{}}, 200*time.Millisecond)
	tr := &fakeTransport{name: "slack", ready: make(chan struct{})}
	c := New(st, ex, []transport.Transport{tr}, []surface.Surface{chat.New("chat", "slack", false)}, nil)
	c.AgentKinds = []agent.Kind{agent.KindClaude, agent.KindCodex}
	go c.Run(ctx)
	<-tr.ready

	th := transport.ThreadID("C-dev/31.0")
	tr.say(th, "agent add")
	tr.waitFor(t, th, "Name for the new agent")
	tr.say(th, "coder")
	tr.waitFor(t, th, "Which agent runs it?")
	tr.say(th, "codex")
	model := tr.waitFor(t, th, "Which model")
	if model.Prompt == nil || len(model.Prompt.Options) != 2 {
		t.Fatalf("model prompt = %+v", model.Prompt)
	}
	if model.Prompt.Options[0].Label != "gpt-9-new" || model.Prompt.Options[0].Description != "the newest" {
		t.Fatalf("first option = %+v", model.Prompt.Options[0])
	}
	tr.say(th, "gpt-8-old")
	tr.waitFor(t, th, "Where does it run")
	tr.say(th, "local")
	tr.waitFor(t, th, "Absolute path")
	tr.say(th, "none")
	tr.waitFor(t, th, "Permission mode?")
	tr.say(th, "manual")
	tr.waitFor(t, th, "Pre-approved tools")
	tr.say(th, "Read-only")
	tr.waitFor(t, th, "Extra instructions")
	tr.say(th, "skip")
	tr.waitFor(t, th, "Save this agent?")
	tr.say(th, "save")
	tr.waitFor(t, th, "agent *coder* saved")

	def, err := st.GetDefinition(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}
	if def.Kind != agent.KindCodex || def.Model != "gpt-8-old" {
		t.Fatalf("stored definition = %+v", def)
	}
}
