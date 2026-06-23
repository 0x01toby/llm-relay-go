// Package pricing computes request costs from token usage and per-model
// pricing data. It is a port of src/pricing.ts (the cost-calculation half).
//
// Pricing data (per 1M tokens, USD) is sourced from models.dev and cached in
// the model_catalog_cache table; the catalog package owns fetching/caching.
// This package only does the arithmetic.
package pricing

import "github.com/taozhang/llmrelay/internal/providers"

// ModelPricing is the per-1M-token USD price for a model. Cache and Read are
// optional (nil = that component is free/untracked).
type ModelPricing struct {
	Input      *float64
	Output     *float64
	CacheRead  *float64
	CacheWrite *float64
}

// TokensPerMillion is the divisor (prices are per 1M tokens).
const TokensPerMillion = 1_000_000.0

// TokenBuckets splits usage into the cost-relevant token buckets, inferring
// the provider type from which cache fields are populated. Mirrors
// getCostTokenBuckets.
type TokenBuckets struct {
	Input       int64
	Output      int64
	CacheRead   int64
	CacheWrite  int64
}

// BucketsForUsage derives the token buckets from a usage record. For OpenAI
// (cached_input_tokens populated), uncached = input - cached. For Anthropic,
// cache_read and cache_write (cache_creation) are separate buckets.
func BucketsForUsage(u providers.UsageData) TokenBuckets {
	b := TokenBuckets{Input: u.InputTokens, Output: u.OutputTokens}
	if u.CachedInputTokens > 0 {
		// OpenAI: subtract cached from input.
		b.CacheRead = u.CachedInputTokens
		b.Input = u.InputTokens - u.CachedInputTokens
		if b.Input < 0 {
			b.Input = 0
		}
	}
	if u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0 {
		// Anthropic.
		b.CacheRead = u.CacheReadInputTokens
		b.CacheWrite = u.CacheCreationInputTokens
	}
	return b
}

// CalculateCost computes the USD cost for a usage record given its pricing.
// Returns 0 if pricing is nil.
func CalculateCost(u providers.UsageData, p *ModelPricing) float64 {
	if p == nil {
		return 0
	}
	b := BucketsForUsage(u)
	cost := 0.0
	if p.Input != nil {
		cost += float64(b.Input) / TokensPerMillion * *p.Input
	}
	if p.Output != nil {
		cost += float64(b.Output) / TokensPerMillion * *p.Output
	}
	if p.CacheRead != nil && b.CacheRead > 0 {
		cost += float64(b.CacheRead) / TokensPerMillion * *p.CacheRead
	}
	if p.CacheWrite != nil && b.CacheWrite > 0 {
		cost += float64(b.CacheWrite) / TokensPerMillion * *p.CacheWrite
	}
	return cost
}
