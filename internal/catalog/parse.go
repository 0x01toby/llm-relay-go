package catalog

import (
	"encoding/json"

	"github.com/taozhang/llmrelay/internal/pricing"
)

// This file decodes models.dev's catalog. The catalog has two shapes we accept:
//
//  1. Raw (as served by models.dev): a top-level object keyed by *provider* id,
//     each value holding a nested "models" map keyed by model id:
//       { "anthropic": { "models": { "claude-opus-4-5": { "cost": {...}, "limit": {...} } } } }
//  2. Flattened (produced by flattenModelsDev for caching): a top-level object
//     keyed by *model* id, where first-party providers win on conflicts:
//       { "claude-opus-4-5": { "cost": {...}, "limit": {...} } }
//
// The same model id (e.g. "claude-opus-4-5") is offered by many providers. We
// collapse to one entry per model id, preferring the first-party provider
// (anthropic / openai) whose pricing is the most complete and authoritative,
// then any provider that has more pricing fields populated.

// providerPriority ranks providers so first-party sources win dedup. Lower
// number = higher priority. Unknown providers get the default (100).
var providerPriority = map[string]int{
	"anthropic": 0,
	"openai":    0,
	"google":    1, // first-party for gemini models
}

const defaultProviderPriority = 100

// flattenModelsDev turns the raw provider-keyed catalog into a model-keyed
// object, resolving duplicate model ids by provider priority (and, within the
// same priority, by which entry has more pricing fields). The returned object is
// JSON-serializable as {modelID: {cost, limit, ...}}.
//
// If the input is already model-keyed (each top-level value is a model object
// with no nested "models" map, e.g. from the DB/embed cache), it is returned
// unchanged so round-tripping works.
func flattenModelsDev(raw map[string]json.RawMessage) map[string]json.RawMessage {
	// Detect the already-flattened form: if no top-level entry has a "models"
	// field, the object is keyed by model id already.
	hasProviderShape := false
	for _, provRaw := range raw {
		if hasNestedModels(provRaw) {
			hasProviderShape = true
			break
		}
	}
	if !hasProviderShape {
		return raw
	}

	type entry struct {
		priority  int
		raw       json.RawMessage // the model object
		costCount int             // populated cost fields (more = better)
	}
	best := map[string]entry{}

	for providerID, provRaw := range raw {
		var prov struct {
			Models map[string]json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(provRaw, &prov); err != nil || prov.Models == nil {
			continue
		}
		prio, ok := providerPriority[providerID]
		if !ok {
			prio = defaultProviderPriority
		}
		for modelID, modelRaw := range prov.Models {
			cur, exists := best[modelID]
			newEntry := entry{priority: prio, raw: modelRaw, costCount: countCostFields(modelRaw)}
			if !exists {
				best[modelID] = newEntry
				continue
			}
			// Replace if this provider is higher priority, or same priority but
			// has more cost detail (e.g. cache_read/cache_write present).
			if prio < cur.priority || (prio == cur.priority && newEntry.costCount > cur.costCount) {
				best[modelID] = newEntry
			}
		}
	}

	out := make(map[string]json.RawMessage, len(best))
	for modelID, e := range best {
		out[modelID] = e.raw
	}
	return out
}

// hasNestedModels reports whether a JSON object has a "models" sub-object,
// i.e. it looks like a provider entry rather than a flat model entry.
func hasNestedModels(v json.RawMessage) bool {
	var probe struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(v, &probe); err != nil {
		return false
	}
	s := string(probe.Models)
	return s != "" && s != "null"
}

// countCostFields returns how many numeric fields a model's cost object has, so
// dedup can prefer entries with richer pricing (e.g. cache_read/cache_write).
func countCostFields(modelRaw json.RawMessage) int {
	var m struct {
		Cost map[string]json.RawMessage `json:"cost"`
	}
	if err := json.Unmarshal(modelRaw, &m); err != nil || m.Cost == nil {
		return 0
	}
	// Only count the price-relevant scalar fields, not nested "tiers".
	n := 0
	for _, k := range []string{"input", "output", "cache_read", "cache_write"} {
		if _, ok := m.Cost[k]; ok {
			n++
		}
	}
	return n
}

// parseModelsDev accepts either the raw provider-keyed catalog or the flattened
// model-keyed form and returns context-window + pricing maps keyed by model id.
func parseModelsDev(data []byte) (map[string]int, map[string]*pricing.ModelPricing, error) {
	// Try the raw provider-keyed shape first: a sample provider entry has a
	// "models" sub-object. If the top-level values don't look like providers
	// (no "models" key anywhere), treat the object as already flattened.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, nil, err
	}

	models := flattenModelsDev(top)
	ctxCache := make(map[string]int, len(models))
	priceCache := make(map[string]*pricing.ModelPricing, len(models))

	for modelID, modelRaw := range models {
		var m struct {
			Limit *struct {
				Context int `json:"context"`
			} `json:"limit"`
			Cost map[string]json.RawMessage `json:"cost"`
		}
		if err := json.Unmarshal(modelRaw, &m); err != nil {
			continue
		}
		if m.Limit != nil && m.Limit.Context > 0 {
			ctxCache[modelID] = m.Limit.Context
		}
		if p := costToPricing(m.Cost); p != nil {
			priceCache[modelID] = p
		}
	}
	return ctxCache, priceCache, nil
}

// costToPricing maps a models.dev cost object to a ModelPricing. Values are
// USD per 1M tokens. Returns nil if neither input nor output is present.
func costToPricing(cost map[string]json.RawMessage) *pricing.ModelPricing {
	if len(cost) == 0 {
		return nil
	}
	p := &pricing.ModelPricing{}
	p.Input = rawNum(cost, "input")
	p.Output = rawNum(cost, "output")
	p.CacheRead = rawNum(cost, "cache_read")
	p.CacheWrite = rawNum(cost, "cache_write")
	if p.Input == nil && p.Output == nil {
		return nil
	}
	return p
}

// rawNum decodes a JSON number from a RawMessage, returning nil if absent or
// not a number (some cost fields like "tiers" are objects).
func rawNum(m map[string]json.RawMessage, key string) *float64 {
	raw, ok := m[key]
	if !ok || len(raw) == 0 {
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	return &f
}
