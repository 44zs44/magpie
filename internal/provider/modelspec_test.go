package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// specHome is a sandbox with a custom relay whose models no list knows
// (Dani Ariyanto on Discord: kenari/*:free).
func specHome(t *testing.T) {
	t.Helper()
	limitsHome(t)
	if err := Save(Provider{ID: "kenari", Name: "Kenari", Chat: "http://127.0.0.1:1/v1", Key: "k",
		Models: []string{"glm-5:free", "qwen-x:free"}}); err != nil {
		t.Fatal(err)
	}
}

// specFiles are the bytes of the two files a spec is kept in.
func specFiles(t *testing.T) string {
	t.Helper()
	var out string
	for _, f := range []string{"settings.json", "providers.json"} {
		b, _ := os.ReadFile(filepath.Join(settings.Dir(), f))
		out += f + ":" + string(b) + "\n"
	}
	return out
}

// A custom provider's model with nothing known takes a whole spec at once,
// the catalog entry every reader goes by carries it, `magpie model specs`
// lists it, and a reset leaves the model as it was before, unknown on
// every part. The agents are told once for a change that writes both files.
func TestModelSpecSetListReset(t *testing.T) {
	specHome(t)
	e, ok := EntryOf("kenari/glm-5:free")
	if !ok {
		t.Fatal("kenari/glm-5:free is not in the catalog")
	}
	if e.Context != 0 || e.Output != 0 || e.Images || e.ImageInput != nil || len(e.Efforts) != 0 {
		t.Fatalf("before: a model nothing knows has %+v", e)
	}
	told := 0
	catalog.Changed = func() { told++ }
	t.Cleanup(func() { catalog.Changed = nil })

	if err := SetModelSpec("kenari/glm-5:free", SpecChange{
		Context: new(131072), Output: new(16384), Vision: new("on"), Reasoning: new("low-high"),
	}); err != nil {
		t.Fatal(err)
	}
	if told != 1 {
		t.Errorf("the agents were told %d times; want once", told)
	}
	e, _ = EntryOf("kenari/glm-5:free")
	if e.Context != 131072 || e.Output != 16384 || !e.Images || e.ImageInput == nil || !*e.ImageInput {
		t.Errorf("entry after set: context %d output %d images %v", e.Context, e.Output, e.Images)
	}
	if !slices.Equal(e.Efforts, []string{"low", "medium", "high"}) || !e.Reasoning {
		t.Errorf("levels after set: %v reasoning %v", e.Efforts, e.Reasoning)
	}
	// the other model of the provider is left as it was
	if o, _ := EntryOf("kenari/qwen-x:free"); o.Context != 0 || o.Images || len(o.Efforts) != 0 {
		t.Errorf("the model given nothing changed: %+v", o)
	}
	rows := ModelSpecs()
	if len(rows) != 1 || rows[0] != (SpecRow{Key: "kenari/glm-5:free", Context: "131072", Output: "16384", Vision: "on", Reasoning: "low,medium,high"}) {
		t.Errorf("specs: %+v", rows)
	}
	sp, ok := SpecOf("kenari/glm-5:free")
	if !ok || sp.Context.From != "model" || sp.Vision.From != "model" || sp.Reasoning.Value != "low,medium,high" {
		t.Errorf("spec: %+v", sp)
	}

	// one part back to auto leaves the others
	if err := SetModelSpec("kenari/glm-5:free", SpecChange{Vision: new("auto")}); err != nil {
		t.Fatal(err)
	}
	if e, _ = EntryOf("kenari/glm-5:free"); e.Images || e.ImageInput != nil || e.Context != 131072 {
		t.Errorf("vision auto: images %v %v context %d", e.Images, e.ImageInput, e.Context)
	}

	gone, err := ResetModelSpec("kenari/glm-5:free")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gone, []string{"context", "output", "reasoning"}) {
		t.Errorf("reset took away %v", gone)
	}
	e, _ = EntryOf("kenari/glm-5:free")
	if e.Context != 0 || e.Output != 0 || e.Images || len(e.Efforts) != 0 || e.Reasoning {
		t.Errorf("after reset: %+v", e)
	}
	if rows := ModelSpecs(); len(rows) != 0 {
		t.Errorf("specs after reset: %+v", rows)
	}
	if gone, err := ResetModelSpec("kenari/glm-5:free"); err != nil || len(gone) != 0 {
		t.Errorf("a second reset: %v %v", gone, err)
	}
}

