package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestCodexProfileCatalogKeepsInstructionsLocally(t *testing.T) {
	home, read := codexHome(t, "", "model_reasoning_effort = \"low\"\n")
	if err := provider.SetModelEfforts("fake/m1", []string{"low", "high"}); err != nil {
		t.Fatal(err)
	}
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	original := read()
	path := filepath.Join(home, ".codex", "magpie-models.json")
	var cat map[string]any
	json.Unmarshal([]byte(readFile(path)), &cat)
	m := cat["models"].([]any)[0].(map[string]any)
	m["base_instructions"] = "profile instructions"
	m["default_reasoning_level"] = "low"
	b, _ := json.Marshal(cat)
	writeFile(t, path, string(b))
	if err := codex(home).Sync(); err != nil {
		t.Fatal(err)
	}
	if settings.Load().ModelCodexInstructions["fake/m1"] != "" || !strings.Contains(readFile(path), "profile instructions") {
		t.Fatal("profile edit was lost or written back into provider settings")
	}
	// The profile's effort drives its catalog even when the main config
	// chooses a different default for plain Codex.
	if err := codex(home).Field("effort").Set("high"); err != nil {
		t.Fatal(err)
	}
	_, _, models, err := codexCatalogRead([]byte(readFile(path)))
	if err != nil || string(models["fake/m1"]["default_reasoning_level"]) != `"high"` || string(models["fake/m1"]["base_instructions"]) != `"profile instructions"` {
		t.Fatal("profile effort used the main config or replaced local instructions")
	}
	if read() != original {
		t.Fatal("profile import changed main Codex config")
	}
}

// Codex wired by its profile alone (hpnjstar on X): codex -p magpie goes
// through magpie, and config.toml, which plain codex reads, is not touched
// by connecting, switching models or disconnecting.
func TestCodexProfileOnlyLeavesConfigTomlAlone(t *testing.T) {
	const own = "model = \"gpt-5.5\"\nmodel_provider = \"acme\"\nopenai_base_url = \"https://relay.example/v1\"\n\n[model_providers.acme]\nname = \"acme\"\nbase_url = \"https://acme.example/v1\"\n"
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, own)
	dir := filepath.Join(home, ".codex")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	if cx.Path != filepath.Join(dir, "magpie.config.toml") {
		t.Fatalf("path %s", cx.Path)
	}
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	prof, _ := os.ReadFile(cx.Path)
	for _, want := range []string{`model_provider = "magpie"`, `model = "fake/m1"`, `model_catalog_json = "magpie-models.json"`, "[model_providers.magpie]"} {
		if !strings.Contains(string(prof), want) {
			t.Errorf("profile lacks %q:\n%s", want, prof)
		}
	}
	if read() != own {
		t.Fatalf("config.toml changed:\n%s", read())
	}
	if !cx.Wired() {
		t.Fatalf("not connected:\n%s", prof)
	}
	if d := cx.Drift(); d != nil {
		t.Fatalf("drift %+v", d)
	}
	if err := cx.Apply("effort", "high"); err != nil {
		t.Fatal(err)
	}
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if read() != own {
		t.Fatalf("config.toml changed after effort and sync:\n%s", read())
	}
	// a way around the scope while connected would leave both wirings
	if err := cx.Field("scope").Set(""); err == nil {
		t.Fatal("scope changed while connected")
	}
	list := filepath.Join(dir, "magpie-models.json")
	before := readFile(list)
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); ref != "magpie-models.json" {
		t.Fatalf("profile lost the catalog reference: %q", ref)
	}
	if readFile(list) != before {
		t.Fatal("disconnect changed the existing catalog")
	}
	if read() != own || codex(home).Wired() {
		t.Fatalf("after disconnect:\n%s", read())
	}
}

