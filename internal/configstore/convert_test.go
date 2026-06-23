package configstore

import (
	"testing"

	"github.com/taozhang/llmrelay/internal/schema"
)

func ptr[T any](v T) *T { return &v }

func TestProviderRowToEntry_Basic(t *testing.T) {
	row := schema.ConsoleProvider{
		ChannelName: "ch", ProviderUUID: "uuid-1", Type: "openai",
		TargetBaseURL: "https://api.openai.com/v1", Priority: 5, Enabled: 1,
		RoutingVisibility: "direct", ModelsJSON: `[{"model":"gpt-4o","context":128000}]`,
		ExtraFieldsJSON: `{"responsesMode":"chat_compat"}`,
	}
	e := providerRowToEntry(row)
	if e == nil {
		t.Fatal("expected entry")
	}
	if e.Type != OpenAI || e.TargetBaseURL != "https://api.openai.com/v1" {
		t.Errorf("type/url: %s %s", e.Type, e.TargetBaseURL)
	}
	if !e.Enabled || e.Priority != 5 {
		t.Errorf("enabled/priority: %v %d", e.Enabled, e.Priority)
	}
	if len(e.Models) != 1 || e.Models[0].Model != "gpt-4o" {
		t.Errorf("models: %+v", e.Models)
	}
	if e.Models[0].Context == nil || *e.Models[0].Context != 128000 {
		t.Errorf("context: %v", e.Models[0].Context)
	}
	if e.ResponsesMode != ResponsesChatCompat {
		t.Errorf("responsesMode: %s", e.ResponsesMode)
	}
	if e.ProviderUUID != "uuid-1" {
		t.Errorf("uuid: %s", e.ProviderUUID)
	}
}

func TestProviderRowToEntry_AnthropicHasNoResponsesMode(t *testing.T) {
	row := schema.ConsoleProvider{
		ChannelName: "ch", Type: "anthropic", TargetBaseURL: "https://api.anthropic.com",
		Enabled: 1, RoutingVisibility: "direct", ModelsJSON: `[{"model":"claude-3"}]`,
	}
	e := providerRowToEntry(row)
	if e.ResponsesMode != DefaultResponsesMode {
		t.Errorf("anthropic should have default responsesMode, got %s", e.ResponsesMode)
	}
}

func TestProviderRowToEntry_AuthNormalization(t *testing.T) {
	// Bearer prefix is stored as-is in the DB; header defaults by type.
	row := schema.ConsoleProvider{
		ChannelName: "ch", Type: "openai", TargetBaseURL: "https://x/v1",
		Enabled: 1, AuthHeader: ptr("authorization"), AuthValue: ptr("Bearer sk-xyz"),
	}
	e := providerRowToEntry(row)
	if e.Auth == nil || e.Auth.Header != "authorization" || e.Auth.Value != "Bearer sk-xyz" {
		t.Errorf("auth: %+v", e.Auth)
	}

	// Anthropic with null header → default x-api-key.
	row2 := row
	row2.Type = "anthropic"
	row2.AuthHeader = nil
	e2 := providerRowToEntry(row2)
	if e2.Auth.Header != "x-api-key" {
		t.Errorf("anthropic default header: %s", e2.Auth.Header)
	}
}

func TestProviderRowToEntry_EmptyAuthValueRejected(t *testing.T) {
	row := schema.ConsoleProvider{
		ChannelName: "ch", Type: "openai", TargetBaseURL: "https://x/v1",
		Enabled: 1, AuthHeader: ptr("authorization"), AuthValue: ptr(""),
	}
	if e := providerRowToEntry(row); e != nil {
		t.Error("empty auth value should produce nil entry (invalid)")
	}
}

func TestProviderRowToEntry_Disabled(t *testing.T) {
	row := schema.ConsoleProvider{
		ChannelName: "ch", Type: "openai", TargetBaseURL: "https://x/v1",
		Enabled: 0, RoutingVisibility: "direct", ModelsJSON: `[{"model":"m"}]`,
	}
	e := providerRowToEntry(row)
	if e.Enabled {
		t.Error("should be disabled")
	}
}

func TestParseModelsJSON_Defensive(t *testing.T) {
	if parseModelsJSON("") != nil {
		t.Error("empty → nil")
	}
	if parseModelsJSON("not json") != nil {
		t.Error("invalid → nil")
	}
	// Missing model field dropped.
	ms := parseModelsJSON(`[{"model":"a"},{"context":5},{"model":"b"}]`)
	if len(ms) != 2 || ms[1].Model != "b" {
		t.Errorf("got %+v", ms)
	}
}

func TestExtractResponsesMode(t *testing.T) {
	cases := []struct {
		extra map[string]interface{}
		want  ResponsesMode
	}{
		{map[string]interface{}{"responsesMode": "chat_compat"}, ResponsesChatCompat},
		{map[string]interface{}{"responsesMode": "disabled"}, ResponsesDisabled},
		{map[string]interface{}{"responsesMode": "native"}, ResponsesNative},
		{map[string]interface{}{"responsesMode": "bogus"}, DefaultResponsesMode},
		{map[string]interface{}{}, DefaultResponsesMode},
		{nil, DefaultResponsesMode},
	}
	for _, c := range cases {
		if got := extractResponsesMode(c.extra); got != c.want {
			t.Errorf("extractResponsesMode(%v) = %s, want %s", c.extra, got, c.want)
		}
	}
}

func TestBuildSnapshot_UUIDIndexAndDisabledAlias(t *testing.T) {
	providers := []schema.ConsoleProvider{
		{ChannelName: "a", ProviderUUID: "uuid-a", Type: "openai", TargetBaseURL: "https://a/v1",
			Enabled: 1, RoutingVisibility: "direct", ModelsJSON: `[{"model":"m"}]`},
	}
	aliases := []schema.ModelAlias{
		{Alias: "on", Provider: "a", Model: "m", Enabled: 1, Visible: 1, TargetsJSON: `[{"provider":"a","model":"m"}]`},
		{Alias: "off", Provider: "a", Model: "m", Enabled: 0, Visible: 1},
	}
	snap := buildSnapshot(providers, aliases)
	if snap.Provider("a") == nil {
		t.Error("provider a missing")
	}
	if snap.ChannelForUUID("uuid-a") != "a" {
		t.Errorf("uuid index: %q", snap.ChannelForUUID("uuid-a"))
	}
	if snap.Alias("on") == nil {
		t.Error("enabled alias missing")
	}
	if snap.Alias("off") != nil {
		t.Error("disabled alias should be excluded")
	}
}
