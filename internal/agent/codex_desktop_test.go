package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
)

// codexAppReads has the Codex app (and a CLI) read every config when app
// is true, the CLI alone when it is false; put back when the test ends.
func codexAppReads(t *testing.T, app *bool) {
	t.Helper()
	was := codexReaders
	t.Cleanup(func() { codexReaders = was })
	codexReaders = func(place) []CodexReader {
		out := []CodexReader{{Kind: "cli", Version: "0.163.0"}}
		if *app {
			out = append(out, CodexReader{Kind: "app", Version: "0.163.0"})
		}
		return out
	}
}

// deskListIs has CodexDesktopList answer list, or not know it while list
// is nil.
func deskListIs(t *testing.T, list *string) {
	t.Helper()
	was := CodexDesktopList
	t.Cleanup(func() { CodexDesktopList = was })
	CodexDesktopList = func() ([]byte, bool) {
		if list == nil || *list == "" {
			return nil, false
		}
		return []byte(*list), true
	}
}

// laddieyao on Discord: Codex joined beside its ChatGPT sign-in, the Codex
// app's picker showed GPT alone while the CLI listed magpie's models. The
// app shows only the models of Codex's list a flag of OpenAI's names,
// unless Codex reads its list from a file: where the app is installed,
// magpie writes the list it hands Codex there and names it; the CLI alone
// keeps the list the gateway hands it.
func TestCodexDesktopListBesideSignIn(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, `model = "gpt-5.5"`+"\n")
	dir := filepath.Join(home, ".codex")
	desk := filepath.Join(dir, "magpie-desktop-models.json")
	app := true
	codexAppReads(t, &app)
	list := `{"models":[{"slug":"gpt-6-sol","base_instructions":"theirs"},{"slug":"fake/m1"}]}`
	deskListIs(t, &list)
	models := func() map[string]map[string]any {
		t.Helper()
		var cat struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal([]byte(readFile(desk)), &cat); err != nil {
			t.Fatalf("decode desktop catalog: %v", err)
		}
		out := make(map[string]map[string]any, len(cat.Models))
		for _, model := range cat.Models {
			if slug, _ := model["slug"].(string); slug != "" {
				out[slug] = model
			}
		}
		return out
	}

	cx := codex(home)
	if ok, err := cx.Join(); err != nil || !ok {
		t.Fatalf("join: %v %v", ok, err)
	}
	cfg := read()
	if !strings.Contains(cfg, `model_catalog_json = "`) || !strings.Contains(cfg, "magpie-desktop-models.json") {
		t.Fatalf("joined with the Codex app here, no list named for it:\n%s", cfg)
	}
	if strings.Contains(cfg, "model_provider =") {
		t.Fatalf("Codex's provider changed:\n%s", cfg)
	}
	initial := models()
	if initial["gpt-6-sol"]["base_instructions"] != "theirs" || initial["fake/m1"] == nil {
		t.Fatalf("the app's list lost its models or instructions: %#v", initial)
	}
	// connected beside the sign-in, as before: not magpie as its provider
	if !cx.Joined() || cx.AppOut() {
		t.Fatalf("joined %v, app out %v\n%s", cx.Joined(), cx.AppOut(), cfg)
	}
	if msg := cx.Check(); msg != "" {
		t.Fatalf("check: %s", msg)
	}

	// magpie's models changed: written again at the next sync
	list = `{"models":[{"slug":"gpt-6-sol","base_instructions":"theirs"},{"slug":"fake/m2"}]}`
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	updated := models()
	if updated["gpt-6-sol"]["base_instructions"] != "theirs" || updated["fake/m2"] == nil || updated["fake/m1"] != nil {
		t.Fatalf("model update changed the wrong entries: %#v", updated)
	}

	// a list not known now (the backend not answering) is unknown: the
	// file as it was stays, never an empty one
	was, err := os.ReadFile(desk)
	if err != nil {
		t.Fatal(err)
	}
	list = ""
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(desk); string(b) != string(was) || !strings.Contains(read(), "magpie-desktop-models.json") {
		t.Fatalf("unknown list: %s\n%s", b, read())
	}

	// a magpie model picked beside the sign-in: still Codex's provider,
	// the list still the app's
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !cx.Beside() || !strings.Contains(cfg, "magpie-desktop-models.json") || strings.Contains(cfg, "model_provider =") {
		t.Fatalf("beside: %v\n%s", cx.Beside(), cfg)
	}

	// magpie API keeps the same local catalog while changing its source.
	if err := cx.Field("login").Set("api"); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !strings.Contains(cfg, `model_provider = "magpie"`) || !strings.Contains(cfg, "magpie-desktop-models.json") {
		t.Fatalf("api:\n%s", cfg)
	}
	if _, err := os.Stat(desk); err != nil {
		t.Fatalf("api: the local catalog is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "magpie-models.json")); !os.IsNotExist(err) {
		t.Fatalf("api: wrote a second catalog: %v", err)
	}
	if err := cx.Field("login").Set(""); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !strings.Contains(cfg, "magpie-desktop-models.json") || strings.Contains(cfg, "model_provider =") {
		t.Fatalf("back beside the sign-in:\n%s", cfg)
	}

	// Disconnect keeps the local catalog and its reference for reconnect.
	kept := readFile(desk)
	ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json")
	if err := cx.Unwire(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); strings.Contains(cfg, "model_provider =") || strings.Contains(cfg, "openai_base_url") {
		t.Fatalf("unwired:\n%s", cfg)
	}
	if got, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); got != ref || readFile(desk) != kept {
		t.Fatal("unwired: the local catalog or its path changed")
	}
}

