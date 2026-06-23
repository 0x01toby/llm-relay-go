// Package routing resolves incoming request paths + model names to upstream
// provider targets. It is a direct port of the resolution functions in
// src/config.ts (resolveRoute, resolveRoutesByModel, the fallback resolvers).
//
// The routing model is provider/channel-centric, not a global model pool:
//
//   - Explicit prefix:  /providers/{channel}/...  → that channel
//   - Model routing:    /v1/{path} with body.model → channels listing that model
//   - Alias:            body.model is a configured alias → its bound target(s)
//   - Failover:         secondary candidates by priority / custom fallback / any-model
//
// Path-splicing rules (buildRouteResult):
//   - OpenAI endpoints: strip "/v1" (the targetBaseUrl must include /v1)
//   - Anthropic endpoints: keep "/v1" (targetBaseUrl excludes it)
package routing

import (
	"regexp"
	"sort"
	"strings"

	"github.com/taozhang/llmrelay/internal/configstore"
)

// RouteResult is a resolved upstream target. Mirrors RouteResult in config.ts.
type RouteResult struct {
	ChannelName    string
	Type           configstore.UpstreamType
	TargetURL      string
	SystemPrompt   string
	Auth           *configstore.AuthConfig
	ResponsesMode  configstore.ResponsesMode
	ResolvedModel  string // real upstream model when request used an alias
	VirtualModel   string // the alias name that selected this target
}

// Resolver resolves routes against a configstore snapshot. Construct one per
// snapshot (snapshots are immutable, so a Resolver is safe to reuse for the
// lifetime of that snapshot).
type Resolver struct {
	snap *configstore.Snapshot
}

// NewResolver builds a Resolver bound to snap.
func NewResolver(snap *configstore.Snapshot) *Resolver { return &Resolver{snap: snap} }

// Snapshot returns the backing snapshot.
func (r *Resolver) Snapshot() *configstore.Snapshot { return r.snap }

var explicitRouteRe = regexp.MustCompile(`^/providers/([^/]+)(/.*)?$`)

// ResolveRoute handles the explicit-prefix form /providers/{channel}/... .
// Returns nil if the channel doesn't exist, is disabled, or is explicit-only.
func (r *Resolver) ResolveRoute(pathname, search string) *RouteResult {
	m := explicitRouteRe.FindStringSubmatch(pathname)
	if m == nil {
		return nil
	}
	channel := m[1]
	entry := r.snap.Provider(channel)
	if entry == nil || !entry.Enabled || !isDirectEntry(entry) {
		return nil
	}
	path := m[2]
	if path == "" {
		path = "/"
	}
	return r.buildRouteResult(channel, entry, path, search)
}

// IsModelRoutedPath reports whether pathname is /v1 or starts with /v1/.
func IsModelRoutedPath(pathname string) bool {
	return pathname == "/v1" || strings.HasPrefix(pathname, "/v1/")
}

// InferExpectedType guesses the provider type from the endpoint path: /v1/messages
// is Anthropic, other /v1/* are OpenAI-compatible.
func InferExpectedType(pathname string) configstore.UpstreamType {
	if pathname == "/v1/messages" || strings.HasPrefix(pathname, "/v1/messages?") {
		return configstore.Anthropic
	}
	if IsModelRoutedPath(pathname) {
		return configstore.OpenAI
	}
	return ""
}

// ResolveRoutesByModel resolves /v1/* requests by the request's model field.
// If the model is an alias, only the alias's bound targets are returned (the
// alias does NOT expand to other channels with the same model name). Otherwise
// all channels listing the model (filtered by expected type) are returned by
// priority. Returns nil for non-/v1 paths or no matches.
func (r *Resolver) ResolveRoutesByModel(pathname, search, model string, forcedType configstore.UpstreamType) []*RouteResult {
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

	// Alias takes precedence: resolve only to its targets.
	if alias := r.snap.Alias(model); alias != nil {
		var routes []*RouteResult
		targets := alias.Targets
		if len(targets) == 0 {
			targets = []configstore.AliasTarget{{Provider: alias.Provider, Model: alias.Model}}
		}
		for _, t := range targets {
			rt := r.resolveExplicitTarget(pathname, search, t, expectedType, false)
			if rt != nil {
				rt.VirtualModel = model
				routes = append(routes, rt)
			}
		}
		return dedupe(routes)
	}

	// Otherwise match channels listing this model.
	var routes []*RouteResult
	for _, m := range r.findRoutesByModel(model, expectedType, false) {
		routes = append(routes, r.buildRouteResult(m.channel, m.entry, pathname, search))
	}
	return routes
}

