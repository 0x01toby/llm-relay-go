package catalog

import (
	"testing"

	"github.com/taozhang/llmrelay/internal/pricing"
)

// TestParseModelsDev_FlattensByModelID verifies the provider-keyed catalog is
// collapsed to one entry per model id, keyed by model id (not provider).
func TestParseModelsDev_FlattensByModelID(t *testing.T) {
	raw := `{
		"anthropic": { "models": {
			"claude-opus-4-5": { "limit": {"context": 200000}, "cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25} }
		}},
		"openai": { "models": {
			"gpt-4o": { "limit": {"context": 128000}, "cost": {"input": 2.5, "output": 10} }
		}}
	}`
	ctx, price, err := parseModelsDev([]byte(raw))
	if err != nil {
		t.Fatalf("parseModelsDev: %v", err)
	}
	if got := ctx["claude-opus-4-5"]; got != 200000 {
		t.Errorf("context claude-opus-4-5 = %d, want 200000", got)
	}
	if got := ctx["gpt-4o"]; got != 128000 {
		t.Errorf("context gpt-4o = %d, want 128000", got)
	}
	if p := price["claude-opus-4-5"]; p == nil || *p.Input != 5 || *p.Output != 25 {
		t.Errorf("pricing claude-opus-4-5 = %+v, want input=5 output=25", p)
	}
}

// TestParseModelsDev_PrefersFirstParty verifies that when the same model id is
// offered by multiple providers, the first-party provider (anthropic/openai)
// wins — its pricing is the most complete (includes cache fields).
func TestParseModelsDev_PrefersFirstParty(t *testing.T) {
	raw := `{
		"venice": { "models": {
			"claude-opus-4-5": { "cost": {"input": 6, "output": 30, "cache_read": 0.6, "cache_write": 7.5} }
		}},
		"anthropic": { "models": {
			"claude-opus-4-5": { "cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25} }
		}},
		"302ai": { "models": {
			"claude-opus-4-5": { "cost": {"input": 5, "output": 25} }
		}}
	}`
	_, price, err := parseModelsDev([]byte(raw))
	if err != nil {
		t.Fatalf("parseModelsDev: %v", err)
	}
	p := price["claude-opus-4-5"]
	if p == nil {
		t.Fatal("missing claude-opus-4-5 pricing")
	}
	// anthropic (first-party) wins over venice (marketer) and 302ai (incomplete).
	if *p.Input != 5 || *p.Output != 25 {
		t.Errorf("got input=%v output=%v, want anthropic's 5/25", *p.Input, *p.Output)
	}
	if *p.CacheRead != 0.5 || *p.CacheWrite != 6.25 {
		t.Errorf("cache got read=%v write=%v, want 0.5/6.25", *p.CacheRead, *p.CacheWrite)
	}
}

// TestParseModelsDev_FlattenedInput verifies a pre-flattened (model-keyed)
// object is also accepted — needed because DB/embed cache round-trips use it.
func TestParseModelsDev_FlattenedInput(t *testing.T) {
	raw := `{
		"gpt-4o-mini": { "limit": {"context": 128000}, "cost": {"input": 0.15, "output": 0.6, "cache_read": 0.075} }
	}`
	ctx, price, err := parseModelsDev([]byte(raw))
	if err != nil {
		t.Fatalf("parseModelsDev: %v", err)
	}
	if got := ctx["gpt-4o-mini"]; got != 128000 {
		t.Errorf("context = %d, want 128000", got)
	}
	p := price["gpt-4o-mini"]
	if p == nil || *p.Input != 0.15 || *p.Output != 0.6 || *p.CacheRead != 0.075 {
		t.Errorf("pricing = %+v", p)
	}
}

// TestParseModelsDev_RealVendored parses the actual vendored catalog and checks
// a few well-known models resolve with the expected context + pricing.
func TestParseModelsDev_RealVendored(t *testing.T) {
	if len(vendoredCatalog) == 0 {
		t.Skip("vendored catalog not embedded")
	}
	_, price, err := parseModelsDev(vendoredCatalog)
	if err != nil {
		t.Fatalf("parseModelsDev vendored: %v", err)
	}
	// Must have a healthy number of priced models.
	if len(price) < 1000 {
		t.Errorf("expected 1000+ priced models, got %d", len(price))
	}
	// anthropic's flagship should resolve with cache pricing.
	p := price["claude-opus-4-5"]
	if p == nil {
		t.Fatal("claude-opus-4-5 not found in vendored catalog")
	}
	if p.Input == nil || *p.Input <= 0 {
		t.Errorf("claude-opus-4-5 input price missing/zero: %+v", p.Input)
	}
	if p.CacheRead == nil {
		t.Error("claude-opus-4-5 should have cache_read price (anthropic first-party)")
	}
}

// TestPricingMap_CanonicalShape verifies PricingMap (consumed by the Models
// page) renders each model's pricing with the canonical keys the dashboard
// expects: input/output/cache_read/cache_write, with absent fields omitted.
func TestPricingMap_CanonicalShape(t *testing.T) {
	in := 5.0
	out := 25.0
	cr := 0.5
	s := &Service{
		pricingCache: map[string]*pricing.ModelPricing{
			"claude-opus-4-5": {Input: &in, Output: &out, CacheRead: &cr},
		},
	}
	m := s.PricingMap()
	got := m["claude-opus-4-5"]
	if got == nil {
		t.Fatal("missing claude-opus-4-5 in pricing map")
	}
	if got["input"] != in {
		t.Errorf("input = %v, want %v", got["input"], in)
	}
	if got["output"] != out {
		t.Errorf("output = %v, want %v", got["output"], out)
	}
	if got["cache_read"] != cr {
		t.Errorf("cache_read = %v, want %v", got["cache_read"], cr)
	}
	if _, present := got["cache_write"]; present {
		t.Error("cache_write should be omitted when nil")
	}
}
