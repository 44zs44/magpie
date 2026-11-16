package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

// Generated fingerprints drive parameter deltas. Last records actual output
// so external deletions/effort edits can be recognized, and Archive keeps the
// complete local rows needed when a hidden or disabled model returns.
type codexCatalogBaseline struct {
	Version  int                             `json:"version"`
	Models   map[string]codexCatalogModel    `json:"models"`
	Top      map[string]codexCatalogField    `json:"top,omitempty"`
	Order    []string                        `json:"order,omitempty"`
	Ordering string                          `json:"ordering,omitempty"`
	Hash     string                          `json:"hash,omitempty"`
	Source   string                          `json:"source,omitempty"`
	Last     map[string]codexCatalogSnapshot `json:"last"`
	Archive  map[string]codexCatalogArchive  `json:"archive"`
	Slots    []string                        `json:"slots,omitempty"`
}

type codexCatalogModel struct {
	Fields    map[string]string            `json:"fields,omitempty"` // version 1
	Settings  map[string]string            `json:"settings"`
	Generated map[string]codexCatalogField `json:"generated,omitempty"`
	Reasoning string                       `json:"reasoning,omitempty"`
}

type codexCatalogField struct {
	Hash     string                       `json:"hash"`
	Object   bool                         `json:"object,omitempty"`
	Children map[string]codexCatalogField `json:"children,omitempty"`
}

type codexCatalogSnapshot struct {
	Ref   string                     `json:"ref"`
	Entry map[string]json.RawMessage `json:"entry"`
}

type codexCatalogArchive struct {
	Ref       string                       `json:"ref"`
	Entry     map[string]json.RawMessage   `json:"entry"`
	Generated map[string]codexCatalogField `json:"generated"`
	Settings  map[string]string            `json:"settings"`
	Source    string                       `json:"source"`
	Ordering  string                       `json:"ordering"`
}

type codexCatalogPlan struct {
	Disable []string
	Apply   map[string]map[string]json.RawMessage
}

