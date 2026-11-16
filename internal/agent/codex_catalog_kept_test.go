package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/codexcat"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// What the user adds by hand to the model list magpie writes for Codex
// stays through the next model switch, sync and stepping out (#1450:
// supports_reasoning_summaries, put in for Codex to show a model's
// thinking, was gone after the next change). An unchanged generated key is
// also kept when the user customized it.
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
		if v := m1["multi_agent_version"]; v != "v1" {
			t.Errorf("%s: the user's version changed: %v", when, v)
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

func TestCodexCatalogKeepsOwnedEditsLocally(t *testing.T) {
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
	if s.ModelCodexInstructions["fake/m1"] != "" || s.ModelCompacts["fake/m1"] != 0 || s.ModelImages["fake/m1"] {
		t.Fatal("local edits changed shared Magpie preferences")
	}
	p, _, _ := provider.Resolve("fake/m1")
	if p.Contexts["m1"] != 0 || !slices.Equal(p.ModelEfforts()["m1"], []string{"low", "high"}) {
		t.Fatal("local reasoning selection was not shared, or unrelated context was imported")
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
	if !strings.Contains(readFile(path), "Template edited alone") {
		t.Fatal("modern template edit lost")
	}
	editModel(func(m map[string]any) { delete(m, "base_instructions") })
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	_, _, models, err := codexCatalogRead([]byte(readFile(path)))
	if err != nil || models["fake/m1"]["base_instructions"] != nil || !strings.Contains(readFile(path), "Template edited alone") {
		t.Fatal("sync replaced a local field deletion or another template field")
	}
}

func TestCodexCatalogMagpieChangeWinsAndBrokenFileIsKept(t *testing.T) {
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
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(path), "settings choice") || strings.Contains(readFile(path), "file choice") {
		t.Fatal("an explicit Magpie change did not replace the local value")
	}
	// Explicit import can still choose the file's value instead.
	writeFile(t, path, string(b))
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
	baseline := readFile(codexBaselinePath(path))
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
			if !strings.Contains(readFile(path), "both modes") || settings.Load().ModelCodexInstructions["fake/m1"] != "" {
				t.Fatal("local edit lost or imported into shared settings")
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
	_, _, models, err := codexCatalogRead([]byte(readFile(path)))
	if err != nil || string(models["fake/m1"]["context_window"]) != "128000" || p.Contexts["m1"] != 0 {
		t.Fatal("import swallowed or shared another local edit")
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
	if !strings.Contains(readFile(path), "pending edit") || settings.Load().ModelCodexInstructions["fake/m1"] != "" {
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
	writeFile(t, path, `{"models":[]}`)
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
	if _, disabled := provider.IDDisabled("fake/m1"); disabled {
		t.Fatal("stale file deletion disabled a model restored by a newer save")
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

func TestCodexCatalogAdoptsLocalJSONAndMergesSourceChanges(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			path := filepath.Join(home, ".codex", "custom-models.json")
			local := `{"custom":{"null":null,"text":"untouched"},"metadata":{"vendor":1,"removed":10,"local":true},"models":[
				{"slug":"fake/b","priority":1,"extra":"b"},
				{"slug":"manual/only","priority":2,"extra":null},
				{"slug":"fake/a","priority":3,"base_instructions":"local prompt","context_window":123000,"model_messages":{"instructions_template":"local template","variables":{"managed":"local","removed":10,"extra":null}},"deleted_later":{"managed":1,"extra":2},"null_later":{"extra":3},"local_flag":false}]}`
			writeFile(t, path, local)
			if version == 1 {
				writeFile(t, codexBaselinePath(path), `{"version":1,"models":{"fake/a":{"fields":{"slug":"old-hash"},"settings":{}}}}`)
			}
			generated := `{"metadata":{"vendor":1,"removed":10},"models":[{"slug":"fake/a","priority":1,"base_instructions":"default prompt","context_window":272000,"model_messages":{"instructions_template":"default template","variables":{"managed":"old","removed":10}},"deleted_later":{"managed":1},"null_later":{"managed":1}}, {"slug":"fake/b","priority":2}]}`
			build := func() ([]byte, error) { return []byte(generated), nil }
			if err := writeCodexCatalog(path, build, false); err != nil {
				t.Fatal(err)
			}
			if readFile(path) != local {
				t.Fatal("initial adoption changed local JSON")
			}
			var baseline codexCatalogBaseline
			if json.Unmarshal([]byte(readFile(codexBaselinePath(path))), &baseline) != nil || baseline.Version != 3 || !baseline.valid() || baseline.Models["fake/a"].Generated["base_instructions"].Hash != codexFieldHash(json.RawMessage(`"default prompt"`)) {
				t.Fatal("baseline recorded local customization instead of generated source")
			}
			// Adding a source model shifts priorities without changing the
			// relative source order of the surviving models.
			generated = strings.Replace(generated, `"models":[`, `"models":[{"slug":"fake/c","priority":0},`, 1)
			if err := writeCodexCatalog(path, build, false); err != nil {
				t.Fatal(err)
			}
			_, order, models, err := codexCatalogRead([]byte(readFile(path)))
			if err != nil || !slices.Equal(order, []string{"fake/b", "manual/only", "fake/a", "fake/c"}) || string(models["fake/c"]["priority"]) != "4" || string(models["fake/a"]["priority"]) != "3" {
				t.Fatalf("addition changed local order/priorities: %v %v", order, models)
			}
			generated = `{"metadata":{"vendor":2},"models":[{"slug":"fake/c","priority":0},{"slug":"fake/a","priority":1,"base_instructions":"default prompt","context_window":512000,"model_messages":{"instructions_template":"default template","variables":{"managed":"new"}},"null_later":null}]}`
			if err := writeCodexCatalog(path, build, false); err != nil {
				t.Fatal(err)
			}
			top, order, models, err := codexCatalogRead([]byte(readFile(path)))
			if err != nil || !slices.Equal(order, []string{"manual/only", "fake/a", "fake/c"}) || models["fake/b"] != nil {
				t.Fatalf("source removal lost custom entries/order: %v", order)
			}
			m := models["fake/a"]
			if string(m["base_instructions"]) != `"local prompt"` || string(m["context_window"]) != "512000" || string(m["null_later"]) != "null" || string(m["local_flag"]) != "false" {
				t.Fatalf("field merge: %s", readFile(path))
			}
			var messages struct {
				Template  string                     `json:"instructions_template"`
				Variables map[string]json.RawMessage `json:"variables"`
			}
			json.Unmarshal(m["model_messages"], &messages)
			var metadata, deleted map[string]json.RawMessage
			json.Unmarshal(top["metadata"], &metadata)
			json.Unmarshal(m["deleted_later"], &deleted)
			if messages.Template != "local template" || string(messages.Variables["managed"]) != `"new"` || messages.Variables["removed"] != nil || string(messages.Variables["extra"]) != "null" || len(deleted) != 1 || string(deleted["extra"]) != "2" || metadata["removed"] != nil || string(metadata["local"]) != "true" || top["custom"] == nil {
				t.Fatalf("nested customization/deletion merge: %s", readFile(path))
			}
			if !codexCatalogPending(path) {
				t.Fatal("disconnect would remove customized JSON")
			}
		})
	}
}

func TestCodexCatalogReordersOnlyWhenSourceOrderChanges(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "custom-models.json")
	writeFile(t, path, `{"models":[{"slug":"fake/b","priority":1},{"slug":"manual/only","priority":2},{"slug":"fake/a","priority":3}]}`)
	generated := `{"models":[{"slug":"fake/a","priority":1},{"slug":"fake/b","priority":2}]}`
	build := func() ([]byte, error) { return []byte(generated), nil }
	if err := writeCodexCatalog(path, build, true); err != nil {
		t.Fatal(err)
	}
	// An explicit Magpie order equal to its existing generated order
	// still replaces a different local order.
	s := settings.Load()
	s.OrderedModels = map[string][]string{"codex": {"fake/a", "fake/b"}}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexCatalog(path, build, true); err != nil {
		t.Fatal(err)
	}
	_, order, models, err := codexCatalogRead([]byte(readFile(path)))
	if err != nil || !slices.Equal(order, []string{"fake/a", "manual/only", "fake/b"}) {
		t.Fatalf("explicit order: %v", order)
	}
	for i, slug := range order {
		var priority int
		json.Unmarshal(models[slug]["priority"], &priority)
		if priority != i+1 {
			t.Fatalf("%s has priority %d at %d", slug, priority, i)
		}
	}
	// Native catalogs can reorder by priority without moving array rows.
	// A local-only row also keeps its effective slot when its priority
	// differs from its position in the array.
	writeFile(t, path, `{"models":[{"slug":"fake/a","priority":2},{"slug":"manual/only","priority":1},{"slug":"fake/b","priority":3}]}`)
	generated = `{"models":[{"slug":"fake/a","priority":2},{"slug":"fake/b","priority":1}]}`
	if err := writeCodexCatalog(path, build, true); err != nil {
		t.Fatal(err)
	}
	_, order, _, err = codexCatalogRead([]byte(readFile(path)))
	if err != nil || !slices.Equal(order, []string{"manual/only", "fake/b", "fake/a"}) {
		t.Fatalf("source priority order: %v", order)
	}
}

func TestCodexCatalogExplicitDefaultOverridesLocalValue(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "custom-models.json")
	writeFile(t, path, `{"models":[{"slug":"fake/m1","context_window":123000,"auto_compact_token_limit":91000,"custom":true}]}`)
	build := func() ([]byte, error) {
		return []byte(`{"models":[{"slug":"fake/m1","context_window":272000,"auto_compact_token_limit":272000}]}`), nil
	}
	if err := writeCodexCatalog(path, build, false); err != nil {
		t.Fatal(err)
	}
	p, _, _ := provider.Resolve("fake/m1")
	p.Contexts = map[string]int{"m1": 272000}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexCatalog(path, build, false); err != nil {
		t.Fatal(err)
	}
	_, _, models, err := codexCatalogRead([]byte(readFile(path)))
	if err != nil || string(models["fake/m1"]["context_window"]) != "272000" || string(models["fake/m1"]["auto_compact_token_limit"]) != "91000" || string(models["fake/m1"]["custom"]) != "true" {
		t.Fatalf("explicit default changed unrelated customization: %s", readFile(path))
	}
}

func TestCodexCatalogNativeAliasKeepsCustomization(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "custom-models.json")
	writeFile(t, path, `{"models":[{"slug":"gpt-test","priority":1,"base_instructions":"local prompt","custom":{"keep":true}}]}`)
	generated := `{"models":[{"slug":"gpt-test","priority":1,"base_instructions":"default prompt"}]}`
	build := func() ([]byte, error) { return []byte(generated), nil }
	for i, slug := range []string{"gpt-test", "codex/gpt-test", "gpt-test"} {
		prompt := "desktop default prompt"
		if i == 1 {
			prompt = "provider default prompt"
		}
		generated = `{"models":[{"slug":"` + slug + `","priority":1,"base_instructions":"` + prompt + `"}]}`
		if err := writeCodexCatalog(path, build, i == 1); err != nil {
			t.Fatal(err)
		}
		_, order, models, err := codexCatalogRead([]byte(readFile(path)))
		if err != nil || !slices.Equal(order, []string{slug}) || string(models[slug]["base_instructions"]) != `"local prompt"` || models[slug]["custom"] == nil {
			t.Fatalf("native alias lost local fields: %s", readFile(path))
		}
	}
}

