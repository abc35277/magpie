package agent

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

// providers is providers.json at path, parsed.
func copilotParsed(t *testing.T, path string) (provs, models []map[string]any, top map[string]any) {
	t.Helper()
	if err := json.Unmarshal([]byte(readFile(path)), &top); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, readFile(path))
	}
	for _, k := range []struct {
		key string
		to  *[]map[string]any
	}{{"providers", &provs}, {"models", &models}} {
		if l, ok := top[k.key].([]any); ok {
			for _, e := range l {
				*k.to = append(*k.to, e.(map[string]any))
			}
		}
	}
	return
}

func copilotSandbox(t *testing.T) (*Agent, string) {
	t.Helper()
	home := effortHome(t)
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("COPILOT_PROVIDERS_CONFIG", "")
	return copilot(home, filepath.Join(home, ".config")), filepath.Join(home, ".copilot", "providers.json")
}

// Connecting Copilot CLI to magpie's model writes the provider Copilot reads
// from providers.json (BYOK, no GitHub sign-in) and picks it in settings.json
// as <provider>/<model>, the way Copilot's /model does (tony9759 on Discord).
func TestCopilotModelWritesProvidersJSON(t *testing.T) {
	cp, provs := copilotSandbox(t)
	if err := cp.Pick("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if got := cp.Field("model").Get(); got != "magpie/deepseek/pro" {
		t.Fatalf("model reads %q", got)
	}
	ps, ms, _ := copilotParsed(t, provs)
	if len(ps) != 1 || ps[0]["name"] != "magpie" || ps[0]["type"] != "openai" ||
		ps[0]["baseUrl"] != gateway.URL()+"/v1" || ps[0]["apiKey"] != agentKey("copilot") {
		t.Fatalf("provider: %v", ps)
	}
	found := map[string]bool{}
	for _, m := range ms {
		if m["provider"] != "magpie" {
			t.Fatalf("model of another provider: %v", m)
		}
		found[m["id"].(string)] = true
	}
	if !found["deepseek/pro"] || !found["deepseek/flash"] {
		t.Fatalf("models: %v", found)
	}
	if !cp.Wired() {
		t.Fatal("not wired")
	}
	if msg := cp.Check(); msg != "" {
		t.Fatalf("check: %s", msg)
	}
}

// What the user keeps in providers.json, a provider of theirs, a model of
// theirs, a key magpie doesn't know, is left as it was by connect, by sync
// and by disconnect; their own model in settings.json is not touched until
// magpie's is picked, and Disconnect puts it back.
func TestCopilotKeepsTheUsersOwn(t *testing.T) {
	cp, provs := copilotSandbox(t)
	writeFile(t, provs, `{"theme":"x","providers":[{"name":"mine","type":"openai","baseUrl":"https://mine.example/v1","apiKey":"K"}],`+
		`"models":[{"id":"m1","provider":"mine","name":"Mine"}]}`)
	writeFile(t, cp.Path, `{"model":"mine/m1","effortLevel":"high"}`)
	if err := cp.Connect(); err != nil {
		t.Fatal(err)
	}
	if v := cp.Field("model").Get(); !strings.HasPrefix(v, "magpie/") {
		t.Fatalf("connect left the model on %q", v)
	}
	ps, ms, top := copilotParsed(t, provs)
	if top["theme"] != "x" || len(ps) != 2 || ps[0]["name"] != "mine" || ps[0]["apiKey"] != "K" {
		t.Fatalf("the user's own is lost:\n%s", readFile(provs))
	}
	if ms[0]["id"] != "m1" || ms[0]["provider"] != "mine" {
		t.Fatalf("the user's model is lost:\n%s", readFile(provs))
	}
	if v, _ := edit.GetJSON(cp.Path, "effortLevel"); v != "high" {
		t.Fatalf("effortLevel %q", v)
	}
	if err := cp.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := cp.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if v := cp.Field("model").Get(); v != "mine/m1" {
		t.Fatalf("disconnect left the model on %q, not the one before", v)
	}
	ps, ms, top = copilotParsed(t, provs)
	if top["theme"] != "x" || len(ps) != 1 || ps[0]["name"] != "mine" || len(ms) != 1 || ms[0]["id"] != "m1" {
		t.Fatalf("after disconnect:\n%s", readFile(provs))
	}
	if cp.Wired() {
		t.Fatal("still wired")
	}
}

// A providers.json that isn't JSON is the user's, however it got that way:
// magpie says so and writes nothing.
func TestCopilotUnreadableProvidersJSONIsNotWrittenOver(t *testing.T) {
	cp, provs := copilotSandbox(t)
	writeFile(t, provs, `{"providers": [oops`)
	if err := cp.Field("model").Set("magpie/deepseek/pro"); err == nil {
		t.Fatal("no error for a file that does not parse")
	}
	if got := readFile(provs); got != `{"providers": [oops` {
		t.Fatalf("the file was written over:\n%s", got)
	}
	if v, _ := edit.GetJSON(cp.Path, "model"); v != "" {
		t.Fatalf("settings.json picked %q for a provider that was not written", v)
	}
}