func (b codexCatalogBaseline) valid() bool {
	if (b.Version < 1 || b.Version > 3) || b.Models == nil {
		return false
	}
	for slug, m := range b.Models {
		if slug == "" || m.Settings == nil {
			return false
		}
		if b.Version == 1 && (m.Fields == nil || m.Fields["slug"] == "") || b.Version >= 2 && (m.Generated == nil || m.Generated["slug"].Hash == "") {
			return false
		}
	}
	if b.Version >= 2 {
		if b.Hash == "" || b.Ordering == "" || len(b.Order) != len(b.Models) {
			return false
		}
		seen := map[string]bool{}
		for _, slug := range b.Order {
			if _, ok := b.Models[slug]; !ok || seen[slug] {
				return false
			}
			seen[slug] = true
		}
	}
	if b.Version == 3 && (b.Last == nil || b.Archive == nil) {
		return false
	}
	for slug, snapshot := range b.Last {
		var id string
		if snapshot.Ref == "" || json.Unmarshal(snapshot.Entry["slug"], &id) != nil || id != slug {
			return false
		}
	}
	for slug, saved := range b.Archive {
		var id string
		if saved.Ref == "" || saved.Generated == nil || saved.Settings == nil || json.Unmarshal(saved.Entry["slug"], &id) != nil || id != slug {
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
	if len(raw) == 0 {
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

func codexCatalogRead(raw []byte) (map[string]json.RawMessage, []string, map[string]map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	var models []map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || top == nil || json.Unmarshal(top["models"], &models) != nil || models == nil {
		return nil, nil, nil, fmt.Errorf("invalid Codex model catalog (expected a models array)")
	}
	delete(top, "models")
	order := make([]string, 0, len(models))
	out := map[string]map[string]json.RawMessage{}
	for _, m := range models {
		var slug string
		if json.Unmarshal(m["slug"], &slug) != nil || slug == "" || out[slug] != nil {
			return nil, nil, nil, fmt.Errorf("invalid or duplicate Codex model slug %q", slug)
		}
		order = append(order, slug)
		out[slug] = m
	}
	return top, order, out, nil
}

func codexCatalogModels(raw []byte) (map[string]map[string]json.RawMessage, error) {
	_, _, models, err := codexCatalogRead(raw)
	if err != nil {
		return nil, err
	}
	for slug, m := range models {
		if raw := m["model_messages"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var messages map[string]json.RawMessage
			if json.Unmarshal(raw, &messages) != nil || messages == nil {
				return nil, fmt.Errorf("%s: model_messages must be an object", slug)
			}
			m["model_messages.instructions_template"] = messages["instructions_template"]
		}
	}
	return models, nil
}

func codexCatalogRef(slug string) string {
	if !strings.Contains(slug, "/") {
		return "codex/" + slug
	}
	return slug
}

func codexCatalogFields(fields map[string]json.RawMessage) map[string]codexCatalogField {
	out := map[string]codexCatalogField{}
	for k, raw := range fields {
		f := codexCatalogField{Hash: codexFieldHash(raw)}
		var children map[string]json.RawMessage
		if json.Unmarshal(raw, &children) == nil && children != nil {
			f.Object, f.Children = true, codexCatalogFields(children)
		}
		out[k] = f
	}
	return out
}

func codexCatalogClone(fields map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		out[k] = bytes.Clone(v)
	}
	return out
}

func codexCatalogRecord(raw []byte) (codexCatalogBaseline, error) {
	top, order, models, err := codexCatalogRead(raw)
	if err != nil {
		return codexCatalogBaseline{}, err
	}
	// Codex sorts by priority, even when the JSON array uses another order.
	sort.SliceStable(order, func(i, j int) bool {
		var a, b float64
		json.Unmarshal(models[order[i]]["priority"], &a)
		json.Unmarshal(models[order[j]]["priority"], &b)
		return a < b
	})
	s := settings.Shared()
	ordering, _ := json.Marshal(s.OrderedModels["codex"])
	out := codexCatalogBaseline{Version: 3, Models: map[string]codexCatalogModel{}, Top: codexCatalogFields(top), Order: order, Slots: slices.Clone(order), Ordering: codexFieldHash(ordering), Hash: codexFieldHash(raw), Last: map[string]codexCatalogSnapshot{}, Archive: map[string]codexCatalogArchive{}}
	for slug, fields := range models {
		m := codexCatalogModel{Generated: codexCatalogFields(fields), Settings: map[string]string{}}
		m.Reasoning, err = codexCatalogEfforts(fields["supported_reasoning_levels"])
		if err != nil {
			return codexCatalogBaseline{}, fmt.Errorf("%s: %w", slug, err)
		}
		ref := codexCatalogRef(slug)
		state, _ := provider.CodexModelEditState(ref)
		for k, v := range state {
			m.Settings[k] = codexFieldHash(v)
		}
		// Track explicit choices too: choosing the current default must
		// still replace a different value in the local catalog.
		put := func(k string, v any) { b, _ := json.Marshal(v); m.Settings[k] = codexFieldHash(b) }
		if name, ok := s.ModelNames[ref]; ok {
			put("display_name", name)
		}
		if !provider.IsGroupRef(ref) {
			if p, model, ok := provider.Resolve(ref); ok {
				if n := p.ContextOf(model); n > 0 {
					put("context_window", n)
				}
			}
		}
		pid, model, _ := strings.Cut(ref, "/")
		for _, key := range []string{ref, pid + "/*", model, "*"} {
			if n := s.AgentContexts["codex"][key]; n > 0 {
				put("context_window", []any{key, n})
				break
			}
		}
		if n := provider.CompactSetIn(s, ref, provider.GroupFinder()); n > 0 {
			put("auto_compact_token_limit", n)
		} else if work := s.ForAgent("codex"); work.FullContext || work.CompactAt > 0 {
			put("auto_compact_token_limit", []any{work.FullContext, work.CompactAt})
		}
		out.Models[slug] = m
		out.Last[slug] = codexCatalogSnapshot{Ref: ref, Entry: codexCatalogClone(fields)}
	}
	return out, nil
}

func codexCatalogEfforts(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "null", nil
	}
	var rows []struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", fmt.Errorf("supported_reasoning_levels must be an array or null")
	}
	levels := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Effort == "" {
			return "", fmt.Errorf("a reasoning level needs an effort")
		}
		levels = append(levels, row.Effort)
	}
	b, _ := json.Marshal(levels)
	return string(b), nil
}