func TestCodexCatalogBaselineFailureKeepsConcurrentSave(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "newer-save"}[concurrent], func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			path := filepath.Join(home, ".codex", "custom-models.json")
			generated := `{"models":[{"slug":"fake/m1","base_instructions":"old generated"}]}`
			build := func() ([]byte, error) { return []byte(generated), nil }
			if err := writeCodexCatalog(path, build, false); err != nil {
				t.Fatal(err)
			}
			before := readFile(path)
			baseline := codexBaselinePath(path)
			generated = `{"models":[{"slug":"fake/m1","base_instructions":"new generated"}]}`
			newer := `{"models":[{"slug":"fake/m1","base_instructions":"newer editor save"}]}`
			err := edit.Filtered(func(target string, data []byte) []byte {
				// edit resolves macOS's /var -> /private/var before calling
				// the filter; the path-hashed basename is unchanged.
				if filepath.Base(target) == filepath.Base(baseline) {
					if concurrent {
						writeFile(t, path, newer)
					}
					if err := os.Remove(baseline); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(baseline, 0o700); err != nil {
						t.Fatal(err)
					}
					writeFile(t, filepath.Join(baseline, "keep"), "force rename failure")
				}
				return data
			}, func() error { return writeCodexCatalog(path, build, false) })
			if err == nil {
				t.Fatal("baseline failure was ignored")
			}
			want := before
			if concurrent {
				want = newer
			}
			if got := readFile(path); got != want {
				t.Fatalf("failure overwrote catalog: got %s, want %s", got, want)
			}
		})
	}
}

