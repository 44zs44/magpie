package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/codexcat"
	"github.com/yetone/magpie/internal/settings"
)

// CodexModelEditState returns only explicitly configured values, not the
// vendor's defaults. Catalog synchronization compares these with its last
// successful write to distinguish a settings edit from an upstream update.
func CodexModelEditState(ref string) (map[string]json.RawMessage, error) {
	s, err := settings.SharedChecked()
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	put := func(field string, value any) { out[field], _ = json.Marshal(value) }
	var levels []string
	if IsGroupRef(ref) {
		g, _, found := FindGroup(ref)
		if !found {
			return nil, fmt.Errorf("no routing group %q", ref)
		}
		ref = GroupPrefix + g.ID
		if g.Context != 0 {
			put("context_window", g.Context)
		}
		if g.Compact > 0 {
			put("auto_compact_token_limit", g.Compact)
		}
		levels = g.Levels
	} else {
		p, model, err := splitRef(ref)
		if err != nil {
			return nil, err
		}
		ref = p.ID + "/" + model
		if n := p.Contexts[model]; n > 0 {
			put("context_window", n)
		}
		if n := s.ModelCompacts[ref]; n > 0 {
			put("auto_compact_token_limit", n)
		}
		levels = s.ModelEfforts[ref]
		if s.ModelReasonless[ref] {
			put("supported_reasoning_levels", []any{})
		}
		if images, ok := s.ModelImages[ref]; ok {
			modalities := []string{"text"}
			if images {
				modalities = append(modalities, "image")
			}
			put("input_modalities", modalities)
		}
	}
	if len(levels) > 0 {
		list := make([]map[string]string, 0, len(levels))
		for _, l := range levels {
			list = append(list, map[string]string{"effort": l})
		}
		put("supported_reasoning_levels", list)
	}
	if text, ok := s.ModelCodexInstructions[ref]; ok {
		put("base_instructions", text)
	}
	return out, nil
}

