package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/codexcat"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// What the user adds by hand to the model list magpie writes for Codex
// stays through the next model switch, sync and stepping out (#1450:
// supports_reasoning_summaries, put in for Codex to show a model's
// thinking, was gone after the next change). A key magpie writes, or took
// out on purpose (multi_agent_version with the V1 setting off), is
// magpie's.
func TestCodexCatalogKeepsWhatTheUserAdded(t *testing.T) {
	home, _ := codexHome(t, "", "")
	list := filepath.Join(home, ".codex", "magpie-models.json")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	// the file as magpie wrote it, edited by hand: two keys on fake/m1's
	// entry, and a version magpie no longer writes
	var cat map[string]any
	if err := json.Unmarshal([]byte(readFile(list)), &cat); err != nil {
		t.Fatal(err)
	}
	e := cat["models"].([]any)[0].(map[string]any)
	if e["slug"] != "fake/m1" {
		t.Fatalf("first entry: %v", e["slug"])
	}
	e["supports_reasoning_summaries"] = true
	e["default_reasoning_summary"] = "detailed"
	e["multi_agent_version"] = "v1"
	b, _ := json.MarshalIndent(cat, "", "  ")
	writeFile(t, list, string(b))

	has := func(when string) {
		t.Helper()
		var got struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal([]byte(readFile(list)), &got); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		var m1 map[string]any
		for _, m := range got.Models {
			if m["slug"] == "fake/m1" {
				m1 = m
			}
		}
		if m1 == nil || m1["supports_reasoning_summaries"] != true || m1["default_reasoning_summary"] != "detailed" {
			t.Errorf("%s: the user's keys are gone: %v %v", when, m1["supports_reasoning_summaries"], m1["default_reasoning_summary"])
		}
		// magpie writes v2 for a model it offers Ultra (pingqipai3 on X),
		// never the user's v1 back
		if v, ok := m1["multi_agent_version"]; ok && v != "v2" {
			t.Errorf("%s: a version magpie no longer writes stayed: %v", when, v)
		}
		if m1["base_instructions"] == nil || m1["supports_parallel_tool_calls"] != true {
			t.Errorf("%s: magpie's own keys are gone", when)
		}
	}

	// a provider added: the sync writes the list again
	if err := provider.Save(provider.Provider{ID: "added", Name: "Added", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m2"}}); err != nil {
		t.Fatal(err)
	}
	SyncCatalog()
	if !strings.Contains(readFile(list), `"added/m2"`) {
		t.Fatal("sync didn't write the new model")
	}
	has("after a sync")

	if err := cx.Fields[0].Set("added/m2"); err != nil {
		t.Fatal(err)
	}
	has("after a model switch")

	// Codex's own default: magpie steps out, and the list the user added
	// to stays for when magpie is back
	if err := cx.Fields[0].Set(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(list); err != nil {
		t.Fatalf("the list the user added to was removed: %v", err)
	}
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	has("after stepping out and back")
}

// A list holding nothing of the user's still goes as magpie steps out.
func TestCodexCatalogRemovedWhenNothingAdded(t *testing.T) {
	home, _ := codexHome(t, "", "")
	list := filepath.Join(home, ".codex", "magpie-models.json")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(list); err != nil {
		t.Fatal(err)
	}
	if err := cx.Fields[0].Set(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(list); err == nil {
		t.Error("magpie's list stayed after it stepped out")
	}
}

func TestCodexCatalogImportsOwnedEdits(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "magpie-models.json")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	editModel := func(change func(map[string]any)) {
		t.Helper()
		var cat map[string]any
		if err := json.Unmarshal([]byte(readFile(path)), &cat); err != nil {
			t.Fatal(err)
		}
		for _, v := range cat["models"].([]any) {
			m := v.(map[string]any)
			if m["slug"] == "fake/m1" {
				change(m)
			}
		}
		b, _ := json.Marshal(cat)
		writeFile(t, path, string(b))
	}
	editModel(func(m map[string]any) {
		m["base_instructions"] = "My exact instruction.\n"
		m["context_window"] = 128000
		m["auto_compact_token_limit"] = 96000
		m["input_modalities"] = []string{"text", "image"}
		m["supported_reasoning_levels"] = []map[string]string{{"effort": "low"}, {"effort": "high"}}
	})
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	s := settings.Load()
	if s.ModelCodexInstructions["fake/m1"] != "My exact instruction.\n" || s.ModelCompacts["fake/m1"] != 96000 || !s.ModelImages["fake/m1"] {
		t.Fatalf("edits did not reach Magpie: prompt %q compact %d images %v", s.ModelCodexInstructions["fake/m1"], s.ModelCompacts["fake/m1"], s.ModelImages["fake/m1"])
	}
	p, _, _ := provider.Resolve("fake/m1")
	if p.Contexts["m1"] != 128000 || strings.Join(s.ModelEfforts["fake/m1"], ",") != "low,high" {
		t.Fatal("specs not saved")
	}
	// Recreate the agent, as at restart, and regenerate from persisted values.
	if err := codex(home).Sync(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(path), "My exact instruction.") {
		t.Fatal("restart lost instruction")
	}
	editModel(func(m map[string]any) {
		m["model_messages"] = map[string]any{"instructions_template": "Template edited alone"}
	})
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if settings.Load().ModelCodexInstructions["fake/m1"] != "Template edited alone" {
		t.Fatal("modern template edit lost")
	}
	editModel(func(m map[string]any) { delete(m, "base_instructions") })
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if settings.Load().ModelCodexInstructions["fake/m1"] != "" || strings.Contains(readFile(path), "Template edited alone") {
		t.Fatal("deleting override did not restore default")
	}
}

func TestCodexCatalogConflictAndBrokenFileAreKept(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "magpie-models.json")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	var cat map[string]any
	json.Unmarshal([]byte(readFile(path)), &cat)
	m := cat["models"].([]any)[0].(map[string]any)
	m["base_instructions"] = "file choice"
	b, _ := json.Marshal(cat)
	writeFile(t, path, string(b))
	s := settings.Load()
	s.ModelCodexInstructions = map[string]string{"fake/m1": "settings choice"}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	baseline := readFile(codexBaselinePath(path))
	if err := cx.Sync(); err == nil || !strings.Contains(err.Error(), "changed both") {
		t.Fatalf("conflict: %v", err)
	}
	if readFile(path) != string(b) || readFile(codexBaselinePath(path)) != baseline {
		t.Fatal("conflict changed file or baseline")
	}
	// Explicit import resolves the conflict in favor of the selected field.
	if err := ImportCodexCatalog(path, "fake/m1", []string{"base_instructions"}); err != nil {
		t.Fatal(err)
	}
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if settings.Load().ModelCodexInstructions["fake/m1"] != "file choice" {
		t.Fatal("explicit import ignored")
	}
	writeFile(t, path, `{"models":[`)
	baseline = readFile(codexBaselinePath(path))
	if err := cx.Sync(); err == nil {
		t.Fatal("broken JSON accepted")
	}
	if readFile(path) != `{"models":[` || readFile(codexBaselinePath(path)) != baseline {
		t.Fatal("broken JSON replaced")
	}
}

func TestCodexCatalogImportCustomFileDoesNotAdoptIt(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "my-models.json")
	raw := `{"models":[{"slug":"fake/m1","base_instructions":"custom instruction","context_window":128000}]}`
	writeFile(t, path, raw)
	if err := ImportCodexCatalog(path, "fake/m1", []string{"base_instructions"}); err != nil {
		t.Fatal(err)
	}
	if readFile(path) != raw {
		t.Fatal("custom file overwritten")
	}
	if _, err := os.Stat(codexBaselinePath(path)); !os.IsNotExist(err) {
		t.Fatal("custom file adopted")
	}
	p, _, _ := provider.Resolve("fake/m1")
	if p.Contexts["m1"] == 128000 {
		t.Fatal("unselected field imported")
	}
	if !strings.Contains(string(codexcat.Catalog(magpieModels("codex"))), "custom instruction") {
		t.Fatal("saved prompt missing from generated catalog")
	}
}