func TestCodexCatalogGlobalDisableAndRestore(t *testing.T) {
	for _, action := range []string{"delete", "provider-off", "hidden", "disabled"} {
		t.Run(action, func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			if err := provider.Save(provider.Provider{ID: "other", Name: "Other", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1"}}); err != nil {
				t.Fatal(err)
			}
			visible := []string{"fake", "other"}
			if _, err := provider.SetModelLists(map[string]provider.ModelList{"codex": {Visible: &visible}}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, ".codex", "custom-models.json")
			writeFile(t, path, `{"custom":"top","models":[
				{"slug":"fake/m1","priority":30,"base_instructions":"local instruction","context_window":123000,"local":{"keep":null}},
				{"slug":"manual/only","priority":20,"custom":true},
				{"slug":"other/m1","priority":10} ]}`)
			build := func() ([]byte, error) { return codexcat.Catalog(magpieModels("codex")), nil }
			sync := func() {
				t.Helper()
				if err := writeCodexCatalog(path, build, true); err != nil {
					t.Fatal(err)
				}
			}
			sync()
			switch action {
			case "delete":
				writeFile(t, path, `{"custom":"top","models":[{"slug":"manual/only","priority":20,"custom":true},{"slug":"other/m1","priority":10}]}`)
			case "provider-off":
				if err := provider.SetOff("fake", true); err != nil {
					t.Fatal(err)
				}
			case "hidden":
				if err := provider.SetHiddenModels("codex", []string{"fake/m1"}); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := provider.DisableModels("fake/m1"); err != nil {
					t.Fatal(err)
				}
			}
			sync()
			_, _, models, err := codexCatalogRead([]byte(readFile(path)))
			if err != nil || models["fake/m1"] != nil {
				t.Fatal("inactive model remains in the catalog")
			}
			wantDisabled := action == "delete" || action == "disabled"
			if _, disabled := provider.IDDisabled("fake/m1"); disabled != wantDisabled {
				t.Fatalf("global disable = %v, want %v", disabled, wantDisabled)
			}
			if _, disabled := provider.IDDisabled("other/m1"); disabled {
				t.Fatal("disabled another provider's model with the same bare id")
			}
			if !codexCatalogPending(path) {
				t.Fatal("disconnect would discard archived local customizations")
			}
			// Several syncs while absent must neither forget the archive
			// nor turn a provider/agent visibility change into a global disable.
			sync()
			p, err := provider.Find("fake")
			if err != nil {
				t.Fatal(err)
			}
			if err := provider.SetContext(*p, "m1", 256000); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "delete", "disabled":
				if _, err := provider.EnableModels("fake/m1"); err != nil {
					t.Fatal(err)
				}
			case "provider-off":
				if err := provider.SetOff("fake", false); err != nil {
					t.Fatal(err)
				}
			case "hidden":
				if err := provider.SetHiddenModels("codex", nil); err != nil {
					t.Fatal(err)
				}
			}
			sync()
			sync()
			top, order, models, err := codexCatalogRead([]byte(readFile(path)))
			sort.SliceStable(order, func(i, j int) bool {
				var a, b int
				json.Unmarshal(models[order[i]]["priority"], &a)
				json.Unmarshal(models[order[j]]["priority"], &b)
				return a < b
			})
			if err != nil || !slices.Equal(order, []string{"other/m1", "manual/only", "fake/m1"}) {
				t.Fatalf("restored order: %v (%v)", order, err)
			}
			m := models["fake/m1"]
			if string(m["base_instructions"]) != `"local instruction"` || string(m["context_window"]) != "256000" || codexFieldHash(m["local"]) != codexFieldHash(json.RawMessage(`{"keep":null}`)) || string(top["custom"]) != `"top"` {
				t.Fatalf("restore lost local data or ignored a parameter changed while inactive: %s %s %s", m["base_instructions"], m["context_window"], m["local"])
			}
			if _, disabled := provider.IDDisabled("fake/m1"); disabled {
				t.Fatal("restored model was disabled again")
			}
		})
	}
}