// ApplyCodexModelEdits imports the catalog fields that have an existing
// configuration meaning. Every field is checked before any file is saved;
// null removes its override. A failed save leaves the caller's catalog and
// baseline untouched so an interrupted import can be retried.
func ApplyCodexModelEdits(edits map[string]map[string]json.RawMessage) error {
	s, err := settings.LoadChecked()
	if err != nil {
		return err
	}
	before, _ := json.Marshal(s)
	providers := map[string]*Provider{}
	groups := map[string]Group{}
	providerChanges, groupChanges := map[string]bool{}, map[string]bool{}
	for _, ref := range slices.Sorted(maps.Keys(edits)) {
		fields := edits[ref]
		var p *Provider
		var model string
		var group *Group
		if IsGroupRef(ref) {
			g, _, found := FindGroup(ref)
			if !found {
				return fmt.Errorf("no routing group %q", ref)
			}
			ref = GroupPrefix + g.ID
			group = &g
		} else {
			p, model, err = splitRef(ref)
			if err != nil {
				return err
			}
			if model == "*" {
				return fmt.Errorf("a catalog edit names one model, not %q", ref)
			}
			if staged := providers[p.ID]; staged != nil {
				p = staged
			}
			ref = p.ID + "/" + model
			providers[p.ID] = p
		}
		for field, raw := range fields {
			clear := len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
			if !clear && p != nil && !p.serves(model) {
				return fmt.Errorf("%s has no model %s", p.ID, model)
			}
			switch field {
			case "base_instructions":
				var text string
				if !clear && json.Unmarshal(raw, &text) != nil {
					return fmt.Errorf("%s: base_instructions must be text or null", ref)
				}
				if len(text) > maxModelSystemPrompt || strings.ContainsRune(text, 0) {
					return fmt.Errorf("%s: base_instructions must be at most %d bytes and contain no NUL", ref, maxModelSystemPrompt)
				}
				if strings.TrimSpace(text) == "" {
					delete(s.ModelCodexInstructions, ref)
				} else {
					if s.ModelCodexInstructions == nil {
						s.ModelCodexInstructions = map[string]string{}
					}
					s.ModelCodexInstructions[ref] = text
				}
			case "context_window", "auto_compact_token_limit":
				var n int
				if !clear && (json.Unmarshal(raw, &n) != nil || n < 0) {
					return fmt.Errorf("%s: %s must be a nonnegative integer or null", ref, field)
				}
				if group != nil {
					if field == "context_window" && group.Context != n {
						group.Context, groupChanges[group.ID] = n, true
					} else if field == "auto_compact_token_limit" && group.Compact != n {
						group.Compact, groupChanges[group.ID] = n, true
					}
				} else if field == "context_window" {
					if p.Contexts[model] != n {
						providerChanges[p.ID] = true
						if n == 0 {
							delete(p.Contexts, model)
						} else {
							if p.Contexts == nil {
								p.Contexts = map[string]int{}
							}
							p.Contexts[model] = n
						}
					}
				} else if n == 0 {
					delete(s.ModelCompacts, ref)
				} else {
					if s.ModelCompacts == nil {
						s.ModelCompacts = map[string]int{}
					}
					s.ModelCompacts[ref] = n
				}
			case "supported_reasoning_levels":
				var rows []struct {
					Effort string `json:"effort"`
				}
				if !clear && json.Unmarshal(raw, &rows) != nil {
					return fmt.Errorf("%s: supported_reasoning_levels must be an array or null", ref)
				}
				var levels []string
				for _, row := range rows {
					if row.Effort == "" {
						return fmt.Errorf("%s: a reasoning level needs an effort", ref)
					}
					levels = append(levels, row.Effort)
				}
				if group != nil {
					if !clear && len(levels) == 0 {
						return fmt.Errorf("%s: an empty group level list restores shared levels; use null", ref)
					}
					levels, err = CleanLevels(levels)
					if err == nil && !slices.Equal(group.Levels, levels) {
						group.Levels, groupChanges[group.ID] = levels, true
					}
				} else {
					levels, err = keptEfforts(p, model, levels)
					delete(s.ModelEfforts, ref)
					delete(s.ModelReasonless, ref)
					if !clear && len(rows) == 0 {
						if s.ModelReasonless == nil {
							s.ModelReasonless = map[string]bool{}
						}
						s.ModelReasonless[ref] = true
					} else if len(levels) > 0 {
						if s.ModelEfforts == nil {
							s.ModelEfforts = map[string][]string{}
						}
						s.ModelEfforts[ref] = levels
					}
				}
				if err != nil {
					return fmt.Errorf("%s: %w", ref, err)
				}
			case "input_modalities":
				if group != nil {
					return fmt.Errorf("%s: image support comes from the group's members; edit the provider model", ref)
				}
				var modes []string
				if !clear && (json.Unmarshal(raw, &modes) != nil || !slices.Contains(modes, "text")) {
					return fmt.Errorf("%s: input_modalities must contain text, optionally image, or be null", ref)
				}
				for _, mode := range modes {
					if mode != "text" && mode != "image" {
						return fmt.Errorf("%s: unsupported input modality %q", ref, mode)
					}
				}
				images := slices.Contains(modes, "image")
				vendor, known := vendorSees(p, model)
				if clear || known && vendor == images {
					delete(s.ModelImages, ref)
				} else {
					if s.ModelImages == nil {
						s.ModelImages = map[string]bool{}
					}
					s.ModelImages[ref] = images
				}
			default:
				return fmt.Errorf("%s: catalog field %q cannot be imported", ref, field)
			}
		}
		if group != nil {
			groups[group.ID] = *group
		}
	}
	if _, err := read(); err != nil {
		return err
	}
	after, _ := json.Marshal(s)
	settingsChanged := !bytes.Equal(before, after)
	if settingsChanged || len(providerChanges)+len(groupChanges) > 0 {
		if err := codexcat.PrepareInstructions(); err != nil {
			return err
		}
	}
	if settingsChanged {
		if err := settings.Save(s); err != nil {
			return err
		}
	}
	// Save owns provider/account normalization and catalog invalidation.
	// Retrying after a write failure is harmless: only changed values save.
	for _, id := range slices.Sorted(maps.Keys(providerChanges)) {
		if err := Save(*providers[id]); err != nil {
			return touchedIf(settingsChanged, err)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(groupChanges)) {
		if err := SaveGroup(groups[id]); err != nil {
			return touchedIf(settingsChanged, err)
		}
	}
	if settingsChanged && len(providerChanges)+len(groupChanges) == 0 {
		catalog.Touched()
	}
	return nil
}
