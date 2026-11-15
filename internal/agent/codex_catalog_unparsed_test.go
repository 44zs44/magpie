package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// A malformed editable catalog is left in place by synchronization.
// Disconnect also keeps it, because it may contain unsaved preferences.
func TestUnparsedCodexCatalogKeptAside(t *testing.T) {
	home, _ := codexHome(t, "", "")
	list := filepath.Join(home, ".codex", "magpie-models.json")
	cx := codex(home)
	if err := cx.Fields[0].Set("fake/m1"); err != nil {
		t.Fatal(err)
	}
	// the file as it stands, broken the way a half-save or a sync conflict
	// leaves one: a cut before the last brace, no longer a catalog
	b := readFile(list)
	writeFile(t, list, b[:len(b)-2])

	// a provider added: the sync writes the list again
	if err := provider.Save(provider.Provider{ID: "added", Name: "Added", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m2"}}); err != nil {
		t.Fatal(err)
	}
	SyncCatalog()
	if readFile(list) != b[:len(b)-2] {
		t.Fatal("sync replaced the malformed editable catalog")
	}

	// Stepping out disconnects the config without deleting the broken file.
	writeFile(t, list, b[:len(b)-2])
	if err := cx.Fields[0].Set(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(list); err != nil || readFile(list) != b[:len(b)-2] {
		t.Fatal("disconnect lost the malformed editable catalog")
	}
}