func TestCodexCatalogReasoningReflectsInProvider(t *testing.T) {
	for _, value := range []string{`[{"effort":"high","description":"my high description","custom":true}]`, `[]`, `null`} {
		t.Run(value, func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			if err := provider.SetModelEfforts("fake/m1", []string{"medium", "high"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, ".codex", "custom-models.json")
			build := func() ([]byte, error) { return codexcat.Catalog(magpieModels("codex")), nil }
			if err := writeCodexCatalog(path, build, true); err != nil {
				t.Fatal(err)
			}
			top, _, models, err := codexCatalogRead([]byte(readFile(path)))
			if err != nil {
				t.Fatal(err)
			}
			models["fake/m1"]["supported_reasoning_levels"] = json.RawMessage(value)
			models["fake/m1"]["default_reasoning_level"] = json.RawMessage(`"medium"`)
			top["models"], _ = json.Marshal([]map[string]json.RawMessage{models["fake/m1"]})
			b, _ := json.Marshal(top)
			writeFile(t, path, string(b))
			// Model preference saves can synchronously invoke catalog sync.
			was := catalog.Changed
			catalog.Changed = SyncCatalog
			t.Cleanup(func() { catalog.Changed = was })
			if err := writeCodexCatalog(path, build, true); err != nil {
				t.Fatal(err)
			}
			if err := writeCodexCatalog(path, build, true); err != nil {
				t.Fatal(err)
			}
			p, err := provider.Find("fake")
			if err != nil {
				t.Fatal(err)
			}
			kept := p.ModelEfforts()["m1"] // The provider editor reads this map.
			if value == "null" || value == "[]" {
				if len(kept) != 0 || provider.Reasonless("fake", "m1") != (value == "[]") {
					t.Fatalf("reset/no-reasoning preference: %v", kept)
				}
				return
			}
			if !slices.Equal(kept, []string{"high"}) {
				t.Fatalf("provider editor selection: %v, want high only", kept)
			}
			_, _, models, err = codexCatalogRead([]byte(readFile(path)))
			if err != nil {
				t.Fatal(err)
			}
			m := models["fake/m1"]
			if codexFieldHash(m["supported_reasoning_levels"]) != codexFieldHash(json.RawMessage(value)) || string(m["default_reasoning_level"]) != `"high"` {
				t.Fatalf("local reasoning metadata/default: %s / %s", m["supported_reasoning_levels"], m["default_reasoning_level"])
			}
			// An explicit Magpie edit wins when both sides changed the
			// same model's levels since the last successful sync.
			m["supported_reasoning_levels"] = json.RawMessage(`[{"effort":"low"}]`)
			top["models"], _ = json.Marshal([]map[string]json.RawMessage{m})
			b, _ = json.Marshal(top)
			writeFile(t, path, string(b))
			catalog.Changed = was
			if err := provider.SetModelEfforts("fake/m1", []string{"medium"}); err != nil {
				t.Fatal(err)
			}
			if err := writeCodexCatalog(path, build, true); err != nil {
				t.Fatal(err)
			}
			p, _ = provider.Find("fake")
			if !slices.Equal(p.ModelEfforts()["m1"], []string{"medium"}) {
				t.Fatal("local edit replaced the newer Magpie reasoning preference")
			}
		})
	}
}

