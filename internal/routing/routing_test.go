package routing

import (
	"testing"

	"github.com/taozhang/llmrelay/internal/configstore"
)

// helper builders for terse test setup.
func openAIProvider(channel, baseURL string, models ...string) *configstore.ConfigEntry {
	return provider(channel, configstore.OpenAI, baseURL, models...)
}

func anthropicProvider(channel, baseURL string, models ...string) *configstore.ConfigEntry {
	return provider(channel, configstore.Anthropic, baseURL, models...)
}

func provider(channel string, t configstore.UpstreamType, baseURL string, models ...string) *configstore.ConfigEntry {
	e := &configstore.ConfigEntry{
		Type: t, TargetBaseURL: baseURL, Enabled: true,
		RoutingVisibility: configstore.VisibilityDirect,
	}
	for _, m := range models {
		e.Models = append(e.Models, configstore.ModelConfig{Model: m})
	}
	return e
}

func storeWith(providers map[string]*configstore.ConfigEntry, aliases map[string]*configstore.AliasEntry) *Resolver {
	s := configstore.NewStore(nil, nil)
	s.LoadForTest(providers, aliases)
	return NewResolver(s.Snapshot())
}

// --- Explicit prefix routing ---

func TestResolveRoute_ExplicitPrefix(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{
		"my-channel": openAIProvider("my-channel", "https://api.openai.com/v1", "gpt-4o"),
	}, nil)
	rt := r.ResolveRoute("/providers/my-channel/v1/chat/completions", "")
	if rt == nil {
		t.Fatal("expected a route")
	}
	if rt.ChannelName != "my-channel" {
		t.Errorf("channel: %q", rt.ChannelName)
	}
	// OpenAI: /v1 stripped, baseURL already has /v1.
	if rt.TargetURL != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("targetURL: %q", rt.TargetURL)
	}
}

func TestResolveRoute_ExplicitPrefix_AnthropicKeepsV1(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{
		"anth": anthropicProvider("anth", "https://api.anthropic.com", "claude-3"),
	}, nil)
	rt := r.ResolveRoute("/providers/anth/v1/messages", "")
	if rt == nil {
		t.Fatal("expected a route")
	}
	// Anthropic: /v1 kept, baseURL excludes /v1.
	if rt.TargetURL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("targetURL: %q", rt.TargetURL)
	}
}

func TestResolveRoute_ExplicitPrefix_SearchPreserved(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{
		"ch": openAIProvider("ch", "https://x/v1", "gpt-4o"),
	}, nil)
	rt := r.ResolveRoute("/providers/ch/v1/chat/completions", "?foo=bar")
	if rt.TargetURL != "https://x/v1/chat/completions?foo=bar" {
		t.Errorf("targetURL with search: %q", rt.TargetURL)
	}
}

func TestResolveRoute_DisabledChannelSkipped(t *testing.T) {
	e := openAIProvider("ch", "https://x/v1", "gpt-4o")
	e.Enabled = false
	r := storeWith(map[string]*configstore.ConfigEntry{"ch": e}, nil)
	if rt := r.ResolveRoute("/providers/ch/v1/chat/completions", ""); rt != nil {
		t.Error("disabled channel should not route")
	}
}

func TestResolveRoute_ExplicitOnlySkipped(t *testing.T) {
	e := openAIProvider("ch", "https://x/v1", "gpt-4o")
	e.RoutingVisibility = configstore.VisibilityExplicitOnly
	r := storeWith(map[string]*configstore.ConfigEntry{"ch": e}, nil)
	if rt := r.ResolveRoute("/providers/ch/v1/chat/completions", ""); rt != nil {
		t.Error("explicit-only channel should not route via explicit prefix")
	}
}

func TestResolveRoute_UnknownChannelNil(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{}, nil)
	if rt := r.ResolveRoute("/providers/nope/v1/chat/completions", ""); rt != nil {
		t.Error("unknown channel should return nil")
	}
}

func TestResolveRoute_NonProvidersPathNil(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{
		"ch": openAIProvider("ch", "https://x/v1", "gpt-4o"),
	}, nil)
	if rt := r.ResolveRoute("/v1/chat/completions", ""); rt != nil {
		t.Error("non-/providers path should return nil from ResolveRoute")
	}
}

// --- Model routing ---

func TestResolveRoutesByModel_Basic(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{
		"a": openAIProvider("a", "https://a/v1", "gpt-4o"),
		"b": openAIProvider("b", "https://b/v1", "gpt-4o"),
	}, nil)
	routes := r.ResolveRoutesByModel("/v1/chat/completions", "", "gpt-4o", "")
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
}

