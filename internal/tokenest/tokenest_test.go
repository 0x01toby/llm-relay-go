package tokenest

import "testing"

func TestCountTokens_NonEmpty(t *testing.T) {
	// "Hello, world!" should produce a small positive token count with any
	// encoder (exact value depends on the encoding; just sanity-check).
	n := CountTokens("Hello, world!", "gpt-4o")
	if n <= 0 {
		t.Errorf("expected positive token count, got %d", n)
	}
}

func TestCountTokens_Empty(t *testing.T) {
	if n := CountTokens("", "gpt-4o"); n != 0 {
		t.Errorf("empty string: got %d, want 0", n)
	}
}

func TestCountTokens_UnknownModelFallsBack(t *testing.T) {
	// An unknown model should still produce a count via cl100k_base or the
	// heuristic — never panic, never return nonsense.
	n := CountTokens("This is a test sentence with several words in it.", "nonexistent-model-xyz")
	if n <= 0 {
		t.Errorf("fallback should produce positive count, got %d", n)
	}
}

func TestCountTokens_HeuristicConsistency(t *testing.T) {
	// The heuristic is ~4 chars/token; verify it's in the right ballpark for a
	// longer string (the heuristic only kicks in if tiktoken is unavailable,
	// but the count should be reasonable either way).
	text := "The quick brown fox jumps over the lazy dog repeatedly."
	n := CountTokens(text, "")
	if n < 5 || n > 50 {
		t.Errorf("token count %d out of expected range for %q", n, text)
	}
}