func TestCodexCatalogInvalidReasoningDoesNotDisable(t *testing.T) {
	home, _ := codexHome(t, "", "")
	p, _ := provider.Find("fake")
	p.Models = []string{"m1", "m2"}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "custom-models.json")
	build := func() ([]byte, error) { return codexcat.Catalog(magpieModels("codex")), nil }
	if err := writeCodexCatalog(path, build, true); err != nil {
		t.Fatal(err)
	}
	before := readFile(codexBaselinePath(path))
	edited := `{"models":[{"slug":"fake/m2","supported_reasoning_levels":[{"effort":"not-a-level"}]}]}`
	writeFile(t, path, edited)
	if err := writeCodexCatalog(path, build, true); err == nil {
		t.Fatal("invalid reasoning accepted")
	}
	if _, disabled := provider.IDDisabled("fake/m1"); disabled {
		t.Fatal("invalid batch partially disabled another model")
	}
	if readFile(path) != edited || readFile(codexBaselinePath(path)) != before {
		t.Fatal("invalid batch replaced local catalog or baseline")
	}
}

func TestCodexCatalogV2AdoptionDoesNotInferGlobalEdits(t *testing.T) {
	home, _ := codexHome(t, "", "")
	p, _ := provider.Find("fake")
	p.Models = []string{"m1", "m2"}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".codex", "custom-models.json")
	build := func() ([]byte, error) { return codexcat.Catalog(magpieModels("codex")), nil }
	if err := writeCodexCatalog(path, build, true); err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFile(codexBaselinePath(path))), &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["version"] = json.RawMessage("2")
	delete(legacy, "last")
	delete(legacy, "archive")
	delete(legacy, "slots")
	b, _ := json.Marshal(legacy)
	writeFile(t, codexBaselinePath(path), string(b))
	writeFile(t, path, `{"models":[{"slug":"fake/m2","supported_reasoning_levels":[{"effort":"high"}],"custom":"keep"}]}`)
	for range 2 {
		if err := writeCodexCatalog(path, build, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, disabled := provider.IDDisabled("fake/m1"); disabled {
		t.Fatal("migration treated an unknown historical omission as a global disable")
	}
	p, _ = provider.Find("fake")
	if len(p.ModelEfforts()["m2"]) != 0 {
		t.Fatal("migration guessed that a pre-existing reasoning value was a new global edit")
	}
	if !strings.Contains(readFile(path), `"custom": "keep"`) && !strings.Contains(readFile(path), `"custom":"keep"`) {
		t.Fatal("migration discarded local customization")
	}
}

func TestCodexCatalogArchivedCustomizationKeepsFile(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "magpie-models.json")
	writeFile(t, path, `{"models":[{"slug":"fake/m1","base_instructions":"keep when disabled"}]}`)
	build := func() ([]byte, error) { return codexcat.Catalog(magpieModels("codex")), nil }
	if err := writeCodexCatalog(path, build, true); err != nil {
		t.Fatal(err)
	}
	if err := provider.DisableModels("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexCatalog(path, build, true); err != nil {
		t.Fatal(err)
	}
	if !codexCatalogPending(path) {
		t.Fatal("an empty active list hid the customized model waiting to return")
	}
}