// The CLI alone asks the gateway for its list, which follows each change
// as it is made: no file is created for it. A catalog already written while
// the Codex app was installed stays, including its local customizations.
func TestCodexDesktopListCLIOnly(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, `model = "gpt-5.5"`+"\n")
	desk := filepath.Join(home, ".codex", "magpie-desktop-models.json")
	app := false
	codexAppReads(t, &app)
	list := `{"local_note":"kept","models":[{"slug":"gpt-6-sol"}]}`
	deskListIs(t, &list)
	cx := codex(home)
	if ok, err := cx.Join(); err != nil || !ok {
		t.Fatalf("join: %v %v", ok, err)
	}
	if cfg := read(); strings.Contains(cfg, "model_catalog_json") {
		t.Fatalf("CLI alone:\n%s", cfg)
	}
	if _, err := os.Stat(desk); !os.IsNotExist(err) {
		t.Fatalf("CLI alone: a list written: %v", err)
	}
	app = true
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !strings.Contains(cfg, "magpie-desktop-models.json") {
		t.Fatalf("the app installed:\n%s", cfg)
	}
	ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json")
	app = false
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); got != ref {
		t.Fatalf("the app gone: catalog path changed to %q", got)
	}
	if got := readFile(desk); !strings.Contains(got, `"local_note": "kept"`) {
		t.Fatalf("the app gone: local catalog content lost:\n%s", got)
	}
}

