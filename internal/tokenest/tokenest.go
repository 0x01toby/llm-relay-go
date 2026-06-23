// Package tokenest estimates token counts for requests/responses when the
// upstream provider omits usage data. It is a port of src/token-estimator.ts.
//
// The chain mirrors the original: try a model-specific tiktoken encoder → fall
// back to cl100k_base → fall back to a character heuristic (~1 token per 4
// chars). Estimation only ever runs when real usage is all-zero; it never
// overwrites provider-supplied counts.
package tokenest

import (
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

var (
	once     sync.Once
	default_ *tiktoken.Tiktoken
	initErr  error
)

// initEncoder initializes the cl100k_base encoder once. The original loads it
// eagerly at boot; we do the same on first Estimate call.
func initEncoder() {
	once.Do(func() {
		default_, initErr = tiktoken.GetEncoding("cl100k_base")
	})
}

// CountTokens counts tokens in text, preferring a model-specific encoder then
// cl100k_base, finally a character heuristic. Never returns an error — the
// caller always gets a usable estimate.
func CountTokens(text, modelName string) int {
	if text == "" {
		return 0
	}

	// Try a model-specific encoder first.
	if modelName != "" {
		if enc, err := tiktoken.EncodingForModel(modelName); err == nil {
			if n := safeEncode(enc, text); n >= 0 {
				return n
			}
		}
	}

	// Fall back to cl100k_base.
	initEncoder()
	if initErr == nil && default_ != nil {
		if n := safeEncode(default_, text); n >= 0 {
			return n
		}
	}

	// Last resort: character heuristic (~4 chars/token).
	return (len([]rune(text)) + 3) / 4
}

// safeEncode encodes and returns the token count, or -1 on error.
func safeEncode(enc *tiktoken.Tiktoken, text string) int {
	if enc == nil {
		return -1
	}
	// pkoukk/tiktoken-go's Encode returns tokens directly (no error). It panics
	// on certain malformed inputs, so recover defensively.
	var tokens []int
	ok := safeRun(func() {
		tokens = enc.Encode(text, nil, nil)
	})
	if !ok {
		return -1
	}
	return len(tokens)
}

func safeRun(fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	fn()
	return true
}