// A bad part — a level the model hasn't, a level that is no level, a range
// that runs backwards, a model the provider doesn't serve, vision for a
// whole provider, a model from any provider — is refused, and nothing of
// the change is written, not even its good parts.
func TestModelSpecRefusalWritesNothing(t *testing.T) {
	specHome(t)
	if err := SetModelSpec("kenari/glm-5:free", SpecChange{Context: new(65536)}); err != nil {
		t.Fatal(err)
	}
	before := specFiles(t)
	for name, c := range map[string]struct {
		ref string
		c   SpecChange
	}{
		"not a level":      {"kenari/glm-5:free", SpecChange{Context: new(1 << 20), Reasoning: new("turbo")}},
		"backwards range":  {"kenari/glm-5:free", SpecChange{Output: new(4096), Reasoning: new("high-low")}},
		"bad vision":       {"kenari/glm-5:free", SpecChange{Context: new(1 << 20), Vision: new("maybe")}},
		"unserved model":   {"kenari/nope", SpecChange{Context: new(1 << 20)}},
		"provider vision":  {"kenari/*", SpecChange{Context: new(1 << 20), Vision: new("on")}},
		"provider levels":  {"kenari/*", SpecChange{Reasoning: new("low")}},
		"any provider":     {"*/glm-5:free", SpecChange{Context: new(1 << 20)}},
		"negative":         {"kenari/glm-5:free", SpecChange{Output: new(-1), Vision: new("on")}},
		"nothing said":     {"kenari/glm-5:free", SpecChange{}},
		"no such provider": {"nobody/glm-5:free", SpecChange{Context: new(1 << 20)}},
	} {
		if err := SetModelSpec(c.ref, c.c); err == nil {
			t.Errorf("%s: %s %+v was taken", name, c.ref, c.c)
		}
		if got := specFiles(t); got != before {
			t.Fatalf("%s: a refused spec wrote:\nbefore %s\nafter  %s", name, before, got)
		}
	}
}

func TestCodexCatalogEditsPersistInModelSettings(t *testing.T) {
	specHome(t)
	const ref = "kenari/glm-5:free"
	edits := map[string]map[string]json.RawMessage{ref: {
		"base_instructions":          json.RawMessage(`"  Custom Codex instructions.\n"`),
		"context_window":             json.RawMessage(`131072`),
		"auto_compact_token_limit":   json.RawMessage(`100000`),
		"supported_reasoning_levels": json.RawMessage(`[{"effort":"low","description":"Less"},{"effort":"high","description":"More"}]`),
		"input_modalities":           json.RawMessage(`["text","image"]`),
	}}
	if err := ApplyCodexModelEdits(edits); err != nil {
		t.Fatal(err)
	}
	e, ok := EntryOf(ref)
	if !ok || e.Context != 131072 || !e.Images || !slices.Equal(e.Efforts, []string{"low", "high"}) {
		t.Fatalf("the gateway's model did not take the catalog edit: %+v", e)
	}
	s := settings.Load()
	if s.ModelCompacts[ref] != 100000 || s.ModelCodexInstructions[ref] != "  Custom Codex instructions.\n" {
		t.Fatalf("compact/prompt were not persisted verbatim: %d %q", s.ModelCompacts[ref], s.ModelCodexInstructions[ref])
	}
	state, err := CodexModelEditState(ref)
	if err != nil || len(state) != 5 {
		t.Fatalf("explicit state: %v %v", state, err)
	}
	other, err := CodexModelEditState("kenari/qwen-x:free")
	if err != nil || len(other) != 0 {
		t.Fatalf("unedited sibling has settings: %v %v", other, err)
	}
	before := specFiles(t)
	told := 0
	catalog.Changed = func() { told++ }
	t.Cleanup(func() { catalog.Changed = nil })
	if err := ApplyCodexModelEdits(edits); err != nil {
		t.Fatal(err)
	}
	if told != 0 || specFiles(t) != before {
		t.Fatal("retrying the same import rewrote configuration or notified agents")
	}
	for field := range edits[ref] {
		edits[ref][field] = json.RawMessage(`null`)
	}
	if err := ApplyCodexModelEdits(edits); err != nil {
		t.Fatal(err)
	}
	state, err = CodexModelEditState(ref)
	if err != nil || len(state) != 0 {
		t.Fatalf("cleared overrides remain: %v %v", state, err)
	}
	e, _ = EntryOf(ref)
	if e.Context != 0 || e.Images || len(e.Efforts) != 0 {
		t.Fatalf("removing overrides did not restore vendor defaults: %+v", e)
	}
}

