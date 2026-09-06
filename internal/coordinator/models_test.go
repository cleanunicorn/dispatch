package coordinator

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCodexModelsFromCache: with a Codex CLI on the host, the models the
// question offers are the ones that CLI would list — in its order, without
// the internal ones, and capped.
func TestCodexModelsFromCache(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "models_cache.json"), `{"fetched_at":"now","models":[
		{"slug":"gpt-9-new","display_name":"GPT-9-New","description":"the newest","visibility":"list"},
		{"slug":"codex-auto-review","display_name":"Codex Auto Review","description":"internal","visibility":"hide"},
		{"slug":"gpt-8-old","display_name":"GPT-8-Old","visibility":"list"},
		{"slug":"","display_name":"nameless","visibility":"list"}
	]}`)
	t.Setenv("CODEX_HOME", home)

	got := codexModels()
	if len(got) != 2 {
		t.Fatalf("models = %+v, want the two listed ones", got)
	}
	if got[0].Label != "gpt-9-new" || got[0].Description != "the newest" {
		t.Errorf("first = %+v", got[0])
	}
	// No description of its own: the display name says something at least.
	if got[1].Label != "gpt-8-old" || got[1].Description != "GPT-8-Old" {
		t.Errorf("second = %+v", got[1])
	}
}

// TestCodexModelsCapped: an account that can reach everything still gets a
// question someone can read.
func TestCodexModelsCapped(t *testing.T) {
	home := t.TempDir()
	body := `{"models":[`
	for i := 0; i < maxModelOptions+4; i++ {
		if i > 0 {
			body += ","
		}
		body += `{"slug":"m` + string(rune('a'+i)) + `","display_name":"M","visibility":"list"}`
	}
	write(t, filepath.Join(home, "models_cache.json"), body+`]}`)
	t.Setenv("CODEX_HOME", home)

	if got := codexModels(); len(got) != maxModelOptions {
		t.Fatalf("len(models) = %d, want %d", len(got), maxModelOptions)
	}
}

// TestCodexModelsFallback: no Codex CLI, an unreadable cache or one shaped
// some other way is not an error — the built-in list answers instead.
func TestCodexModelsFallback(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("CODEX_HOME", empty)
	if got := codexModels(); len(got) != len(codexFallbackModels) {
		t.Fatalf("no cache: models = %+v, want the built-in list", got)
	}

	write(t, filepath.Join(empty, "models_cache.json"), "not json at all")
	if got := codexModels(); len(got) != len(codexFallbackModels) {
		t.Fatalf("bad cache: models = %+v, want the built-in list", got)
	}

	write(t, filepath.Join(empty, "models_cache.json"), `{"models":[{"slug":"hidden","visibility":"hide"}]}`)
	if got := codexModels(); len(got) != len(codexFallbackModels) {
		t.Fatalf("nothing listed: models = %+v, want the built-in list", got)
	}

	if got := codexCachedModels(""); got != nil {
		t.Fatalf("no home: models = %+v, want none", got)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