// A model_catalog_json the user set while joined is theirs: it stands, and
// no list of magpie's is written beside it. And a key naming magpie's list
// with the file gone (removed by hand) goes, as Codex loads no config then,
// while the list isn't known to write it again.
func TestCodexDesktopListKeepsTheirs(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, `model = "gpt-5.5"`+"\n")
	dir := filepath.Join(home, ".codex")
	desk := filepath.Join(dir, "magpie-desktop-models.json")
	app := true
	codexAppReads(t, &app)
	list := `{"models":[{"slug":"gpt-6-sol"}]}`
	deskListIs(t, &list)
	cx := codex(home)
	if ok, err := cx.Join(); err != nil || !ok {
		t.Fatalf("join: %v %v", ok, err)
	}
	os.Remove(desk)
	list = ""
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); strings.Contains(cfg, "model_catalog_json") {
		t.Fatalf("named with the file gone:\n%s", cfg)
	}
	list = `{"models":[{"slug":"gpt-6-sol"}]}`
	cfg := strings.Replace(read(), `model = "gpt-5.5"`, `model = "gpt-5.5"`+"\n"+`model_catalog_json = "mine.json"`, 1)
	os.WriteFile(filepath.Join(dir, "mine.json"), []byte(`{"models":[]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o644)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !strings.Contains(cfg, `model_catalog_json = "mine.json"`) {
		t.Fatalf("theirs:\n%s", cfg)
	}
	if _, err := os.Stat(desk); !os.IsNotExist(err) {
		t.Fatalf("written beside theirs: %v", err)
	}
}

// not_4O4 on X: the Windows app listed GPT alone, in a new window and a
// new project too. Its app-server reads the list only as it starts, so
// magpie keeps when it named the list (DeskAt), for one running from
// before to be restarted: the time it was named, kept while the list is
// unknown or its file rewritten, zero once the key is gone, and new when
// it is named again.
func TestCodexDesktopListNamedAt(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, `model = "gpt-5.5"`+"\n")
	app := true
	codexAppReads(t, &app)
	// not known at the Connect: nothing named yet, and no time
	var list string
	deskListIs(t, &list)
	cx := codex(home)
	if ok, err := cx.Join(); err != nil || !ok {
		t.Fatalf("join: %v %v", ok, err)
	}
	if strings.Contains(read(), "model_catalog_json") || !cx.DeskAt().IsZero() {
		t.Fatalf("named before the list is known: %v\n%s", cx.DeskAt(), read())
	}
	// known a sync later, after the app-server started
	list = `{"models":[{"slug":"gpt-6-sol"},{"slug":"fake/m1"}]}`
	before := time.Now()
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	at := cx.DeskAt()
	if !strings.Contains(read(), "magpie-desktop-models.json") || at.Before(before) || at.After(time.Now()) {
		t.Fatalf("named at %v, want after %v\n%s", at, before, read())
	}
	// a new list in the file is no new naming: the app's next launch reads it
	list = `{"models":[{"slug":"gpt-6-sol"},{"slug":"fake/m2"}]}`
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if !cx.DeskAt().Equal(at) {
		t.Fatalf("named at %v after a new list, want %v", cx.DeskAt(), at)
	}
	// The catalog stays named while magpie is the provider, so a temporary
	// login change does not restart its naming time.
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	if !cx.DeskAt().Equal(at) {
		t.Fatalf("a magpie model picked beside the sign-in: named at %v, want %v", cx.DeskAt(), at)
	}
	if err := cx.Field("login").Set("api"); err != nil {
		t.Fatal(err)
	}
	if !cx.DeskAt().Equal(at) {
		t.Fatalf("api: named at %v\n%s", cx.DeskAt(), read())
	}
	// Back beside the sign-in, it is still the same file.
	if err := cx.Field("login").Set(""); err != nil {
		t.Fatal(err)
	}
	if again := cx.DeskAt(); !again.Equal(at) {
		t.Fatalf("named again at %v, want %v\n%s", again, at, read())
	}
}

func TestCodexLocalCatalogPathSurvivesTransitions(t *testing.T) {
	for _, name := range []string{"magpie-desktop-models.json", "my-models.json"} {
		t.Run(name, func(t *testing.T) {
			home, _ := codexHome(t, chatGPTAuth, "model = \"gpt-5.5\"\nmodel_catalog_json = \""+name+"\"\n")
			dir := filepath.Join(home, ".codex")
			local := filepath.Join(dir, name)
			writeFile(t, local, `{"title":"my catalog","models":[{"slug":"fake/m1","base_instructions":"my instruction","custom":true}]}`)
			app := true
			codexAppReads(t, &app)
			list := `{"models":[{"slug":"gpt-5.5"},{"slug":"fake/m1","base_instructions":"vendor instruction"}]}`
			deskListIs(t, &list)
			cx := codex(home)
			check := func(when string) {
				t.Helper()
				if ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); ref != name {
					t.Fatalf("%s: catalog path %q, want %q", when, ref, name)
				}
				b := readFile(local)
				if !strings.Contains(b, "my catalog") || !strings.Contains(b, "my instruction") || !strings.Contains(b, `"custom":true`) && !strings.Contains(b, `"custom": true`) {
					t.Fatalf("%s: local customizations lost: %s", when, b)
				}
				if _, err := os.Stat(filepath.Join(dir, "magpie-models.json")); !os.IsNotExist(err) {
					t.Fatalf("%s: wrote a second catalog: %v", when, err)
				}
				if msg := cx.Check(); msg != "" {
					t.Fatalf("%s: %s", when, msg)
				}
			}
			if ok, err := cx.Join(); err != nil || !ok {
				t.Fatalf("join: %v %v", ok, err)
			}
			check("join")
			if cx.DeskAt().IsZero() {
				t.Fatal("synchronizing an existing catalog missed the desktop reload notice")
			}
			if err := cx.Apply("model", "fake/m1"); err != nil {
				t.Fatal(err)
			}
			check("model pick")
			if err := cx.Field("login").Set("api"); err != nil {
				t.Fatal(err)
			}
			check("API")
			if err := provider.Save(provider.Provider{ID: "added", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m2"}}); err != nil {
				t.Fatal(err)
			}
			if err := codex(home).Sync(); err != nil {
				t.Fatal(err)
			}
			check("provider sync")
			if !strings.Contains(readFile(local), "added/m2") {
				t.Fatal("provider model did not reach the local catalog")
			}
			list = `{"models":[{"slug":"gpt-5.5"},{"slug":"fake/m1","base_instructions":"vendor instruction"},{"slug":"added/m2"}]}`
			if err := cx.Field("login").Set(""); err != nil {
				t.Fatal(err)
			}
			check("ChatGPT")
			codexUsedUp = func() bool { return true }
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			check("ChatGPT held")
			codexUsedUp = func() bool { return false }
			if err := cx.Sync(); err != nil {
				t.Fatal(err)
			}
			check("ChatGPT recovered")
			if err := cx.Disconnect(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(readFile(local), "my instruction") {
				t.Fatal("disconnect removed the local catalog")
			}
			if ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); ref != name {
				t.Fatalf("disconnect lost the customized catalog reference: %q", ref)
			}
			if err := cx.Apply("model", "fake/m1"); err != nil {
				t.Fatal(err)
			}
			check("reconnect")
		})
	}
}