// ResolveRouteByModel returns the first candidate (or nil).
func (r *Resolver) ResolveRouteByModel(pathname, search, model string, forcedType configstore.UpstreamType) *RouteResult {
	rs := r.ResolveRoutesByModel(pathname, search, model, forcedType)
	if len(rs) == 0 {
		return nil
	}
	return rs[0]
}

// modelMatch is an internal {channel, entry} pair from findRoutesByModel.
type modelMatch struct {
	channel string
	entry   *configstore.ConfigEntry
}

// findRoutesByModel returns channels listing model, sorted by priority desc
// then channel name asc, skipping disabled and (by default) explicit-only
// entries. Mirrors findRoutesByModel in config.ts.
func (r *Resolver) findRoutesByModel(model string, expectedType configstore.UpstreamType, includeExplicitOnly bool) []modelMatch {
	providers := r.snap.Providers()
	channels := make([]string, 0, len(providers))
	for ch := range providers {
		channels = append(channels, ch)
	}
	sort.Slice(channels, func(i, j int) bool {
		a, b := providers[channels[i]], providers[channels[j]]
		pa, pb := a.Priority, b.Priority
		if pa != pb {
			return pa > pb
		}
		return channels[i] < channels[j]
	})

	var matches []modelMatch
	for _, ch := range channels {
		entry := providers[ch]
		if !entry.Enabled {
			continue
		}
		if !includeExplicitOnly && !isDirectEntry(entry) {
			continue
		}
		if expectedType != "" && entry.Type != expectedType {
			continue
		}
		for _, m := range entry.Models {
			if m.Model == model {
				matches = append(matches, modelMatch{channel: ch, entry: entry})
				break
			}
		}
	}
	return matches
}

// resolveExplicitTarget resolves an alias target (provider may be a UUID).
func (r *Resolver) resolveExplicitTarget(pathname, search string, target configstore.AliasTarget, expectedType configstore.UpstreamType, requireListedModel bool) *RouteResult {
	channel := r.snap.ChannelForUUID(target.Provider)
	entry := r.snap.Provider(channel)
	if entry == nil || !entry.Enabled || entry.Type != expectedType {
		return nil
	}
	if requireListedModel {
		found := false
		for _, m := range entry.Models {
			if m.Model == target.Model {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	rt := r.buildRouteResult(channel, entry, pathname, search)
	if rt != nil {
		rt.ResolvedModel = target.Model
	}
	return rt
}

// buildRouteResult splices the target URL per the provider type. Mirrors
// buildRouteResult in config.ts.
func (r *Resolver) buildRouteResult(channel string, entry *configstore.ConfigEntry, path, search string) *RouteResult {
	t := entry.Type
	if t == "" {
		t = configstore.OpenAI
	}
	normalized := path
	if IsModelRoutedPath(path) && t == configstore.OpenAI {
		// Strip "/v1": caller's targetBaseUrl must include /v1.
		normalized = path[3:]
		if normalized == "" {
			normalized = "/"
		}
	}
	rt := &RouteResult{
		ChannelName:  channel,
		Type:         t,
		TargetURL:    entry.TargetBaseURL + normalized + search,
		SystemPrompt: entry.SystemPrompt,
		Auth:         entry.Auth,
	}
	if t == configstore.OpenAI {
		rt.ResponsesMode = entry.ResponsesMode
	}
	return rt
}

// dedupe removes routes with identical channel:model:url keys.
func dedupe(routes []*RouteResult) []*RouteResult {
	seen := map[string]bool{}
	out := routes[:0]
	for _, rt := range routes {
		key := rt.ChannelName + ":" + rt.ResolvedModel + ":" + rt.TargetURL
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, rt)
	}
	return out
}

func isDirectEntry(entry *configstore.ConfigEntry) bool {
	v := entry.RoutingVisibility
	if v == "" {
		return true
	}
	return v == configstore.VisibilityDirect
}