func TestResolveRoutesByModel_PriorityOrdering(t *testing.T) {
	high := openAIProvider("high", "https://high/v1", "gpt-4o")
	high.Priority = 10
	low := openAIProvider("low", "https://low/v1", "gpt-4o")
	low.Priority = 1
	r := storeWith(map[string]*configstore.ConfigEntry{"low": low, "high": high}, nil)
	routes := r.ResolveRoutesByModel("/v1/chat/completions", "", "gpt-4o", "")
	if len(routes) != 2 || routes[0].ChannelName != "high" {
		t.Errorf("expected high first, got %+v", routes)
	}
}

func TestResolveRoutesByModel_TypeFiltering(t *testing.T) {
	// /v1/messages → anthropic. An openai channel listing the model won't match.
	r := storeWith(map[string]*configstore.ConfigEntry{
		"oai": openAIProvider("oai", "https://oai/v1", "shared-model"),
		"ant": anthropicProvider("ant", "https://ant", "shared-model"),
	}, nil)
	routes := r.ResolveRoutesByModel("/v1/messages", "", "shared-model", "")
	if len(routes) != 1 || routes[0].ChannelName != "ant" {
		t.Errorf("expected only anthropic channel, got %+v", routes)
	}
}

func TestResolveRoutesByModel_ForcedTypeOverridesInference(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{
		"oai": openAIProvider("oai", "https://oai/v1", "m"),
	}, nil)
	// Force openai even on /v1/messages.
	routes := r.ResolveRoutesByModel("/v1/messages", "", "m", configstore.OpenAI)
	if len(routes) != 1 || routes[0].ChannelName != "oai" {
		t.Errorf("forced type should override inference: %+v", routes)
	}
}

func TestResolveRoutesByModel_DisabledSkipped(t *testing.T) {
	e := openAIProvider("ch", "https://ch/v1", "m")
	e.Enabled = false
	r := storeWith(map[string]*configstore.ConfigEntry{"ch": e}, nil)
	if routes := r.ResolveRoutesByModel("/v1/chat/completions", "", "m", ""); len(routes) != 0 {
		t.Error("disabled channel should not match")
	}
}

func TestResolveRoutesByModel_NonV1PathReturnsNil(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{
		"ch": openAIProvider("ch", "https://ch/v1", "m"),
	}, nil)
	if routes := r.ResolveRoutesByModel("/other", "", "m", ""); routes != nil {
		t.Error("non-/v1 path should return nil")
	}
}

// --- Alias routing ---

func TestResolveRoutesByModel_AliasResolvesToTarget(t *testing.T) {
	r := storeWith(
		map[string]*configstore.ConfigEntry{
			"real": openAIProvider("real", "https://real/v1", "gpt-4o"),
		},
		map[string]*configstore.AliasEntry{
			"fast": {Alias: "fast", Provider: "real", Model: "gpt-4o",
				Targets: []configstore.AliasTarget{{Provider: "real", Model: "gpt-4o"}}, Enabled: true, Visible: true},
		},
	)
	routes := r.ResolveRoutesByModel("/v1/chat/completions", "", "fast", "")
	if len(routes) != 1 {
		t.Fatalf("expected 1 alias route, got %d", len(routes))
	}
	if routes[0].ResolvedModel != "gpt-4o" {
		t.Errorf("resolved model: %q", routes[0].ResolvedModel)
	}
	if routes[0].VirtualModel != "fast" {
		t.Errorf("virtual model: %q", routes[0].VirtualModel)
	}
}

func TestResolveRoutesByModel_AliasDoesNotExpandToSameNameChannels(t *testing.T) {
	// An alias "gpt-4o" pointing at channel A must NOT also match channel B
	// that lists "gpt-4o" — alias resolves only to its bound target.
	r := storeWith(
		map[string]*configstore.ConfigEntry{
			"a": openAIProvider("a", "https://a/v1", "gpt-4o"),
			"b": openAIProvider("b", "https://b/v1", "gpt-4o"),
		},
		map[string]*configstore.AliasEntry{
			"gpt-4o": {Alias: "gpt-4o", Provider: "a", Model: "gpt-4o",
				Targets: []configstore.AliasTarget{{Provider: "a", Model: "gpt-4o"}}, Enabled: true},
		},
	)
	routes := r.ResolveRoutesByModel("/v1/chat/completions", "", "gpt-4o", "")
	if len(routes) != 1 || routes[0].ChannelName != "a" {
		t.Errorf("alias should resolve only to its target, got %+v", routes)
	}
}