func TestCodexCatalogDesktopAndProfileEdits(t *testing.T) {
	for _, name := range []string{"magpie-models.json", "magpie-desktop-models.json"} {
		t.Run(name, func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			path := filepath.Join(home, ".codex", name)
			build := func() ([]byte, error) { return codexcat.Catalog(magpieModels("codex")), nil }
			if err := writeCodexCatalog(path, build, name == "magpie-models.json"); err != nil {
				t.Fatal(err)
			}
			var cat map[string]any
			json.Unmarshal([]byte(readFile(path)), &cat)
			cat["models"].([]any)[0].(map[string]any)["base_instructions"] = "both modes"
			b, _ := json.Marshal(cat)
			writeFile(t, path, string(b))
			if err := writeCodexCatalog(path, build, name == "magpie-models.json"); err != nil {
				t.Fatal(err)
			}
			if settings.Load().ModelCodexInstructions["fake/m1"] != "both modes" {
				t.Fatal("edit not imported")
			}
		})
	}
}

func TestCodexCatalogPartialImportKeepsOtherEdits(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "magpie-models.json")
	var cat map[string]any
	json.Unmarshal([]byte(readFile(path)), &cat)
	m := cat["models"].([]any)[0].(map[string]any)
	m["base_instructions"], m["context_window"] = "selected prompt", 128000
	b, _ := json.Marshal(cat)
	writeFile(t, path, string(b))
	if err := ImportCodexCatalog(path, "fake/m1", []string{"base_instructions"}); err != nil {
		t.Fatal(err)
	}
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	p, _, _ := provider.Resolve("fake/m1")
	if p.Contexts["m1"] != 128000 {
		t.Fatal("import swallowed the other pending edit")
	}
}

