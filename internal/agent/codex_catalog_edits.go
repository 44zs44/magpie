package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	satomic "sync/atomic"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/codexcat"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// A generated catalog is editable, but is not a second configuration source.
// Fingerprints distinguish hand edits from old generated defaults. Only a
// successful write advances them; prompts themselves are not duplicated here.
type codexCatalogBaseline struct {
	Version int                          `json:"version"`
	Models  map[string]codexCatalogModel `json:"models"`
}

type codexCatalogModel struct {
	Fields   map[string]string `json:"fields"`
	Settings map[string]string `json:"settings"`
}

func (b codexCatalogBaseline) valid() bool {
	if b.Version != 1 || b.Models == nil {
		return false
	}
	for slug, m := range b.Models {
		if slug == "" || m.Fields == nil || m.Fields["slug"] == "" || m.Settings == nil {
			return false
		}
	}
	return true
}

var codexEditableFields = []string{"base_instructions", "context_window", "auto_compact_token_limit", "supported_reasoning_levels", "input_modalities"}

var codexCatalogEditMu sync.Mutex
var codexCatalogAgain satomic.Bool

func codexBaselinePath(path string) string {
	return filepath.Join(settings.Dir(), fmt.Sprintf("codex-catalog-%x.json", sha256.Sum256([]byte(filepath.Clean(path)))))
}

func codexFieldHash(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&value) != nil {
		return "invalid"
	}
	b, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func codexCatalogModels(raw []byte) (map[string]map[string]json.RawMessage, error) {
	var cat struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil || cat.Models == nil {
		return nil, fmt.Errorf("invalid Codex model catalog (expected a models array)")
	}
	out := map[string]map[string]json.RawMessage{}
	for _, m := range cat.Models {
		var slug string
		if json.Unmarshal(m["slug"], &slug) != nil || slug == "" || out[slug] != nil {
			return nil, fmt.Errorf("invalid or duplicate Codex model slug %q", slug)
		}
		if raw := m["model_messages"]; len(raw) > 0 && string(raw) != "null" {
			var messages map[string]json.RawMessage
			if json.Unmarshal(raw, &messages) != nil || messages == nil {
				return nil, fmt.Errorf("%s: model_messages must be an object", slug)
			}
			m["model_messages.instructions_template"] = messages["instructions_template"]
			delete(messages, "instructions_template")
			m["model_messages"], _ = json.Marshal(messages)
		} else {
			m["model_messages"] = json.RawMessage(`{}`)
		}
		out[slug] = m
	}
	return out, nil
}

func codexCatalogRef(slug string) string {
	if !strings.Contains(slug, "/") {
		return "codex/" + slug
	}
	return slug
}

func codexCatalogRecord(models map[string]map[string]json.RawMessage) codexCatalogBaseline {
	out := codexCatalogBaseline{Version: 1, Models: map[string]codexCatalogModel{}}
	for slug, fields := range models {
		m := codexCatalogModel{Fields: map[string]string{}, Settings: map[string]string{}}
		for k, v := range fields {
			m.Fields[k] = codexFieldHash(v)
		}
		// An upstream-only model can be listed before Magpie has an account
		// for it. Its edits will be refused by ApplyCodexModelEdits.
		state, _ := provider.CodexModelEditState(codexCatalogRef(slug))
		for k, v := range state {
			m.Settings[k] = codexFieldHash(v)
		}
		out.Models[slug] = m
	}
	return out
}