// Sync follows the catalog, drift is told, and COPILOT_PROVIDERS_CONFIG /
// COPILOT_HOME move the files as they move Copilot's.
func TestCopilotSyncCheckAndEnv(t *testing.T) {
	cp, provs := copilotSandbox(t)
	cp.Field("model").Set("magpie/deepseek/pro")
	// a model dropped by hand comes back with the sync; the user's pick stays
	edit.WriteAtomic(provs, []byte(`{"providers":[{"name":"magpie","type":"openai","baseUrl":"http://127.0.0.1:1/v1","apiKey":"x"}],"models":[]}`))
	if cp.Check() == "" {
		t.Fatal("a provider pointing elsewhere is not told")
	}
	if err := cp.Sync(); err != nil {
		t.Fatal(err)
	}
	_, ms, _ := copilotParsed(t, provs)
	if len(ms) < 2 || cp.Check() != "" {
		t.Fatalf("sync left %d models, check %q", len(ms), cp.Check())
	}

	other := filepath.Join(t.TempDir(), "mine.json")
	t.Setenv("COPILOT_PROVIDERS_CONFIG", other)
	home := t.TempDir()
	cp2 := copilot(home, filepath.Join(home, ".config"))
	if err := cp2.Field("model").Set("magpie/deepseek/flash"); err != nil {
		t.Fatal(err)
	}
	if _, ms, _ := copilotParsed(t, other); len(ms) < 2 {
		t.Fatalf("COPILOT_PROVIDERS_CONFIG not written: %s", readFile(other))
	}
	t.Setenv("COPILOT_PROVIDERS_CONFIG", "")
	t.Setenv("COPILOT_HOME", filepath.Join(home, "ch"))
	if got := copilot(home, filepath.Join(home, ".config")).Path; got != filepath.Join(home, "ch", "settings.json") {
		t.Fatalf("COPILOT_HOME ignored: %s", got)
	}
}

// #5885: Copilot's /model offers its effort picker only to a model whose
// entry declares capabilities.supports.reasoning_effort, which magpie's
// never did — the effort stood for every session, set from magpie alone.
// Each of magpie's models now declares the levels its catalog lists,
// weakest first, each a level Copilot names; a word it doesn't — Codex's
// ultra, a vendor's own — is left out, the gateway fitting what is asked
// to the model's own nearest, and a model with no levels declares none,
// Copilot's own defaults standing.
func TestCopilotEffortsInProvidersJSON(t *testing.T) {
	cp, provs := copilotSandbox(t)
	// the models of a provider are the user's picks of its list; plain is
	// picked with no levels of its own listed
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://api.deepseek.com/v1", Key: "k",
		Models: []string{"pro", "flash", "plain"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("deepseek", "https://api.deepseek.com/v1", []catalog.Model{
		{ID: "pro", Name: "pro", Efforts: []string{"ultra", "none", "low", "high", "max"}},
		{ID: "flash", Name: "flash", Efforts: []string{"xhigh", "medium"}},
		{ID: "plain", Name: "plain"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cp.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	_, ms, _ := copilotParsed(t, provs)
	levels := func(id string) []string {
		t.Helper()
		for _, m := range ms {
			if m["id"] != id {
				continue
			}
			c, _ := m["capabilities"].(map[string]any)
			s, _ := c["supports"].(map[string]any)
			l, _ := s["reasoning_effort"].([]any)
			var out []string
			for _, v := range l {
				out = append(out, v.(string))
			}
			return out
		}
		t.Fatalf("no model %s written:\n%s", id, readFile(provs))
		return nil
	}
	if got, want := levels("deepseek/pro"), []string{"none", "low", "high", "max"}; !slices.Equal(got, want) {
		t.Fatalf("pro's levels %q, want %q\n%s", got, want, readFile(provs))
	}
	if got, want := levels("deepseek/flash"), []string{"medium", "xhigh"}; !slices.Equal(got, want) {
		t.Fatalf("flash's levels %q, want %q\n%s", got, want, readFile(provs))
	}
	for _, m := range ms {
		if m["id"] == "deepseek/plain" {
			if _, has := m["capabilities"]; has {
				t.Fatalf("a model with no levels declares capabilities:\n%s", readFile(provs))
			}
			return
		}
	}
	t.Fatalf("no model deepseek/plain written:\n%s", readFile(provs))
}

// What the user hand-patched into a model of magpie's — capabilities among
// them — magpie's next write takes back: its entries are its own, and the
// adapter is the lasting place for the field (#5885).
func TestCopilotEffortsRewrittenOnSync(t *testing.T) {
	cp, provs := copilotSandbox(t)
	if err := catalog.SaveLive("deepseek", "https://api.deepseek.com/v1", []catalog.Model{
		{ID: "pro", Name: "pro", Efforts: []string{"low", "high"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cp.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, provs, `{"providers":[{"name":"magpie","type":"openai","baseUrl":"http://127.0.0.1:1/v1","apiKey":"x"}],`+
		`"models":[{"id":"deepseek/pro","provider":"magpie","name":"pro","capabilities":{"supports":{"reasoning_effort":["low","medium","high"]}}}]}`)
	if err := cp.Sync(); err != nil {
		t.Fatal(err)
	}
	_, ms, _ := copilotParsed(t, provs)
	for _, m := range ms {
		if m["id"] != "deepseek/pro" {
			continue
		}
		c, _ := m["capabilities"].(map[string]any)
		s, _ := c["supports"].(map[string]any)
		l, _ := s["reasoning_effort"].([]any)
		if len(l) != 2 || l[0] != "low" || l[1] != "high" {
			t.Fatalf("sync left the hand-patched levels %v:\n%s", l, readFile(provs))
		}
		return
	}
	t.Fatalf("sync left no model deepseek/pro:\n%s", readFile(provs))
}
