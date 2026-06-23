package repo

import (
	"testing"
)

func TestHashKey_SHA256(t *testing.T) {
	// Known SHA-256 of "hello" to confirm the digest is correct hex.
	got := hashKey("hello")
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("hashKey(hello) = %q, want %q", got, want)
	}
}

func TestCreateRawKey_Format(t *testing.T) {
	key, err := createRawKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) < 10 || key[:3] != "ak-" {
		t.Errorf("raw key format wrong: %q", key)
	}
	// Two keys must differ (random).
	key2, _ := createRawKey()
	if key == key2 {
		t.Error("two generated keys are identical")
	}
}

func TestCreateKeyID_HexLength(t *testing.T) {
	id, err := createKeyID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 { // 16 bytes hex = 32 chars
		t.Errorf("key id length = %d, want 32", len(id))
	}
}

func TestKeyPrefix(t *testing.T) {
	if p := keyPrefix("ak-abcd1234567"); p != "ak-abcd123" {
		t.Errorf("prefix = %q, want %q", p, "ak-abcd123")
	}
}

func TestDedupeTrim(t *testing.T) {
	got := dedupeTrim([]string{"  a ", "b", "a", "", "  "})
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] %q != %q", i, got[i], want[i])
		}
	}
}

func TestParseAllowedModels_Defensive(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`["gpt-4o", "claude", "gpt-4o"]`, []string{"gpt-4o", "claude"}},
		{`["  spaced  ", 123, true]`, []string{"spaced"}}, // drops non-strings
		{`not json`, nil},
		{`[]`, []string{}},
	}
	for _, c := range cases {
		got := ParseAllowedModels(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ParseAllowedModels(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("ParseAllowedModels(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestIsModelAllowed(t *testing.T) {
	patterns := []string{"gpt-4o", "claude-*", "fast"}
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-4o", true},
		{"claude-sonnet", true},     // wildcard match
		{"claude", false},           // wildcard needs prefix
		{"fast", true},
		{"other", false},
	}
	for _, c := range cases {
		if got := IsModelAllowed(c.model, patterns); got != c.want {
			t.Errorf("IsModelAllowed(%q) = %v, want %v", c.model, got, c.want)
		}
	}
	// Empty patterns = allow all.
	if !IsModelAllowed("anything", nil) {
		t.Error("empty patterns should allow all")
	}
}

func TestUSDQuotaMath(t *testing.T) {
	// Quota rounds to nearest micro-USD.
	if q := USDToQuotaMicroUSD(1.5); q != 1_500_000 {
		t.Errorf("USDToQuotaMicroUSD(1.5) = %d", q)
	}
	// Charge rounds up.
	if c := USDCostToChargeMicroUSD(0.0000001); c != 1 {
		t.Errorf("tiny cost should charge up to 1 microUSD, got %d", c)
	}
	if c := USDCostToChargeMicroUSD(1.0); c != 1_000_000 {
		t.Errorf("USDCostToChargeMicroUSD(1.0) = %d", c)
	}
}

func TestBuildQuotaSnapshot(t *testing.T) {
	quota := int64(2_000_000) // $2
	// Not exhausted.
	snap := BuildQuotaSnapshot(&quota, 500_000) // used $0.50
	if snap.QuotaExhausted {
		t.Error("should not be exhausted")
	}
	if snap.CostUsed != 0.5 {
		t.Errorf("used = %v", snap.CostUsed)
	}
	if snap.CostRemaining == nil || *snap.CostRemaining != 1.5 {
		t.Errorf("remaining = %v", snap.CostRemaining)
	}
	// Exhausted at the boundary.
	snap = BuildQuotaSnapshot(&quota, 2_000_000)
	if !snap.QuotaExhausted {
		t.Error("should be exhausted at quota boundary")
	}
	// Unlimited (nil quota).
	snap = BuildQuotaSnapshot(nil, 999_999_999)
	if snap.QuotaExhausted {
		t.Error("unlimited quota should never exhaust")
	}
	if snap.CostQuota != nil {
		t.Error("unlimited quota should have nil CostQuota")
	}
}