// An imported high-only selection stays after a source sync that rewords its
// reasoning descriptions only: generation adds a synthetic Ultra for such a
// model, and the exact local selection is what comes back, default level
// included. A real upstream effort change, or a Magpie setting change, still
// applies as before.
func TestCodexCatalogKeepsImportedLevelsWhenSourceRewordsThem(t *testing.T) {
	home, _ := codexHome(t, "", "")
	path := filepath.Join(home, ".codex", "custom-models.json")
	reworded := false
	build := func() ([]byte, error) {
		b := codexcat.Catalog(magpieModels("codex"))
		if !reworded {
			return b, nil
		}
		var cat map[string]any
		if err := json.Unmarshal(b, &cat); err != nil {
			return nil, err
		}
		for _, v := range cat["models"].([]any) {
			m := v.(map[string]any)
			if m["slug"] != "fake/m1" {
				continue
			}
			for _, l := range m["supported_reasoning_levels"].([]any) {
				level := l.(map[string]any)
				level["description"] = "source words for " + level["effort"].(string)
			}
		}
		return json.Marshal(cat)
	}
	sync := func() {
		t.Helper()
		if err := writeCodexCatalog(path, build, true); err != nil {
			t.Fatal(err)
		}
	}
	levels := func() []map[string]any {
		t.Helper()
		_, _, models, err := codexCatalogRead([]byte(readFile(path)))
		if err != nil {
			t.Fatal(err)
		}
		var got []map[string]any
		if err := json.Unmarshal(models["fake/m1"]["supported_reasoning_levels"], &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	sync()
	// What an import leaves: high alone, with the user's own description.
	var cat map[string]any
	if err := json.Unmarshal([]byte(readFile(path)), &cat); err != nil {
		t.Fatal(err)
	}
	for _, v := range cat["models"].([]any) {
		m := v.(map[string]any)
		if m["slug"] == "fake/m1" {
			m["supported_reasoning_levels"] = []map[string]any{{"effort": "high", "description": "my high description", "custom": true}}
			m["default_reasoning_level"] = "high"
		}
	}
	b, _ := json.Marshal(cat)
	writeFile(t, path, string(b))
	sync() // the import shares high with Magpie
	if got, want := settings.Load().ModelEfforts["fake/m1"], []string{"high"}; !slices.Equal(got, want) {
		t.Fatalf("imported selection: %v, want %v", got, want)
	}
	sync() // a second sync records the resulting local selection in Last
	if got := levels(); len(got) != 1 || got[0]["effort"] != "high" {
		t.Fatalf("levels after the import: %v", got)
	}
	// The source keeps the same efforts and rewords their descriptions;
	// generation offers high with a synthetic Ultra beside it.
	reworded = true
	sync()
	var was codexCatalogBaseline
	if err := json.Unmarshal([]byte(readFile(codexBaselinePath(path))), &was); err != nil {
		t.Fatal(err)
	}
	if was.Models["fake/m1"].Reasoning != `["high","ultra"]` {
		t.Fatalf("the source should offer a synthetic Ultra: %q", was.Models["fake/m1"].Reasoning)
	}
	got := levels()
	if len(got) != 1 || got[0]["effort"] != "high" || got[0]["description"] != "my high description" || got[0]["custom"] != true {
		t.Fatalf("a description-only source change replaced the imported selection: %v", got)
	}
	_, _, models, err := codexCatalogRead([]byte(readFile(path)))
	if err != nil || string(models["fake/m1"]["default_reasoning_level"]) != `"high"` {
		t.Fatalf("default level after the reword: %s", models["fake/m1"]["default_reasoning_level"])
	}
}