func codexCatalogEdits(cur []byte, was, next codexCatalogBaseline) (codexCatalogPlan, error) {
	plan := codexCatalogPlan{Apply: map[string]map[string]json.RawMessage{}}
	if was.Version < 3 || len(cur) == 0 || was.Source != next.Source {
		return plan, nil
	}
	_, _, models, err := codexCatalogRead(cur)
	if err != nil {
		return plan, err
	}
	canonical := map[string]bool{}
	for slug := range models {
		canonical[provider.CanonicalRef(codexCatalogRef(slug))] = true
	}
	const field = "supported_reasoning_levels"
	for slug, last := range was.Last {
		old, managed := was.Models[slug]
		current, listed := next.Models[slug]
		if !managed || !listed {
			continue
		}
		ref := provider.CanonicalRef(last.Ref)
		if _, archived := was.Archive[slug]; archived {
			continue
		}
		if models[slug] == nil {
			if canonical[ref] {
				continue
			}
			if strings.ContainsAny(ref, "*?") {
				return plan, fmt.Errorf("%s: cannot disable a catalog model whose id contains a wildcard", ref)
			}
			plan.Disable = append(plan.Disable, ref)
			continue
		}
		value, err := codexCatalogEfforts(models[slug][field])
		if err != nil {
			return plan, fmt.Errorf("%s: %w", slug, err)
		}
		before, err := codexCatalogEfforts(last.Entry[field])
		if err != nil || value == before {
			continue
		}
		if _, err := provider.CodexModelEditState(ref); err != nil {
			continue // local-only or a backend model with no Magpie account
		}
		// A change in Magpie wins over an edit of the same field. Ignore
		// generated descriptions: only the available effort sequence matters.
		if old.Settings[field] != current.Settings[field] {
			continue
		}
		if old.Reasoning != current.Reasoning {
			continue
		}
		plan.Apply[ref] = map[string]json.RawMessage{field: models[slug][field]}
	}
	slices.Sort(plan.Disable)
	return plan, nil
}

// Objects merge by member; arrays and scalars change as a whole. A missing
// member is a deletion, distinct from an explicit null.
func codexCatalogMergeFields(local, generated map[string]json.RawMessage, was map[string]codexCatalogField) {
	keys := map[string]bool{}
	for k := range was {
		keys[k] = true
	}
	for k := range generated {
		keys[k] = true
	}
	for k := range keys {
		raw, exists := generated[k]
		old := was[k]
		if codexFieldHash(raw) == old.Hash {
			continue
		}
		if !exists {
			var have map[string]json.RawMessage
			if old.Object && json.Unmarshal(local[k], &have) == nil && have != nil {
				codexCatalogMergeFields(have, nil, old.Children)
				if len(have) > 0 {
					local[k], _ = json.Marshal(have)
					continue
				}
			}
			delete(local, k)
			continue
		}
		var next, have map[string]json.RawMessage
		if json.Unmarshal(raw, &next) == nil && next != nil {
			json.Unmarshal(local[k], &have)
			if have == nil {
				have = map[string]json.RawMessage{}
			}
			codexCatalogMergeFields(have, next, old.Children)
			local[k], _ = json.Marshal(have)
		} else {
			local[k] = raw
		}
	}
}

