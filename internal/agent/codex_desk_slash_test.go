package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
)

// #4785's sibling: the Codex app's list named by an absolute path spelled
// with / is still recognized on Windows. With the app gone the existing
// local catalog and its path stay available to the CLI.
func TestCodexDesktopListSlashSpelled(t *testing.T) {
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, `model = "gpt-5.5"`+"\n")
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	desk := filepath.Join(home, ".codex", "magpie-desktop-models.json")
	app := true
	codexAppReads(t, &app)
	list := `{"local_note":"kept","models":[{"slug":"gpt-6-sol"}]}`
	deskListIs(t, &list)
	cx := codex(home)
	if ok, err := cx.Join(); err != nil || !ok {
		t.Fatalf("join: %v %v", ok, err)
	}
	cfg := read()
	if !strings.Contains(cfg, "magpie-desktop-models.json") {
		t.Fatalf("the app installed:\n%s", cfg)
	}
	// as an older magpie, or the user, spelled it: absolute, with /
	var out []string
	for _, l := range strings.Split(cfg, "\n") {
		if strings.HasPrefix(l, "model_catalog_json") {
			l = `model_catalog_json = "` + filepath.ToSlash(desk) + `"`
		}
		out = append(out, l)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	app = false
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if ref, _ := edit.GetTOMLTop(cx.Path, "model_catalog_json"); ref != filepath.ToSlash(desk) {
		t.Fatalf("the app gone: catalog path changed to %q", ref)
	}
	if got := readFile(desk); !strings.Contains(got, `"local_note": "kept"`) {
		t.Fatalf("the app gone: local catalog content lost:\n%s", got)
	}
}