func TestCodexCatalogEditsRefuseWholeInvalidImport(t *testing.T) {
	specHome(t)
	before := specFiles(t)
	for _, fields := range []map[string]json.RawMessage{
		{"context_window": json.RawMessage(`-1`)},
		{"context_window": json.RawMessage(`1.5`)},
		{"context_window": json.RawMessage(`"131072"`)},
		{"auto_compact_token_limit": json.RawMessage(`false`)},
		{"base_instructions": json.RawMessage(`{"text":"bad"}`)},
		{"base_instructions": json.RawMessage(`"NUL\u0000"`)},
		{"supported_reasoning_levels": json.RawMessage(`[{"effort":"turbo"}]`)},
		{"supported_reasoning_levels": json.RawMessage(`[{}]`)},
		{"input_modalities": json.RawMessage(`["text","audio"]`)},
		{"input_modalities": json.RawMessage(`[]`)},
		{"slug": json.RawMessage(`"other/model"`)},
	} {
		edits := map[string]map[string]json.RawMessage{
			"kenari/glm-5:free":  {"context_window": json.RawMessage(`131072`), "base_instructions": json.RawMessage(`"Valid"`)},
			"kenari/qwen-x:free": fields,
		}
		if err := ApplyCodexModelEdits(edits); err == nil {
			t.Errorf("invalid fields accepted: %v", fields)
		}
		if specFiles(t) != before {
			t.Fatalf("invalid import wrote configuration: %v", fields)
		}
	}
	if err := ApplyCodexModelEdits(map[string]map[string]json.RawMessage{"kenari/no-such-model": {"context_window": json.RawMessage(`10000`)}}); err == nil {
		t.Fatal("unknown model accepted")
	}
}

func TestCodexCatalogEditsGroupLeavesMembersAlone(t *testing.T) {
	specHome(t)
	if err := SaveGroup(Group{ID: "my-models", Name: "My models", Members: []string{"kenari/glm-5:free", "kenari/qwen-x:free"}}); err != nil {
		t.Fatal(err)
	}
	edits := map[string]map[string]json.RawMessage{"group/my-models": {
		"context_window": json.RawMessage(`131072`), "auto_compact_token_limit": json.RawMessage(`100000`),
		"supported_reasoning_levels": json.RawMessage(`[{"effort":"low"},{"effort":"high"}]`),
		"base_instructions":          json.RawMessage(`"Group instructions"`),
	}}
	if err := ApplyCodexModelEdits(edits); err != nil {
		t.Fatal(err)
	}
	g, _, ok := FindGroup("group/my-models")
	if !ok || g.Context != 131072 || g.Compact != 100000 || !slices.Equal(g.Levels, []string{"low", "high"}) {
		t.Fatalf("group settings: %+v", g)
	}
	state, err := CodexModelEditState("group/my-models")
	if err != nil || len(state) != 4 {
		t.Fatalf("group explicit state: %v %v", state, err)
	}
	for _, ref := range g.Members {
		member, err := CodexModelEditState(ref)
		if err != nil || !reflect.DeepEqual(member, map[string]json.RawMessage{}) {
			t.Fatalf("a group import changed its member %s: %v %v", ref, member, err)
		}
	}
	before := specFiles(t)
	if err := ApplyCodexModelEdits(map[string]map[string]json.RawMessage{"group/my-models": {"input_modalities": json.RawMessage(`["text"]`)}}); err == nil {
		t.Fatal("group image support accepted without a group setting")
	}
	if specFiles(t) != before {
		t.Fatal("refused group edit changed configuration")
	}
}