func codexCatalogMerge(cur, generated []byte, was codexCatalogBaseline, next *codexCatalogBaseline) ([]byte, error) {
	top, order, local, err := codexCatalogRead(cur)
	if err != nil {
		return nil, err
	}
	nextTop, _, models, err := codexCatalogRead(generated)
	if err != nil {
		return nil, err
	}
	tracked := was.Version >= 2
	sourceChanged := tracked && was.Source != next.Source
	if tracked && !sourceChanged {
		codexCatalogMergeFields(top, nextTop, was.Top)
	}
	previous := slices.Clone(was.Order)
	slots := slices.Clone(was.Slots)
	if len(slots) == 0 {
		slots = slices.Clone(order)
	}
	for slug, saved := range was.Archive {
		next.Archive[slug] = saved
	}
	// Capture the actual local row before a provider withdrawal, hidden
	// model or global disable removes it from this list.
	for slug, old := range was.Models {
		if !tracked || models[slug] != nil {
			continue
		}
		alias := codexCatalogRef(slug)
		if alias == slug && strings.HasPrefix(slug, "codex/") {
			alias = strings.TrimPrefix(slug, "codex/")
		}
		if models[alias] != nil {
			continue
		}
		entry := local[slug]
		if entry == nil {
			entry = was.Last[slug].Entry
		}
		if entry != nil {
			next.Archive[slug] = codexCatalogArchive{Ref: provider.CanonicalRef(codexCatalogRef(slug)), Entry: codexCatalogClone(entry), Generated: old.Generated, Settings: old.Settings, Source: was.Source, Ordering: was.Ordering}
		}
	}
	// An enabled/unhidden model resumes its complete local row, using the
	// source baseline from when it left so intervening parameter edits apply.
	restoredOrderChanged := false
	for _, savedSlug := range slots {
		saved, archived := next.Archive[savedSlug]
		if !archived {
			continue
		}
		slug := savedSlug
		if models[slug] == nil {
			slug = codexCatalogRef(slug)
			if slug == savedSlug && strings.HasPrefix(slug, "codex/") {
				slug = strings.TrimPrefix(slug, "codex/")
			}
		}
		if models[slug] == nil {
			continue
		}
		if local[slug] == nil {
			local[slug] = codexCatalogClone(saved.Entry)
			local[slug]["slug"], _ = json.Marshal(slug)
			at := len(order)
			for _, after := range slots[slices.Index(slots, savedSlug)+1:] {
				if i := slices.Index(order, after); i >= 0 {
					at = i
					break
				}
			}
			order = slices.Insert(order, at, slug)
		}
		if was.Models == nil {
			was.Models = map[string]codexCatalogModel{}
		}
		was.Models[slug] = codexCatalogModel{Generated: saved.Generated, Settings: saved.Settings}
		if saved.Source != next.Source {
			old := was.Models[slug]
			old.Generated = next.Models[slug].Generated
			was.Models[slug] = old
		}
		restoredOrderChanged = restoredOrderChanged || saved.Ordering != next.Ordering
		delete(next.Archive, savedSlug)
		if savedSlug != slug {
			slots[slices.Index(slots, savedSlug)] = slug
		}
	}
	kept := make([]string, 0, len(order))
	for _, slug := range order {
		fields := local[slug]
		localLevels := bytes.Clone(fields["supported_reasoning_levels"])
		old, managed := was.Models[slug]
		if models[slug] == nil {
			// Joining ChatGPT and using Magpie as the provider spell the
			// same native model as gpt-x and codex/gpt-x respectively.
			alias := codexCatalogRef(slug)
			if alias == slug && strings.HasPrefix(slug, "codex/") {
				alias = strings.TrimPrefix(slug, "codex/")
			}
			if models[alias] != nil && local[alias] == nil && was.Models[alias].Generated == nil {
				for i, prior := range previous {
					if prior == slug {
						previous[i] = alias
					}
				}
				delete(local, slug)
				slug = alias
				local[slug] = fields
				fields["slug"], _ = json.Marshal(slug)
			} else if tracked && managed {
				delete(local, slug)
				continue
			}
		}
		if tracked && managed {
			keepLocalLevels := false
			if last, ok := was.Last[slug]; ok {
				state, err := provider.CodexModelEditState(codexCatalogRef(slug))
				value, valueErr := codexCatalogEfforts(localLevels)
				before, _ := codexCatalogEfforts(last.Entry["supported_reasoning_levels"])
				configured, _ := codexCatalogEfforts(state["supported_reasoning_levels"])
				// An imported preference can gain a synthetic Ultra during
				// generation. Keep the exact local selection, including on a
				// retry after settings saved but the catalog write failed, and
				// when a later sync only rewords the source's descriptions:
				// the effort sequence and the explicit setting are unchanged.
				reworded := old.Reasoning == next.Models[slug].Reasoning &&
					old.Settings["supported_reasoning_levels"] == next.Models[slug].Settings["supported_reasoning_levels"]
				keepLocalLevels = err == nil && valueErr == nil && value != "null" && value == configured && (value != before || reworded)
			}
			if !sourceChanged {
				priority, hadPriority := fields["priority"]
				codexCatalogMergeFields(fields, models[slug], old.Generated)
				if hadPriority {
					fields["priority"] = priority
				} else {
					delete(fields, "priority")
				}
			}
			for _, k := range append(append([]string{}, codexEditableFields...), "display_name") {
				if old.Settings[k] != next.Models[slug].Settings[k] {
					if raw, ok := models[slug][k]; ok {
						fields[k] = raw
					} else {
						delete(fields, k)
					}
					if k == "base_instructions" {
						var have, messages map[string]json.RawMessage
						json.Unmarshal(fields["model_messages"], &have)
						json.Unmarshal(models[slug]["model_messages"], &messages)
						if raw, ok := messages["instructions_template"]; ok {
							if have == nil {
								have = map[string]json.RawMessage{}
							}
							have["instructions_template"] = raw
							fields["model_messages"], _ = json.Marshal(have)
						} else if have != nil {
							delete(have, "instructions_template")
							fields["model_messages"], _ = json.Marshal(have)
						}
					}
				}
			}
			if keepLocalLevels {
				fields["supported_reasoning_levels"] = localLevels
			}
			// Effort membership is a shared preference; local explanations
			// and any other row metadata stay on the surviving efforts.
			var beforeLevels, afterLevels []map[string]json.RawMessage
			if json.Unmarshal(localLevels, &beforeLevels) == nil && json.Unmarshal(fields["supported_reasoning_levels"], &afterLevels) == nil {
				for _, after := range afterLevels {
					for _, before := range beforeLevels {
						if bytes.Equal(after["effort"], before["effort"]) {
							for k, v := range before {
								if k != "effort" {
									after[k] = v
								}
							}
						}
					}
				}
				if afterLevels != nil {
					fields["supported_reasoning_levels"], _ = json.Marshal(afterLevels)
				}
			}
			if !sourceChanged && old.Generated["supported_reasoning_levels"].Hash != next.Models[slug].Generated["supported_reasoning_levels"].Hash || old.Settings["supported_reasoning_levels"] != next.Models[slug].Settings["supported_reasoning_levels"] {
				var effort string
				var levels []struct {
					Effort string `json:"effort"`
				}
				json.Unmarshal(fields["default_reasoning_level"], &effort)
				json.Unmarshal(fields["supported_reasoning_levels"], &levels)
				valid := effort == ""
				for _, level := range levels {
					valid = valid || level.Effort == effort
				}
				if !valid {
					if raw, ok := models[slug]["default_reasoning_level"]; ok {
						fields["default_reasoning_level"] = raw
					} else {
						delete(fields, "default_reasoning_level")
					}
				}
			}
		}
		kept = append(kept, slug)
	}
	// A local-only model isn't a provider deletion. Newly generated slugs
	// become managed without overwriting a same-slug local entry on adoption.
	priority := 0.0
	for _, fields := range local {
		var n float64
		if json.Unmarshal(fields["priority"], &n) == nil && n > priority {
			priority = n
		}
	}
	for _, slug := range next.Order {
		_, existed := was.Models[slug]
		if local[slug] == nil && (!tracked || !existed) {
			local[slug] = models[slug]
			priority++
			local[slug]["priority"], _ = json.Marshal(priority)
			kept = append(kept, slug)
		}
	}
	var commonBefore, commonAfter []string
	for _, slug := range previous {
		if models[slug] != nil {
			commonBefore = append(commonBefore, slug)
		}
	}
	for _, slug := range next.Order {
		if slices.Contains(previous, slug) {
			commonAfter = append(commonAfter, slug)
		}
	}
	if tracked && (!sourceChanged && !slices.Equal(commonBefore, commonAfter) || was.Ordering != next.Ordering || restoredOrderChanged) {
		// Keep local-only rows in their slots, and fill managed slots in
		// Magpie's order. Codex's existing slots are ordered by priority,
		// which can differ from their positions in the JSON array.
		sort.SliceStable(kept, func(i, j int) bool {
			var a, b float64
			json.Unmarshal(local[kept[i]]["priority"], &a)
			json.Unmarshal(local[kept[j]]["priority"], &b)
			return a < b
		})
		ordered := make([]string, 0, len(kept))
		var managedOrder []string
		for _, slug := range next.Order {
			if local[slug] != nil {
				managedOrder = append(managedOrder, slug)
			}
		}
		i := 0
		for _, slug := range kept {
			if models[slug] == nil {
				ordered = append(ordered, slug)
			} else {
				ordered = append(ordered, managedOrder[i])
				i++
			}
		}
		kept = ordered
		for i, slug := range kept {
			local[slug]["priority"], _ = json.Marshal(i + 1)
		}
	}
	entries := make([]map[string]json.RawMessage, 0, len(kept))
	next.Last = map[string]codexCatalogSnapshot{}
	for _, slug := range kept {
		entries = append(entries, local[slug])
		next.Last[slug] = codexCatalogSnapshot{Ref: provider.CanonicalRef(codexCatalogRef(slug)), Entry: codexCatalogClone(local[slug])}
	}
	i := 0
	next.Slots = nil
	for _, slug := range slots {
		if _, archived := next.Archive[slug]; archived {
			next.Slots = append(next.Slots, slug)
		} else if local[slug] != nil && i < len(kept) {
			next.Slots = append(next.Slots, kept[i])
			i++
		}
	}
	next.Slots = append(next.Slots, kept[i:]...)
	top["models"], _ = json.Marshal(entries)
	out, err := json.MarshalIndent(top, "", "  ")
	if err == nil && codexFieldHash(out) == codexFieldHash(cur) {
		return cur, nil
	}
	return out, err
}