func codexCatalogChanges(models map[string]map[string]json.RawMessage, was codexCatalogBaseline, generated map[string]map[string]json.RawMessage) (map[string]map[string]json.RawMessage, error) {
	edits := map[string]map[string]json.RawMessage{}
	for slug, fields := range models {
		old, known := was.Models[slug]
		if !known {
			continue
		}
		changes := map[string]json.RawMessage{}
		for _, k := range codexEditableFields {
			if codexFieldHash(fields[k]) != old.Fields[k] {
				changes[k] = fields[k]
			}
		}
		const template = "model_messages.instructions_template"
		if codexFieldHash(fields["model_messages"]) != old.Fields["model_messages"] {
			return nil, fmt.Errorf("%s: only model_messages.instructions_template is editable; keep the other template fields unchanged", slug)
		}
		if codexFieldHash(fields[template]) != old.Fields[template] {
			if v, both := changes["base_instructions"]; both && codexFieldHash(v) != codexFieldHash(fields[template]) {
				return nil, fmt.Errorf("%s: base_instructions and instructions_template were changed to different values", slug)
			}
			changes["base_instructions"] = fields[template]
		}
		if len(changes) == 0 {
			continue
		}
		ref := codexCatalogRef(slug)
		state, err := provider.CodexModelEditState(ref)
		if err != nil {
			return nil, err
		}
		for k, v := range changes {
			current := codexFieldHash(state[k])
			// Reasoning descriptions are generated prose, not preferences.
			if k == "supported_reasoning_levels" && len(v) > 0 {
				var levels []struct {
					Effort string `json:"effort"`
				}
				if json.Unmarshal(v, &levels) == nil {
					v, _ = json.Marshal(levels)
				}
			}
			effective := generated[slug][k]
			if k == "supported_reasoning_levels" && len(effective) > 0 {
				var levels []struct {
					Effort string `json:"effort"`
				}
				if json.Unmarshal(effective, &levels) == nil {
					effective, _ = json.Marshal(levels)
				}
			}
			// Existing setters remove an override equal to the vendor's
			// value. Its effective value also permits a partial-save retry.
			if current != old.Settings[k] && current != codexFieldHash(v) && codexFieldHash(effective) != codexFieldHash(v) {
				return nil, fmt.Errorf("%s: %s changed both in Magpie and in the catalog; import the intended field explicitly or undo the file edit", slug, k)
			}
		}
		edits[ref] = changes
	}
	return edits, nil
}

// writeCodexCatalog imports only changes against the last successful write,
// then rebuilds from the saved settings. Catalog and baseline commit together,
// independently of agent wiring: a wiring rollback must not erase a hand edit.
func writeCodexCatalog(path string, build func() ([]byte, error), keep bool) (err error) {
	// A save can invoke SyncCatalog synchronously. Never wait for that
	// callback under this lock; a successful import schedules a fresh round.
	if !codexCatalogEditMu.TryLock() {
		codexCatalogAgain.Store(true)
		return fmt.Errorf("a Codex catalog edit is in progress; retry the sync")
	}
	unlock, locked := filelock.Lock(filepath.Join(settings.Dir(), "codex-catalog-edits.lock"), 0)
	imported := false
	defer func() {
		unlock()
		codexCatalogEditMu.Unlock()
		again := codexCatalogAgain.Swap(false)
		if imported && err == nil || again {
			catalog.Touched()
		}
	}()
	if !locked {
		return fmt.Errorf("another Magpie is editing a Codex catalog; retry the sync")
	}
	defer func() {
		if err != nil {
			log.Printf("codex catalog %s: %v", path, err)
		}
	}()
	if _, err := settings.LoadChecked(); err != nil {
		return err
	}
	if codexcat.ModelOverridesOn() {
		if err := codexcat.PrepareInstructions(); err != nil {
			return err
		}
	}
	cur, err := edit.Read(path)
	if err != nil {
		return err
	}
	b, err := build()
	if err != nil {
		return err
	}
	next, err := codexCatalogModels(b)
	if err != nil {
		return err
	}
	if len(cur) > 0 {
		have, err := codexCatalogModels(cur)
		if err != nil {
			return fmt.Errorf("%w; the file was left unchanged", err)
		}
		raw, err := edit.Read(codexBaselinePath(path))
		if err != nil {
			return err
		}
		var was codexCatalogBaseline
		if len(raw) > 0 {
			if json.Unmarshal(raw, &was) != nil || !was.valid() {
				return fmt.Errorf("invalid Codex catalog baseline; the catalog was left unchanged")
			}
		} else {
			// Existing catalogs predate tracking. A difference cannot safely
			// be called either an old default or a hand edit.
			was = codexCatalogRecord(next)
		}
		edits, err := codexCatalogChanges(have, was, next)
		if err != nil {
			return err
		}
		if len(edits) > 0 && len(raw) == 0 {
			return fmt.Errorf("this catalog has no edit baseline; import the intended fields with magpie model codex-import before syncing")
		}
		if len(edits) > 0 {
			imported = true
			if err := provider.ApplyCodexModelEdits(edits); err != nil {
				return err
			}
			b, err = build()
			if err != nil {
				return err
			}
		}
		if keep {
			b = codexcat.Keep(cur, b)
		}
	}
	next, err = codexCatalogModels(b)
	if err != nil {
		return err
	}
	record, err := json.Marshal(codexCatalogRecord(next))
	if err != nil {
		return err
	}
	// Re-read before replacing: a save made during generation is not ours.
	latest, err := edit.Read(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, cur) {
		return fmt.Errorf("the catalog changed while syncing; retry after saving it")
	}
	baseline := codexBaselinePath(path)
	if err := os.MkdirAll(filepath.Dir(baseline), 0o700); err != nil {
		return err
	}
	old, err := edit.Read(baseline)
	if err != nil {
		return err
	}
	return edit.Atomically(func() error {
		if !bytes.Equal(cur, b) {
			if err := edit.WriteAtomic(path, b); err != nil {
				return err
			}
		}
		if bytes.Equal(old, record) {
			return nil
		}
		return edit.WriteAtomic(baseline, record)
	}, path, baseline)
}

