package routing

import (
	"strings"

	"github.com/taozhang/llmrelay/internal/configstore"
)

// This file ports the failover/fallback resolvers and the model/provider
// listing functions from config.ts.

// ResolveRoutesForFallbackModels resolves custom fallback model specifiers
// (each is either an alias name or a "channel:model" pair). Used by the
// failover engine to build secondary candidates. Mirrors
// resolveRoutesForFallbackModels.
func (r *Resolver) ResolveRoutesForFallbackModels(pathname, search string, fallbackModels []string, forcedType configstore.UpstreamType) []*RouteResult {
	if !IsModelRoutedPath(pathname) {
		return nil
	}
	expectedType := forcedType
	if expectedType == "" {
		expectedType = InferExpectedType(pathname)
	}
	if expectedType == "" {
		return nil
	}

	var routes []*RouteResult
	for _, fb := range fallbackModels {
		// Alias first.
		if alias := r.snap.Alias(fb); alias != nil {
			routes = append(routes, r.resolveAliasFallback(pathname, search, fb, expectedType)...)
			continue
		}
		// channel:model pair.
		if rt := r.resolveChannelModelFallback(pathname, search, fb, expectedType); rt != nil {
			routes = append(routes, rt)
		}
	}
	return dedupe(routes)
}

func (r *Resolver) resolveAliasFallback(pathname, search, alias string, expectedType configstore.UpstreamType) []*RouteResult {
	a := r.snap.Alias(alias)
	if a == nil {
		return nil
	}
	targets := a.Targets
	if len(targets) == 0 {
		targets = []configstore.AliasTarget{{Provider: a.Provider, Model: a.Model}}
	}
	var routes []*RouteResult
	for _, t := range targets {
		rt := r.resolveExplicitTarget(pathname, search, t, expectedType, false)
		if rt != nil {
			rt.VirtualModel = alias
			routes = append(routes, rt)
		}
	}
	return routes
}

// resolveChannelModelFallback parses a "channelName:model" or "uuid:model" pair.
func (r *Resolver) resolveChannelModelFallback(pathname, search, spec string, expectedType configstore.UpstreamType) *RouteResult {
	idx := strings.Index(spec, ":")
	if idx <= 0 || idx == len(spec)-1 {
		return nil
	}
	channelOrUUID := strings.TrimSpace(spec[:idx])
	model := strings.TrimSpace(spec[idx+1:])
	if channelOrUUID == "" || model == "" {
		return nil
	}
	channel := r.snap.ChannelForUUID(channelOrUUID)
	entry := r.snap.Provider(channel)
	if entry == nil || !entry.Enabled || entry.Type != expectedType {
		return nil
	}
	// Model must be listed.
	listed := false
	for _, m := range entry.Models {
		if m.Model == model {
			listed = true
			break
		}
	}
	if !listed {
		return nil
	}
	return r.resolveExplicitTarget(pathname, search, configstore.AliasTarget{Provider: channel, Model: model}, expectedType, false)
}

// ResolveRoutesForAnyModelFallback returns one route per model listed on every
// enabled direct-routing channel of the expected type (the "any_model"
// failover mode). Mirrors resolveRoutesForAnyModelFallback.
func (r *Resolver) ResolveRoutesForAnyModelFallback(pathname, search string, forcedType configstore.UpstreamType) []*RouteResult {
	if !IsModelRoutedPath(pathname) {
		return nil
	}
	expectedType := forcedType
	if expectedType == "" {
		expectedType = InferExpectedType(pathname)
	}
	if expectedType == "" {
		return nil
	}

	var routes []*RouteResult
	for _, m := range r.sortedProviders() {
		if !m.entry.Enabled || !isDirectEntry(m.entry) || m.entry.Type != expectedType {
			continue
		}
		for _, model := range m.entry.Models {
			rt := r.buildRouteResult(m.channel, m.entry, pathname, search)
			if rt != nil {
				rt.ResolvedModel = model.Model
				routes = append(routes, rt)
			}
		}
	}
	return routes
}

// sortedProviders returns all providers as modelMatch, sorted by priority desc
// then name asc.
func (r *Resolver) sortedProviders() []modelMatch {
	providers := r.snap.Providers()
	out := make([]modelMatch, 0, len(providers))
	for ch, e := range providers {
		out = append(out, modelMatch{channel: ch, entry: e})
	}
	sortByPriority(out)
	return out
}

func sortByPriority(ms []modelMatch) {
	// Stable sort by priority desc, then channel asc (matches the TS sort).
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0; j-- {
			a, b := ms[j-1], ms[j]
			if a.entry.Priority > b.entry.Priority {
				break
			}
			if a.entry.Priority == b.entry.Priority && a.channel <= b.channel {
				break
			}
			ms[j-1], ms[j] = b, a
		}
	}
}

// --- Listing (for /v1/models and the console) ---

// ModelInfo is one model exposed to clients. Mirrors ModelInfo in config.ts.
type ModelInfo struct {
	ID          string
	ChannelName string
	Type        configstore.UpstreamType
	Context     *int
}

// Models returns the public model list: all direct-routing models, then
// visible aliases, de-duplicated by "model:type". Mirrors getModels.
func (r *Resolver) Models() []ModelInfo {
	seen := map[string]bool{}
	var models []ModelInfo

	for _, m := range r.sortedProviders() {
		if !m.entry.Enabled || !isDirectEntry(m.entry) {
			continue
		}
		t := m.entry.Type
		if t == "" {
			t = configstore.OpenAI
		}
		for _, model := range m.entry.Models {
			key := model.Model + ":" + string(t)
			if seen[key] {
				continue
			}
			seen[key] = true
			models = append(models, ModelInfo{
				ID:          model.Model,
				ChannelName: m.channel,
				Type:        t,
				Context:     model.Context,
			})
		}
	}

	// Append visible aliases.
	for alias, a := range r.snap.Aliases() {
		if !a.Visible {
			continue
		}
		firstType, firstCtx := r.firstAliasType(a)
		if firstType == "" {
			continue
		}
		key := alias + ":" + string(firstType)
		if seen[key] {
			continue
		}
		seen[key] = true
		models = append(models, ModelInfo{
			ID:          alias,
			ChannelName: "virtual-route",
			Type:        firstType,
			Context:     firstCtx,
		})
	}
	return models
}

func (r *Resolver) firstAliasType(a *configstore.AliasEntry) (configstore.UpstreamType, *int) {
	targets := a.Targets
	if len(targets) == 0 {
		targets = []configstore.AliasTarget{{Provider: a.Provider, Model: a.Model}}
	}
	for _, t := range targets {
		ch := r.snap.ChannelForUUID(t.Provider)
		entry := r.snap.Provider(ch)
		if entry == nil || !entry.Enabled {
			continue
		}
		for _, m := range entry.Models {
			if m.Model == t.Model {
				et := entry.Type
				if et == "" {
					et = configstore.OpenAI
				}
				return et, m.Context
			}
		}
	}
	return "", nil
}

// ChannelModels returns all models grouped by channel (no de-dup, includes
// explicit-only channels). Mirrors getChannelModels.
func (r *Resolver) ChannelModels() []ModelInfo {
	var models []ModelInfo
	for _, m := range r.sortedProviders() {
		if !m.entry.Enabled {
			continue
		}
		t := m.entry.Type
		if t == "" {
			t = configstore.OpenAI
		}
		for _, model := range m.entry.Models {
			models = append(models, ModelInfo{
				ID:          model.Model,
				ChannelName: m.channel,
				Type:        t,
				Context:     model.Context,
			})
		}
	}
	return models
}