// Only a successful write advances the generated baseline. Catalog and
// baseline commit together, independently of the agent wiring transaction.
func writeCodexCatalog(path string, build func() ([]byte, error), providerList bool) (err error) {
	// A save can invoke SyncCatalog synchronously. Never wait for that
	// callback under this lock; a competing sync schedules a fresh round.
	if !codexCatalogEditMu.TryLock() {
		codexCatalogAgain.Store(true)
		return fmt.Errorf("a Codex catalog edit is in progress; retry the sync")
	}
	unlock, locked := filelock.Lock(filepath.Join(settings.Dir(), "codex-catalog-edits.lock"), 0)
	defer func() {
		unlock()
		codexCatalogEditMu.Unlock()
		again := codexCatalogAgain.Swap(false)
		if again {
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
	if len(cur) > 0 {
		if _, _, _, err := codexCatalogRead(cur); err != nil {
			return fmt.Errorf("%w; the file was left unchanged", err)
		}
	}
	baseline := codexBaselinePath(path)
	old, err := edit.Read(baseline)
	if err != nil {
		return err
	}
	var was codexCatalogBaseline
	if len(old) > 0 && (json.Unmarshal(old, &was) != nil || !was.valid()) {
		return fmt.Errorf("invalid Codex catalog baseline; the catalog was left unchanged")
	}
	b, err := build()
	if err != nil {
		return err
	}
	next, err := codexCatalogRecord(b)
	if err != nil {
		return err
	}
	next.Source = "desktop"
	if providerList {
		next.Source = "provider"
	}
	plan, err := codexCatalogEdits(cur, was, next)
	if err != nil {
		return err
	}
	// Validate the whole input and deletion plan before any shared setting
	// is changed, then reject a save made while the source was being built.
	latest, err := edit.Read(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, cur) {
		return fmt.Errorf("the catalog changed while syncing; retry after saving it")
	}
	if len(plan.Apply) > 0 {
		if err := provider.ApplyCodexModelEdits(plan.Apply); err != nil {
			return err
		}
	}
	if len(plan.Disable) > 0 {
		if err := provider.DisableModels(plan.Disable...); err != nil {
			return err
		}
	}
	if len(plan.Apply)+len(plan.Disable) > 0 {
		provider.Changed()
		b, err = build()
		if err != nil {
			return err
		}
		source := next.Source
		next, err = codexCatalogRecord(b)
		if err != nil {
			return err
		}
		next.Source = source
	}
	if len(cur) > 0 {
		// Version 1 recorded merged output, so it cannot safely identify
		// generated deltas. Migrate it as an initial adoption.
		b, err = codexCatalogMerge(cur, b, was, &next)
		if err != nil {
			return err
		}
	}
	record, err := json.Marshal(next)
	if err != nil {
		return err
	}
	// Re-read before replacing: a save made during generation is not ours.
	latest, err = edit.Read(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, cur) {
		return fmt.Errorf("the catalog changed while syncing; retry after saving it")
	}
	if err := os.MkdirAll(filepath.Dir(baseline), 0o700); err != nil {
		return err
	}
	changed := !bytes.Equal(cur, b)
	_, statErr := os.Stat(path)
	if changed {
		if err := edit.WriteAtomic(path, b); err != nil {
			return err
		}
	}
	if bytes.Equal(old, record) {
		return nil
	}
	if err := edit.WriteAtomic(baseline, record); err != nil {
		if !changed {
			return err
		}
		// An editor may save after our catalog write. A failed baseline
		// must not roll that newer content back to the earlier file.
		latest, readErr := edit.Read(path)
		if readErr != nil || !bytes.Equal(latest, b) {
			return errors.Join(err, readErr, fmt.Errorf("catalog changed before rollback; keeping its current contents"))
		}
		if os.IsNotExist(statErr) {
			return errors.Join(err, edit.Remove(path))
		}
		return errors.Join(err, edit.WriteAtomic(path, cur))
	}
	return nil
}

// ImportCodexCatalog imports selected fields of one model, without taking
// ownership of a custom catalog or changing model_catalog_json. The next
// successful sync records the resulting generated state.
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
	if was.Version >= 2 {
		if old, ok := was.Models[slug]; ok {
			// Also apply an explicit import whose value equals today's
			// default; the generated JSON itself may not have changed.
			for field := range changes {
				old.Settings[field] = "import"
			}
			was.Models[slug] = old
			b, err := json.Marshal(was)
			if err != nil {
				return err
			}
			return edit.WriteAtomic(baseline, b)
		}
	}
	return nil
}

// A disconnect may remove a generated list, but never local customizations.
// Unknown or malformed state is kept for explicit recovery too.
func codexCatalogPending(path string) bool {
	cur, err := edit.Read(path)
	if err != nil {
		return true
	}
	if len(cur) == 0 {
		return false
	}
	if _, _, _, err := codexCatalogRead(cur); err != nil {
		return true
	}
	raw, err := edit.Read(codexBaselinePath(path))
	if err != nil || len(raw) == 0 {
		return true
	}
	var was codexCatalogBaseline
	if json.Unmarshal(raw, &was) != nil || !was.valid() || was.Version < 2 {
		return true
	}
	return len(was.Archive) > 0 || codexFieldHash(cur) != was.Hash
}