func TestCodexCatalogEditsRetryAfterProviderWriteFails(t *testing.T) {
	specHome(t)
	const ref = "kenari/glm-5:free"
	backup := Path() + ".bak"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	// The provider file remains readable, but its mandatory backup cannot
	// be replaced. This fails after the independent settings save.
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	edits := map[string]map[string]json.RawMessage{ref: {
		"context_window": json.RawMessage(`131072`), "auto_compact_token_limit": json.RawMessage(`100000`),
		"input_modalities": json.RawMessage(`["text","image"]`),
	}}
	if err := ApplyCodexModelEdits(edits); err == nil {
		t.Fatal("provider storage failure was hidden")
	}
	s := settings.Load()
	p, err := Find("kenari")
	if err != nil || p.Contexts["glm-5:free"] != 0 || s.ModelCompacts[ref] != 100000 || !s.ModelImages[ref] {
		t.Fatalf("unexpected partial import: provider %v, settings %v, error %v", p.Contexts, s.ModelCompacts, err)
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexModelEdits(edits); err != nil {
		t.Fatal(err)
	}
	e, ok := EntryOf(ref)
	if !ok || e.Context != 131072 || !e.Images {
		t.Fatalf("retry did not complete the import: %+v", e)
	}
	before := specFiles(t)
	if err := ApplyCodexModelEdits(edits); err != nil || specFiles(t) != before {
		t.Fatalf("completed import was not idempotent: %v", err)
	}
}

func TestCodexCatalogEditsNeedReadableOriginalsBeforeSaving(t *testing.T) {
	specHome(t)
	cache := catalog.CodexModelsCache()
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(`{"models":`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := specFiles(t)
	for field, raw := range map[string]json.RawMessage{
		"context_window":             json.RawMessage(`131072`),
		"auto_compact_token_limit":   json.RawMessage(`100000`),
		"supported_reasoning_levels": json.RawMessage(`[{"effort":"low"}]`),
		"input_modalities":           json.RawMessage(`["text","image"]`),
	} {
		if err := ApplyCodexModelEdits(map[string]map[string]json.RawMessage{"kenari/glm-5:free": {field: raw}}); err == nil {
			t.Errorf("%s saved without preserving the unreadable original catalog", field)
		}
		if specFiles(t) != before {
			t.Fatalf("%s changed configuration despite unreadable originals", field)
		}
	}
}

// A model's own window and reply limit win over the ones given every model
// of its provider ("kenari/*"), which a model with none of its own takes;
// a reset of the model leaves it the provider's.
func TestModelSpecProviderWildcard(t *testing.T) {
	specHome(t)
	if err := SetModelSpec("kenari/*", SpecChange{Context: new(65536), Output: new(8192)}); err != nil {
		t.Fatal(err)
	}
	if err := SetModelSpec("kenari/glm-5:free", SpecChange{Context: new(131072), Output: new(16384)}); err != nil {
		t.Fatal(err)
	}
	if e, _ := EntryOf("kenari/glm-5:free"); e.Context != 131072 || e.Output != 16384 {
		t.Errorf("the model's own: %d %d", e.Context, e.Output)
	}
	if e, _ := EntryOf("kenari/qwen-x:free"); e.Context != 65536 || e.Output != 8192 {
		t.Errorf("the provider's: %d %d", e.Context, e.Output)
	}
	if sp, _ := SpecOf("kenari/qwen-x:free"); sp.Context.From != "provider" || sp.Output.From != "provider" {
		t.Errorf("spec from: %+v", sp)
	}
	if _, err := ResetModelSpec("kenari/glm-5:free"); err != nil {
		t.Fatal(err)
	}
	if e, _ := EntryOf("kenari/glm-5:free"); e.Context != 65536 || e.Output != 8192 {
		t.Errorf("after the model's reset: %d %d", e.Context, e.Output)
	}
	if gone, err := ResetModelSpec("kenari/*"); err != nil || !slices.Equal(gone, []string{"context", "output"}) {
		t.Errorf("provider reset: %v %v", gone, err)
	}
	if e, _ := EntryOf("kenari/qwen-x:free"); e.Context != 0 || e.Output != 0 {
		t.Errorf("after the provider's reset: %d %d", e.Context, e.Output)
	}
}

// What the user set wins over what the vendor's own list says — a plugin
// listing a 32K window, images and reasoning — and survives the list being
// fetched again with other numbers. A model with no spec keeps its list's.
func TestModelSpecWinsOverTheListAndASync(t *testing.T) {
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", "")
	catalog.Reset()
	t.Cleanup(catalog.Reset)
	spec := "@magpie-community/opencode-x-auth"
	write := func(name string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		os.MkdirAll(settings.Dir(), 0o700)
		if err := os.WriteFile(filepath.Join(settings.Dir(), name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("plugins.json", map[string]any{"plugins": []map[string]any{{"spec": spec}}})
	write("plugin-auth.json", map[string]map[string]any{"xp": {"type": "api", "key": "sk-1234"}})
	list := func(ctx int) {
		plugin.UseCached([]plugin.Provider{{ID: "xp", Spec: spec, Name: "XP",
			Methods:  []plugin.Method{{Type: "api", Label: "API key"}},
			Accounts: []plugin.Account{{Key: "xp", Type: "api"}},
			Models: []plugin.Model{
				{ID: "deepseek-v4.1-flash-uncensored", Name: "DS", Context: ctx, Output: 8192, Image: true, ImageSaid: true, Reasoning: true},
				{ID: "other", Name: "Other", Context: ctx, Output: 8192, Image: true, ImageSaid: true},
			}}})
	}
	list(32768)
	t.Cleanup(func() { plugin.UseCached(nil) })
	if e, ok := EntryOf("xp/deepseek-v4.1-flash-uncensored"); !ok || e.Context != 32768 || !e.Images {
		t.Fatalf("the list's: %+v %v", e, ok)
	}
	if err := SetModelSpec("xp/deepseek-v4.1-flash-uncensored", SpecChange{
		Context: new(1 << 20), Output: new(65536), Vision: new("off"), Reasoning: new("off"),
	}); err != nil {
		t.Fatal(err)
	}
	check := func(when string, listed int) {
		t.Helper()
		e, _ := EntryOf("xp/deepseek-v4.1-flash-uncensored")
		if e.Context != 1<<20 || e.Output != 65536 || e.Images || e.ImageInput == nil || *e.ImageInput || !e.Reasonless || e.Reasoning {
			t.Errorf("%s: context %d output %d images %v reasonless %v reasoning %v", when, e.Context, e.Output, e.Images, e.Reasonless, e.Reasoning)
		}
		if o, _ := EntryOf("xp/other"); o.Context != listed || o.Output != 8192 || !o.Images {
			t.Errorf("%s: the model with no spec: context %d output %d images %v", when, o.Context, o.Output, o.Images)
		}
	}
	check("set", 32768)
	list(200000) // fetched again: the vendor says another window
	check("after the list is fetched again", 200000)
}