func TestCodexProfileKeepsLocalCatalogPath(t *testing.T) {
	home, read := codexHome(t, "", "model = \"gpt-5.5\"\n")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	cx = codex(home)
	writeFile(t, cx.Path, "model_catalog_json = \"my-models.json\"\n")
	local := filepath.Join(home, ".codex", "my-models.json")
	writeFile(t, local, `{"title":"my catalog","models":[{"slug":"fake/m1","base_instructions":"my instruction"}]}`)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "added", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m2"}}); err != nil {
		t.Fatal(err)
	}
	if err := codex(home).Sync(); err != nil {
		t.Fatal(err)
	}
	if ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); ref != "my-models.json" {
		t.Fatalf("profile catalog path changed: %q", ref)
	}
	for _, want := range []string{"my catalog", "my instruction", "added/m2"} {
		if !strings.Contains(readFile(local), want) {
			t.Fatalf("local catalog lost %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "magpie-models.json")); !os.IsNotExist(err) {
		t.Fatalf("profile wrote a second catalog: %v", err)
	}
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(local), "my instruction") {
		t.Fatal("profile disconnect removed the user's catalog")
	}
	if ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); ref != "my-models.json" {
		t.Fatalf("profile disconnect lost the local catalog reference: %q", ref)
	}
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); ref != "my-models.json" || !strings.Contains(readFile(local), "my instruction") {
		t.Fatal("profile reconnect lost the local catalog")
	}
	if read() != "model = \"gpt-5.5\"\n" {
		t.Fatal("profile changed config.toml")
	}
}

func TestCodexCatalogFileUsesCodexHome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".codex")
	ws := place{spell: func(string) string { return "/home/alice/.codex" }}
	mapped := place{spell: ws.spell, sys: func(ref string) string { return filepath.Join(dir, "distro", filepath.FromSlash(ref)) }}
	for _, tt := range []struct {
		name string
		at   place
		ref  string
		want string
	}{
		{"relative", place{}, "models/custom.json", filepath.Join(dir, "models", "custom.json")},
		{"absolute", place{}, filepath.Join(dir, "custom.json"), filepath.Join(dir, "custom.json")},
		{"WSL relative", ws, "models/custom.json", filepath.Join(dir, "models", "custom.json")},
		{"WSL native", ws, "/home/alice/.codex/custom.json", filepath.Join(dir, "custom.json")},
		{"WSL mapped absolute", mapped, "/opt/custom.json", filepath.Join(dir, "distro", "opt", "custom.json")},
		{"WSL unknown", ws, "/opt/custom.json", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := codexCatalogFile(tt.at, dir, tt.ref)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("unknown WSL path accepted: %q", got)
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("path %q, error %v, want %q", got, err, tt.want)
			}
		})
	}
}

// A model of Codex's own is not a profile model: plain codex runs those.
func TestCodexProfileOnlyRefusesAnOwnModel(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Field("scope").Set("profile"); err != nil {
		t.Fatal(err)
	}
	if err := codex(home).Field("model").Set("gpt-5.5"); err == nil {
		t.Fatal("an own model was written to the profile")
	}
}

// The default wiring still writes config.toml, and "applies to" refuses to
// move under it.
func TestCodexScopeWaitsForDisconnect(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, "")
	cx := codex(home)
	if err := cx.Apply("model", "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), "openai_base_url") {
		t.Fatalf("default wiring changed:\n%s", read())
	}
	if err := cx.Field("scope").Set("profile"); err == nil {
		t.Fatal("scope changed while connected")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "magpie.config.toml")); err == nil {
		t.Fatal("a profile was written by the default wiring")
	}
}

// Codex routed through magpie by the base URL alone (beside its ChatGPT
// sign-in, or for failover) reads as not wired, yet magpie keeps config.toml
// and its list there. "applies to" moving to the profile then made magpie
// stop looking at config.toml: its wiring stayed behind unmanaged and the
// row read disconnected, Codex on its own list (kakzyanaba on X).
func TestCodexScopeWaitsForBaseURLRouteToo(t *testing.T) {
	const routed = "openai_base_url = \"http://127.0.0.1:3425/backend-api/codex\"\n"
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, routed)
	cx := codex(home)
	if cx.Routed == nil || !cx.Routed() {
		t.Fatal("fixture is not routed by the base URL")
	}
	if err := cx.Field("scope").Set("profile"); err == nil {
		t.Fatal("scope changed while Codex is routed through magpie")
	}
	if codex(home).Path != filepath.Join(home, ".codex", "config.toml") || read() != routed {
		t.Fatalf("Codex moved off config.toml:\n%s", read())
	}
}