func TestCodexCatalogDisconnectKeepsPendingEdits(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "magpie-models.json")
	var cat map[string]any
	json.Unmarshal([]byte(readFile(path)), &cat)
	cat["models"].([]any)[0].(map[string]any)["base_instructions"] = "pending edit"
	b, _ := json.Marshal(cat)
	writeFile(t, path, string(b))
	if err := cx.Fields[0].Set(""); err != nil {
		t.Fatal(err)
	}
	if readFile(path) != string(b) {
		t.Fatal("disconnect discarded pending edit")
	}
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if settings.Load().ModelCodexInstructions["fake/m1"] != "pending edit" {
		t.Fatal("reconnect lost pending edit")
	}
}

func TestCodexCatalogConcurrentSaveIsKept(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "magpie-models.json")
	build := func() ([]byte, error) { return codexcat.Catalog(magpieModels("codex")), nil }
	if err := writeCodexCatalog(path, build, true); err != nil {
		t.Fatal(err)
	}
	baseline := readFile(codexBaselinePath(path))
	newer := `{"models":[{"slug":"fake/m1","base_instructions":"newer user save"}]}`
	a := atomic(&Agent{Sync: func() error {
		return writeCodexCatalog(path, func() ([]byte, error) {
			writeFile(t, path, newer)
			return build()
		}, true)
	}}, filepath.Join(home, ".codex", "config.toml"))
	if err := a.Sync(); err == nil || !strings.Contains(err.Error(), "changed while syncing") {
		t.Fatalf("concurrent save: %v", err)
	}
	if readFile(path) != newer || readFile(codexBaselinePath(path)) != baseline {
		t.Fatal("rollback erased concurrent save")
	}
}

func TestCodexCatalogImportSynchronousNotification(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "magpie-models.json")
	var cat map[string]any
	json.Unmarshal([]byte(readFile(path)), &cat)
	cat["models"].([]any)[0].(map[string]any)["base_instructions"] = "synchronous callback"
	b, _ := json.Marshal(cat)
	writeFile(t, path, string(b))
	was := catalog.Changed
	catalog.Changed = SyncCatalog
	t.Cleanup(func() { catalog.Changed = was })
	if err := ImportCodexCatalog(path, "fake/m1", []string{"base_instructions"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(path), "synchronous callback") {
		t.Fatal("callback lost import")
	}
}

func TestCodexCatalogBrokenBaselineRefusesImport(t *testing.T) {
	home, _ := codexHome(t, "", "")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "magpie-models.json")
	original := readFile(path)
	writeFile(t, codexBaselinePath(path), `{"version":1,"models":{"fake/m1":{}}}`)
	before := readFile(settings.Path())
	if err := ImportCodexCatalog(path, "fake/m1", []string{"base_instructions"}); err == nil {
		t.Fatal("incomplete baseline accepted")
	}
	if err := cx.Sync(); err == nil {
		t.Fatal("sync accepted incomplete baseline")
	}
	if readFile(path) != original || readFile(settings.Path()) != before {
		t.Fatal("bad baseline changed state")
	}
}
