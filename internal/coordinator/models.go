package coordinator

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/cleanunicorn/dispatch/internal/agent"
)

// Model options are per kind, because a model id is vendor vocabulary:
// claude has aliases every install can name (`sonnet`, `opus`), codex and
// opencode want a real id. Whatever is offered, a typed answer wins — the
// options are a shortcut, never the list of what is allowed.

// codexFallbackModels are the Codex models dispatch was built knowing, used
// when this host has no Codex CLI to ask (see codexCachedModels). Ids age;
// the list is a starting point and the question takes a typed id over any
// of them.
var codexFallbackModels = []agent.Option{
	{Label: "gpt-5.6-sol", Description: "reliable agentic workhorse"},
	{Label: "gpt-5.6-terra", Description: "balanced quality, latency and cost"},
	{Label: "gpt-5.6-luna", Description: "fast and affordable"},
	{Label: "gpt-6-astra", Description: "most capable, for demanding work"},
	{Label: "gpt-5.5", Description: "previous generation"},
	{Label: "gpt-5.4-mini", Description: "small, fast, cost-efficient"},
}

// opencodeModels are provider/model pairs to start from. OpenCode reaches
// every provider, so this is an example set and nothing more.
var opencodeModels = []agent.Option{
	{Label: "zai-coding-plan/glm-4.6", Description: "GLM 4.6 on the Z.ai coding plan"},
	{Label: "deepseek/deepseek-chat", Description: "DeepSeek"},
}

// codexModels is what the Model question offers for a codex definition:
// the models this host's Codex CLI was last told about, else the built-in
// list. The CLI keeps its own list in `models_cache.json` and refreshes it
// from OpenAI, so a host that runs codex knows the current models better
// than a list compiled into dispatch does.
func codexModels() []agent.Option {
	if opts := codexCachedModels(codexHome()); len(opts) > 0 {
		return opts
	}
	return codexFallbackModels
}

// codexHome is the Codex CLI's config directory, the same one it reads:
// $CODEX_HOME, else ~/.codex. Empty when there is no home to look in.
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// maxModelOptions caps how many models the question offers. The cache
// carries everything an account can reach; a prompt is a shortcut, and a
// long one is worse than a short one plus a typed id.
const maxModelOptions = 6

// codexCachedModels reads the CLI's model cache, keeping the models it
// would itself list (`visibility: "list"` — the others are internal, like
// the review model) in the order the cache carries them. Anything the file
// does not answer for — missing, unreadable, a shape this dispatch does not
// know — is no models, not an error: the caller falls back.
func codexCachedModels(home string) []agent.Option {
	if home == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, "models_cache.json"))
	if err != nil {
		return nil
	}
	var cache struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil
	}
	var out []agent.Option
	for _, m := range cache.Models {
		if m.Slug == "" || m.Visibility != "list" {
			continue
		}
		desc := m.Description
		if desc == "" {
			desc = m.DisplayName
		}
		out = append(out, agent.Option{Label: m.Slug, Description: desc})
		if len(out) == maxModelOptions {
			break
		}
	}
	return out
}
