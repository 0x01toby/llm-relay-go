package pricing

import (
	"math"
	"testing"

	"github.com/taozhang/llmrelay/internal/providers"
)

func fptr(f float64) *float64 { return &f }

func TestBucketsForUsage_OpenAI(t *testing.T) {
	u := providers.UsageData{InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 500}
	b := BucketsForUsage(u)
	// uncached = 1000 - 200 = 800; cache_read = 200.
	if b.Input != 800 {
		t.Errorf("input: %d", b.Input)
	}
	if b.CacheRead != 200 {
		t.Errorf("cache_read: %d", b.CacheRead)
	}
	if b.Output != 500 {
		t.Errorf("output: %d", b.Output)
	}
}

func TestBucketsForUsage_Anthropic(t *testing.T) {
	u := providers.UsageData{InputTokens: 100, CacheReadInputTokens: 50, CacheCreationInputTokens: 30, OutputTokens: 200}
	b := BucketsForUsage(u)
	// Anthropic: input stays 100, cache_read=50, cache_write=30.
	if b.Input != 100 || b.CacheRead != 50 || b.CacheWrite != 30 {
		t.Errorf("buckets: %+v", b)
	}
}

func TestCalculateCost(t *testing.T) {
	p := &ModelPricing{Input: fptr(10), Output: fptr(30), CacheRead: fptr(1), CacheWrite: fptr(1.25)}
	// 1M input @ $10 = $10; but we use 500K input, 500K output.
	u := providers.UsageData{InputTokens: 500_000, OutputTokens: 500_000}
	cost := CalculateCost(u, p)
	// 0.5M * 10 + 0.5M * 30 = 5 + 15 = 20.
	if math.Abs(cost-20.0) > 0.001 {
		t.Errorf("cost: %v, want 20", cost)
	}
}

func TestCalculateCost_NilPricing(t *testing.T) {
	u := providers.UsageData{InputTokens: 100, OutputTokens: 100}
	if cost := CalculateCost(u, nil); cost != 0 {
		t.Errorf("nil pricing should be 0, got %v", cost)
	}
}

func TestCalculateCost_WithCache(t *testing.T) {
	p := &ModelPricing{Input: fptr(3), Output: fptr(15), CacheRead: fptr(0.3), CacheWrite: fptr(3.75)}
	u := providers.UsageData{InputTokens: 1000, CacheReadInputTokens: 5000, CacheCreationInputTokens: 1000, OutputTokens: 500}
	cost := CalculateCost(u, p)
	// input: 1k/1M * 3 = 0.003; cache_read: 5k/1M * 0.3 = 0.0015;
	// cache_write: 1k/1M * 3.75 = 0.00375; output: 500/1M * 15 = 0.0075.
	want := 0.003 + 0.0015 + 0.00375 + 0.0075
	if math.Abs(cost-want) > 0.0001 {
		t.Errorf("cost: %v, want %v", cost, want)
	}
}
