package configstore

import (
	"encoding/json"
	"strings"

	"github.com/taozhang/llmrelay/internal/repo"
	"github.com/taozhang/llmrelay/internal/schema"
)

// buildSnapshot assembles an immutable Snapshot from DB rows. It performs the
// same row→entry conversions as config.ts's rowToConfigEntry / rowToEntry,
// including auth-header normalization, responsesMode extraction from
// extraFields, and alias target parsing.
func buildSnapshot(providerRows []schema.ConsoleProvider, aliasRows []schema.ModelAlias) *Snapshot {
	snap := NewSnapshot()

	for _, row := range providerRows {
		entry := providerRowToEntry(row)
		if entry == nil {
			continue
		}
		snap.providers[row.ChannelName] = entry
		if entry.ProviderUUID != "" {
			snap.uuidIndex[entry.ProviderUUID] = row.ChannelName
		}
	}

	for _, row := range aliasRows {
		if !rowBool(row.Enabled) {
			continue
		}
		targets, err := repo.ParseTargets(row)
		if err != nil {
			// Skip a malformed alias rather than failing the whole load; the
			// original logs and continues.
			continue
		}
		convTargets := make([]AliasTarget, 0, len(targets))
		for _, t := range targets {
			convTargets = append(convTargets, AliasTarget{Provider: t.Provider, Model: t.Model})
		}
		snap.aliases[row.Alias] = &AliasEntry{
			Alias:    row.Alias,
			Provider: row.Provider,
			Model:    row.Model,
			Targets:  convTargets,
			Visible:  rowBool(row.Visible),
			Enabled:  true,
		}
	}

	return snap
}

// providerRowToEntry converts a console_providers row to a ConfigEntry. Returns
// nil for a row with an invalid empty auth value (mirrors the original's throw,
// which here we treat as skip).
func providerRowToEntry(row schema.ConsoleProvider) *ConfigEntry {
	// Empty auth value is invalid (auth header set but value empty).
	if row.AuthValue != nil && *row.AuthValue == "" {
		return nil
	}

	t := OpenAI
	if row.Type == string(Anthropic) {
		t = Anthropic
	}

	entry := &ConfigEntry{
		Type:              t,
		TargetBaseURL:     row.TargetBaseURL,
		Priority:          row.Priority,
		Enabled:           row.Enabled != 0,
		RoutingVisibility: normalizeVisibility(row.RoutingVisibility),
		ProviderUUID:      row.ProviderUUID,
		// Default responsesMode (the OpenAI-specific value is overridden below).
		ResponsesMode: DefaultResponsesMode,
	}

	if row.SystemPrompt != nil {
		entry.SystemPrompt = *row.SystemPrompt
	}
	if row.AuthValue != nil {
		entry.Auth = &AuthConfig{
			Header: normalizeAuthHeader(row.AuthHeader, t),
			Value:  *row.AuthValue,
		}
	}

	entry.Models = parseModelsJSON(row.ModelsJSON)
	entry.ExtraFields = parseExtraFields(row.ExtraFieldsJSON)

	if t == OpenAI {
		entry.ResponsesMode = extractResponsesMode(entry.ExtraFields)
	}
	return entry
}

func rowBool(v int) bool { return v != 0 }

func normalizeVisibility(v string) RoutingVisibility {
	if v == string(VisibilityExplicitOnly) {
		return VisibilityExplicitOnly
	}
	return VisibilityDirect
}

func normalizeAuthHeader(h *string, t UpstreamType) string {
	if h == nil || *h == "" {
		return defaultAuthHeader(t)
	}
	switch *h {
	case "x-api-key", "authorization":
		return *h
	}
	// The original throws here; we fall back to the default to keep the load
	// resilient (a bad row shouldn't break the whole snapshot).
	return defaultAuthHeader(t)
}

func defaultAuthHeader(t UpstreamType) string {
	if t == Anthropic {
		return "x-api-key"
	}
	return "authorization"
}

func parseModelsJSON(jsonStr string) []ModelConfig {
	if strings.TrimSpace(jsonStr) == "" {
		return nil
	}
	var raw []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil
	}
	out := make([]ModelConfig, 0, len(raw))
	for _, m := range raw {
		model, _ := m["model"].(string)
		if model == "" {
			continue
		}
		mc := ModelConfig{Model: model, Extra: m}
		if ctx, ok := m["context"]; ok {
			if f, ok := ctx.(float64); ok && f > 0 {
				c := int(f)
				mc.Context = &c
			}
		}
		out = append(out, mc)
	}
	return out
}

func parseExtraFields(jsonStr string) map[string]interface{} {
	jsonStr = strings.TrimSpace(jsonStr)
	if jsonStr == "" {
		return nil
	}
	var fields map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &fields); err != nil {
		return nil
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// extractResponsesMode pulls the responsesMode out of extraFields (or returns
// the default). Mirrors getOpenAiResponsesMode.
func extractResponsesMode(extra map[string]interface{}) ResponsesMode {
	if v, ok := extra["responsesMode"]; ok {
		if s, ok := v.(string); ok {
			switch ResponsesMode(s) {
			case ResponsesNative, ResponsesChatCompat, ResponsesDisabled:
				return ResponsesMode(s)
			}
		}
	}
	return DefaultResponsesMode
}

// ModelID returns the id of a model config (helper used by routing).
func ModelID(m ModelConfig) string { return m.Model }