func TestResolveRoutesByModel_DisabledAliasIgnored(t *testing.T) {
	r := storeWith(
		map[string]*configstore.ConfigEntry{
			"real": openAIProvider("real", "https://real/v1", "gpt-4o"),
		},
		map[string]*configstore.AliasEntry{
			"fast": {Alias: "fast", Provider: "real", Model: "gpt-4o", Enabled: false},
		},
	)
	// Disabled alias is ignored, so it falls through to model matching on "fast"
	// which no channel lists → 0 routes.
	if routes := r.ResolveRoutesByModel("/v1/chat/completions", "", "fast", ""); len(routes) != 0 {
		t.Errorf("disabled alias should be ignored, got %+v", routes)
	}
}

// --- Fallback resolution ---

func TestResolveRoutesForFallbackModels_AliasAndChannelColonModel(t *testing.T) {
	r := storeWith(
		map[string]*configstore.ConfigEntry{
			"primary": openAIProvider("primary", "https://p/v1", "m1"),
			"backup":  openAIProvider("backup", "https://b/v1", "m2"),
		},
		map[string]*configstore.AliasEntry{
			"mini": {Alias: "mini", Provider: "primary", Model: "m1",
				Targets: []configstore.AliasTarget{{Provider: "primary", Model: "m1"}}, Enabled: true},
		},
	)
	routes := r.ResolveRoutesForFallbackModels("/v1/chat/completions", "", []string{"mini", "backup:m2"}, "")
	if len(routes) != 2 {
		t.Fatalf("expected 2 fallback routes, got %d: %+v", len(routes), routes)
	}
}

func TestResolveRoutesForAnyModelFallback(t *testing.T) {
	r := storeWith(map[string]*configstore.ConfigEntry{
		"a": openAIProvider("a", "https://a/v1", "m1", "m2"),
		"b": openAIProvider("b", "https://b/v1", "m3"),
	}, nil)
	routes := r.ResolveRoutesForAnyModelFallback("/v1/chat/completions", "", "")
	// 2 from a + 1 from b = 3.
	if len(routes) != 3 {
		t.Errorf("expected 3 any-model routes, got %d: %+v", len(routes), routes)
	}
}

// --- Type inference ---

func TestInferExpectedType(t *testing.T) {
	cases := map[string]configstore.UpstreamType{
		"/v1/messages":          configstore.Anthropic,
		"/v1/messages?stream=1": configstore.Anthropic,
		"/v1/chat/completions":  configstore.OpenAI,
		"/v1/embeddings":        configstore.OpenAI,
		"/other":                "",
	}
	for path, want := range cases {
		if got := InferExpectedType(path); got != want {
			t.Errorf("InferExpectedType(%q) = %q, want %q", path, got, want)
		}
	}
}

// --- Models list ---

func TestModels_DeduplicatesAndIncludesAliases(t *testing.T) {
	ctx := 8000
	e := openAIProvider("a", "https://a/v1", "gpt-4o")
	e.Models[0].Context = &ctx
	r := storeWith(
		map[string]*configstore.ConfigEntry{
			"a": e,
			"b": openAIProvider("b", "https://b/v1", "gpt-4o"), // dup model, deduped
		},
		map[string]*configstore.AliasEntry{
			"fast": {Alias: "fast", Provider: "a", Model: "gpt-4o",
				Targets: []configstore.AliasTarget{{Provider: "a", Model: "gpt-4o"}}, Enabled: true, Visible: true},
			"hidden": {Alias: "hidden", Provider: "a", Model: "gpt-4o", Enabled: true, Visible: false},
		},
	)
	models := r.Models()
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if !ids["gpt-4o"] || !ids["fast"] {
		t.Errorf("expected gpt-4o and fast, got %v", ids)
	}
	if ids["hidden"] {
		t.Error("invisible alias should not appear in models")
	}
}

// --- Path normalization edge cases ---

func TestBuildRouteResult_OpenAIBareV1(t *testing.T) {
	// /v1 alone (no subpath) for openai → strip /v1 → "/".
	r := storeWith(map[string]*configstore.ConfigEntry{
		"ch": openAIProvider("ch", "https://x/v1", "m"),
	}, nil)
	rt := r.ResolveRouteByModel("/v1", "", "m", "")
	if rt == nil {
		t.Fatal("expected route")
	}
	if rt.TargetURL != "https://x/v1/" {
		t.Errorf("bare /v1 targetURL: %q", rt.TargetURL)
	}
}