// ImportCodexCatalog imports selected fields of one model, without taking
// ownership of a custom catalog or changing model_catalog_json. It also
// establishes a baseline for a managed catalog that predates edit tracking.
func ImportCodexCatalog(path, ref string, fields []string) (err error) {
	if !codexCatalogEditMu.TryLock() {
		codexCatalogAgain.Store(true)
		return fmt.Errorf("a Codex catalog edit is in progress; retry the import")
	}
	unlock, locked := filelock.Lock(filepath.Join(settings.Dir(), "codex-catalog-edits.lock"), 0)
	defer func() {
		unlock()
		codexCatalogEditMu.Unlock()
		again := codexCatalogAgain.Swap(false)
		if err == nil || again {
			catalog.Touched()
		}
	}()
	if !locked {
		return fmt.Errorf("another Magpie is editing a Codex catalog; retry the import")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	raw, err := edit.Read(path)
	if err != nil {
		return err
	}
	models, err := codexCatalogModels(raw)
	if err != nil {
		return err
	}
	slug := ref
	m := models[slug]
	if m == nil && strings.HasPrefix(ref, "codex/") {
		slug = strings.TrimPrefix(ref, "codex/")
		m = models[slug]
	}
	if m == nil {
		return fmt.Errorf("catalog has no model %s", ref)
	}
	changes := map[string]json.RawMessage{}
	for _, field := range fields {
		key := field
		if field == "model_messages.instructions_template" {
			key = "base_instructions"
		}
		valid := false
		for _, k := range codexEditableFields {
			if k == key {
				valid = true
			}
		}
		if !valid {
			return fmt.Errorf("unsupported Codex catalog field %q", field)
		}
		if prior, ok := changes[key]; ok && codexFieldHash(prior) != codexFieldHash(m[field]) {
			return fmt.Errorf("select one instruction field, or give both the same value")
		}
		changes[key] = m[field]
	}
	if len(changes) == 0 {
		return fmt.Errorf("name at least one field to import")
	}
	baseline := codexBaselinePath(path)
	previous, err := edit.Read(baseline)
	if err != nil {
		return err
	}
	var was codexCatalogBaseline
	if len(previous) > 0 && (json.Unmarshal(previous, &was) != nil || !was.valid()) {
		return fmt.Errorf("invalid Codex catalog baseline; import was not applied")
	}
	if err := provider.ApplyCodexModelEdits(map[string]map[string]json.RawMessage{ref: changes}); err != nil {
		return err
	}
	if name := filepath.Base(path); name == "magpie-models.json" || name == "magpie-desktop-models.json" {
		record := codexCatalogRecord(models)
		if len(previous) > 0 {
			old, known := was.Models[slug]
			if !known {
				old = record.Models[slug]
			}
			for _, field := range fields {
				old.Fields[field] = record.Models[slug].Fields[field]
			}
			for field := range changes {
				old.Settings[field] = record.Models[slug].Settings[field]
			}
			was.Models[slug] = old
			record = was
		}
		b, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(baseline), 0o700); err != nil {
			return err
		}
		return edit.WriteAtomic(baseline, b)
	}
	return nil
}

// A disconnect may remove a generated list, but never an edit waiting to be
// imported. Unknown or malformed state is kept for explicit recovery too.
func codexCatalogPending(path string) bool {
	cur, err := edit.Read(path)
	if err != nil {
		return true
	}
	if len(cur) == 0 {
		return false
	}
	models, err := codexCatalogModels(cur)
	if err != nil {
		return true
	}
	raw, err := edit.Read(codexBaselinePath(path))
	if err != nil || len(raw) == 0 {
		return true
	}
	var was codexCatalogBaseline
	if json.Unmarshal(raw, &was) != nil || !was.valid() {
		return true
	}
	for slug, fields := range models {
		old, ok := was.Models[slug]
		if !ok {
			return true
		}
		for _, k := range append(append([]string{}, codexEditableFields...), "model_messages.instructions_template", "model_messages") {
			if codexFieldHash(fields[k]) != old.Fields[k] {
				return true
			}
		}
	}
	return false
}
