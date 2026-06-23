//go:build integration

// Integration tests for the console API against a real Postgres. Skipped unless
// TEST_DATABASE_URL is set. Run with:
//
//	docker run -d --rm --name lrs-test-pg -e POSTGRES_USER=lrs -e POSTGRES_PASSWORD=lrs \
//	  -e POSTGRES_DB=lrs_test -p 5433:5432 postgres:17-alpine
//	TEST_DATABASE_URL=postgresql://lrs:lrs@localhost:5433/lrs_test \
//	  go test ./internal/consoleapi/ -tags integration -v
//
// If running integration tests for multiple packages that share the same test
// database, use -p 1 so packages execute sequentially and do not race on DB
// resets:
//
//	TEST_DATABASE_URL=postgresql://lrs:lrs@localhost:5433/lrs_test \
//	  go test -p 1 ./internal/consoleapi/ ./internal/repo/ -tags integration
package consoleapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/consoleauth"
	"github.com/taozhang/llmrelay/internal/migrate"
)

const testPassword = "deploy-test-key"

func testDBURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return u
}

func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testDBURL(t)
	ctx := context.Background()

	if err := migrate.ResetDB(ctx, url); err != nil {
		t.Fatalf("ResetDB: %v", err)
	}
	if status := migrate.NewRunner(url, false).Run(ctx); status.State != migrate.StateSuccess {
		t.Fatalf("migrate: %+v", status)
	}

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestAPIWithPool(t *testing.T, pool *pgxpool.Pool) *API {
	t.Helper()
	store := configstore.NewStoreForPool(pool)
	if err := store.EnsureLoaded(context.Background()); err != nil {
		t.Fatalf("load configstore: %v", err)
	}
	return New(pool, store, testPassword, 50000)
}

func authCookie(t *testing.T) *http.Cookie {
	t.Helper()
	return &http.Cookie{
		Name:  consoleauth.CookieName,
		Value: consoleauth.AuthToken(testPassword),
	}
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func decodeJSON(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return out
}

func TestIntegration_Auth_RequiresCookie(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	req := httptest.NewRequest(http.MethodGet, "/__console/api/providers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIntegration_Auth_NoPasswordConfigured_503(t *testing.T) {
	pool := freshDB(t)
	store := configstore.NewStoreForPool(pool)
	_ = store.EnsureLoaded(context.Background())
	a := New(pool, store, "", 50000)
	mux := a.Routes()

	req := httptest.NewRequest(http.MethodGet, "/__console/api/providers", nil)
	req.AddCookie(authCookie(t))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIntegration_ProviderCRUD(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	// 1. Create provider.
	createBody := map[string]any{
		"channelName":       "test-openai",
		"type":              "openai",
		"targetBaseUrl":     "https://api.openai.com/v1",
		"systemPrompt":      "be helpful",
		"models":            []any{"gpt-4o", map[string]any{"model": "gpt-4o-mini", "context": 128000}},
		"priority":          5,
		"routingVisibility": "direct",
		"auth":              map[string]any{"header": "authorization", "value": "sk-test"},
	}
	req := httptest.NewRequest(http.MethodPost, "/__console/api/providers", jsonBody(createBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create provider: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	created := decodeJSON(t, w.Body)
	if created["channelName"] != "test-openai" {
		t.Fatalf("unexpected created provider: %+v", created)
	}

	// 2. List providers.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/providers", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list providers: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	list := decodeJSON(t, w.Body)
	providers := list["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(providers))
	}

	// 3. Get provider details.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/providers/test-openai", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("get provider: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	detail := decodeJSON(t, w.Body)
	if detail["targetBaseUrl"] != "https://api.openai.com/v1" {
		t.Fatalf("unexpected detail: %+v", detail)
	}
	models := detail["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}

	// 4. Update provider (rename + change URL).
	updateBody := map[string]any{
		"channelName":       "test-openai-renamed",
		"type":              "openai",
		"targetBaseUrl":     "https://api.openai.com/v2",
		"models":            []any{"gpt-4o"},
		"routingVisibility": "explicit_only",
	}
	req = httptest.NewRequest(http.MethodPatch, "/__console/api/providers/test-openai", jsonBody(updateBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("update provider: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	updated := decodeJSON(t, w.Body)
	if updated["channelName"] != "test-openai-renamed" || updated["routingVisibility"] != "explicit_only" {
		t.Fatalf("unexpected updated provider: %+v", updated)
	}
	// 5. Toggle enabled.
	req = httptest.NewRequest(http.MethodPatch, "/__console/api/providers/test-openai-renamed/enabled", jsonBody(map[string]any{"enabled": false}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("toggle provider: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 6. Delete provider.
	req = httptest.NewRequest(http.MethodDelete, "/__console/api/providers/test-openai-renamed", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete provider: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 7. List should be empty again.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/providers", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	list = decodeJSON(t, w.Body)
	providers = list["providers"].([]any)
	if len(providers) != 0 {
		t.Fatalf("expected 0 providers after delete, got %d", len(providers))
	}
}

// TestIntegration_ProviderAuth_ValueOnly_AutoHeader covers the dashboard's
// "auto" auth-header selection: the payload carries { value } with no header,
// expecting the backend to infer the default header from the provider type.
// This was the root cause of "configured key not taking effect".
func TestIntegration_ProviderAuth_ValueOnly_AutoHeader(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	// Create an Anthropic provider with only { value: apiKey } (no header) —
	// exactly what the frontend sends when authHeader === "auto".
	createBody := map[string]any{
		"channelName":   "claude-ch",
		"type":          "anthropic",
		"targetBaseUrl": "https://api.anthropic.com",
		"models":        []any{"claude-3-5-haiku-latest"},
		"auth":          map[string]any{"value": "sk-anthropic-key"},
	}
	req := httptest.NewRequest(http.MethodPost, "/__console/api/providers", jsonBody(createBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// GET detail: the auth must be persisted with the inferred default header.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/providers/claude-ch", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	got := decodeJSON(t, w.Body)
	auth, ok := got["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth missing: %+v", got)
	}
	if auth["value"] != "sk-anthropic-key" {
		t.Errorf("auth value not persisted: %+v", auth)
	}
	if auth["header"] != "x-api-key" {
		t.Errorf("expected default header x-api-key for anthropic, got %v", auth["header"])
	}

	// Same check for OpenAI: auto header should be "authorization".
	openaiBody := map[string]any{
		"channelName":   "openai-ch",
		"type":          "openai",
		"targetBaseUrl": "https://api.openai.com/v1",
		"models":        []any{"gpt-4o"},
		"auth":          map[string]any{"value": "sk-openai-key"},
	}
	req = httptest.NewRequest(http.MethodPost, "/__console/api/providers", jsonBody(openaiBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create openai: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/__console/api/providers/openai-ch", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	got = decodeJSON(t, w.Body)
	auth = got["auth"].(map[string]any)
	if auth["header"] != "authorization" {
		t.Errorf("expected default header authorization for openai, got %v", auth["header"])
	}
}

func TestIntegration_AliasCRUD(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	// 1. Create alias.
	createBody := map[string]any{
		"alias":       "fast-gpt",
		"provider":    "ch1",
		"model":       "gpt-4o",
		"targets":     []map[string]any{{"provider": "ch1", "model": "gpt-4o"}},
		"description": "fast alias",
		"visible":     true,
		"enabled":     true,
	}
	req := httptest.NewRequest(http.MethodPost, "/__console/api/model-aliases", jsonBody(createBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create alias: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	created := decodeJSON(t, w.Body)
	id, ok := created["id"].(float64)
	if !ok {
		t.Fatalf("expected numeric id, got %T", created["id"])
	}

	// 2. List aliases.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/model-aliases", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	list := decodeJSON(t, w.Body)
	aliases := list["aliases"].([]any)
	if len(aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d", len(aliases))
	}

	// 3. Update alias.
	updateBody := map[string]any{
		"alias":       "fast-gpt-renamed",
		"provider":    "ch1",
		"model":       "gpt-4o",
		"targets":     []map[string]any{{"provider": "ch1", "model": "gpt-4o"}},
		"description": "renamed alias",
		"visible":     false,
		"enabled":     true,
	}
	req = httptest.NewRequest(http.MethodPatch, "/__console/api/model-aliases/"+jsonNumberString(int64(id)), jsonBody(updateBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("update alias: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	updated := decodeJSON(t, w.Body)
	if updated["alias"] != "fast-gpt-renamed" || updated["visible"] != false {
		t.Fatalf("unexpected updated alias: %+v", updated)
	}

	// 4. Toggle enabled.
	req = httptest.NewRequest(http.MethodPatch, "/__console/api/model-aliases/"+jsonNumberString(int64(id))+"/enabled", jsonBody(map[string]any{"enabled": false}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("toggle alias: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 5. Delete alias.
	req = httptest.NewRequest(http.MethodDelete, "/__console/api/model-aliases/"+jsonNumberString(int64(id)), nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete alias: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIntegration_KeyCRUD(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	// 1. Create key.
	req := httptest.NewRequest(http.MethodPost, "/__console/api/keys", jsonBody(map[string]any{"name": "test-key", "cost_quota": nil}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create key: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	created := decodeJSON(t, w.Body)
	// Response shape must be { key, record } so the dashboard can splice the
	// record straight into the key list (this was the cause of the blank page
	// after creating a key).
	rawKey, ok := created["key"].(string)
	if !ok || rawKey == "" {
		t.Fatalf("expected non-empty key, got %v", created["key"])
	}
	record, ok := created["record"].(map[string]any)
	if !ok {
		t.Fatalf("expected record object, got %T: %+v", created["record"], created)
	}
	id, ok := record["id"].(string)
	if !ok || id == "" {
		t.Fatalf("expected record.id, got %v", record["id"])
	}
	// record must carry the full ManagedApiKey shape (same fields as the list).
	for _, field := range []string{"name", "prefix", "created_at", "allowed_models", "cost_quota", "cost_used", "quota_exhausted"} {
		if _, present := record[field]; !present {
			t.Errorf("record missing field %q: %+v", field, record)
		}
	}

	// 2. List keys.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/keys", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	list := decodeJSON(t, w.Body)
	keys := list["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}

	// 3. Rename key.
	req = httptest.NewRequest(http.MethodPatch, "/__console/api/keys/"+id, jsonBody(map[string]any{"name": "renamed-key"}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("rename key: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 4. Set allowed models.
	req = httptest.NewRequest(http.MethodPatch, "/__console/api/keys/"+id+"/allowed-models", jsonBody(map[string]any{"models": []string{"gpt-4o"}}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("set allowed models: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 5. Delete key.
	req = httptest.NewRequest(http.MethodDelete, "/__console/api/keys/"+id, nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete key: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIntegration_SettingsCRUD(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	// 1. Update timeouts.
	req := httptest.NewRequest(http.MethodPatch, "/__console/api/settings/timeouts", jsonBody(map[string]any{"defaultFirstByteTimeoutMs": 60000}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("update timeouts: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 2. Read timeouts.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/settings/timeouts", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("get timeouts: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeJSON(t, w.Body)
	if got["defaultFirstByteTimeoutMs"] != float64(60000) {
		t.Fatalf("unexpected timeouts: %+v", got)
	}

	// 3. Update failover.
	req = httptest.NewRequest(http.MethodPatch, "/__console/api/settings/failover", jsonBody(map[string]any{"enabled": false}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("update failover: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 4. Read failover.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/settings/failover", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("get failover: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got = decodeJSON(t, w.Body)
	if got["enabled"] != false {
		t.Fatalf("unexpected failover: %+v", got)
	}
}

func jsonNumberString(n int64) string {
	return strconv.FormatInt(n, 10)
}

func TestIntegration_RequestDetail(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	// Insert a fully-populated row directly via SQL (the gateway is the writer;
	// the detail endpoint only reads).
	ctx := context.Background()
	status := 200
	statusText := "200 OK"
	apiKeyName := "My MacBook"
	origPayload := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`
	respPayload := `{"type":"message","content":[{"type":"text","text":"Hi"}]}`
	_, err := pool.Exec(ctx, `
		INSERT INTO console_requests
		  (request_id, created_at, route_prefix, upstream_type, method, path, target_url,
		   request_model, api_key_id, api_key_name,
		   original_payload, original_payload_truncated,
		   forwarded_payload, forwarded_payload_truncated,
		   response_status, response_status_text, response_payload,
		   response_payload_truncated, response_body_bytes,
		   first_chunk_at, first_token_at, completed_at, has_streaming_content,
		   response_model, stop_reason,
		   input_tokens, output_tokens, total_tokens,
		   cache_creation_input_tokens, cache_read_input_tokens,
		   retry_attempt, source_request_type)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)
	`,
		"det-1", int64(1782220415818), "sssapi", "anthropic", "POST", "/v1/messages",
		"https://node-hk.sssaiapi.com/api/v1/messages", "claude-sonnet-4-6", nil, &apiKeyName,
		origPayload, 0, origPayload, 0, &status, &statusText, respPayload, 0, 1759,
		int64(1782220415819), int64(1782220415819), int64(1782220416052), 1,
		"claude-sonnet-4.6", "end_turn",
		int64(10), int64(21), int64(31),
		int64(0), int64(5),
		0, "generic",
	)
	if err != nil {
		t.Fatalf("insert request row: %v", err)
	}

	// GET the detail.
	req := httptest.NewRequest(http.MethodGet, "/__console/api/requests/det-1", nil)
	req.AddCookie(authCookie(t))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeJSON(t, w.Body)

	// record must be present and non-null (the stub bug returned only
	// {request_id}, which crashed DetailView at detail.record.response_usage).
	record, ok := got["record"].(map[string]any)
	if !ok {
		t.Fatalf("expected record object, got %T", got["record"])
	}
	// analysis must be present (DetailView reads analysis.summary unguarded).
	analysis, ok := got["analysis"].(map[string]any)
	if !ok {
		t.Fatalf("expected analysis object, got %T", got["analysis"])
	}
	if analysis["summary"] == nil || analysis["summary"] == "" {
		t.Errorf("analysis.summary should be non-empty: %+v", analysis)
	}
	if analysis["cache_state"] != "hit" {
		t.Errorf("expected cache_state hit (cache_read=5), got %v", analysis["cache_state"])
	}

	// Nested response_usage and response_timing must be objects.
	if _, ok := record["response_usage"].(map[string]any); !ok {
		t.Errorf("record.response_usage should be object: %+v", record["response_usage"])
	}
	if _, ok := record["response_timing"].(map[string]any); !ok {
		t.Errorf("record.response_timing should be object: %+v", record["response_timing"])
	}

	// failover_chain must be an array (never nil) — DetailView/maps iterate it.
	chain, ok := record["failover_chain"].([]any)
	if !ok {
		t.Errorf("record.failover_chain should be array, got %T", record["failover_chain"])
	}
	if chain == nil {
		t.Error("record.failover_chain should not be nil")
	}

	// header objects must be objects (DetailView does JSON.stringify on them).
	for _, h := range []string{"original_headers", "forward_headers", "response_headers"} {
		if _, ok := record[h].(map[string]any); !ok {
			t.Errorf("record.%s should be object, got %T", h, record[h])
		}
	}
}

// mockUpstream is a tiny HTTP server that stands in for an OpenAI-compatible
// upstream during the test/preview integration tests. It serves /models and
// /chat/completions.
type mockUpstream struct {
	*httptest.Server
	modelsPath   int // count of /models hits
	chatPath     int // count of /chat/completions hits
	modelsReply  func(w http.ResponseWriter)
	chatReply    func(w http.ResponseWriter)
	expectedAuth string
}

func newMockUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	m := &mockUpstream{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.expectedAuth != "" {
			if r.Header.Get("Authorization") != "Bearer "+m.expectedAuth {
				http.Error(w, `{"error":"bad auth"}`, http.StatusUnauthorized)
				return
			}
		}
		switch r.URL.Path {
		case "/v1/models", "/models":
			m.modelsPath++
			if m.modelsReply != nil {
				m.modelsReply(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`))
		case "/v1/chat/completions", "/chat/completions":
			m.chatPath++
			if m.chatReply != nil {
				m.chatReply(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"pong"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.Server.Close)
	return m
}

func TestIntegration_UpstreamModelsPreview(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	mock := newMockUpstream(t)
	mock.expectedAuth = "sk-test-key"

	// POST /upstream-models-preview with typed-in params.
	body := map[string]any{
		"targetBaseUrl": mock.URL + "/v1",
		"type":          "openai",
		"authHeader":    "authorization",
		"authValue":     "sk-test-key",
	}
	req := httptest.NewRequest(http.MethodPost, "/__console/api/upstream-models-preview", jsonBody(body))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeJSON(t, w.Body)
	models := got["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(models), models)
	}
	if mock.modelsPath != 1 {
		t.Errorf("expected 1 /models hit, got %d", mock.modelsPath)
	}
}

func TestIntegration_ProviderUpstreamModels_SavedChannel(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	mock := newMockUpstream(t)
	mock.expectedAuth = "sk-saved-key"

	// Create a provider pointing at the mock.
	createBody := map[string]any{
		"channelName":   "mock",
		"type":          "openai",
		"targetBaseUrl": mock.URL + "/v1",
		"models":        []any{"gpt-4o"},
		"auth":          map[string]any{"header": "authorization", "value": "sk-saved-key"},
	}
	req := httptest.NewRequest(http.MethodPost, "/__console/api/providers", jsonBody(createBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create provider: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// GET upstream-models for the saved channel.
	req = httptest.NewRequest(http.MethodGet, "/__console/api/providers/mock/upstream-models", nil)
	req.AddCookie(authCookie(t))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("upstream-models: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeJSON(t, w.Body)
	models := got["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if mock.modelsPath != 1 {
		t.Errorf("expected 1 /models hit, got %d", mock.modelsPath)
	}
}

func TestIntegration_ProviderTest(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	mock := newMockUpstream(t)
	mock.expectedAuth = "sk-test-key"

	// Create a provider pointing at the mock.
	createBody := map[string]any{
		"channelName":   "mock",
		"type":          "openai",
		"targetBaseUrl": mock.URL + "/v1",
		"models":        []any{"gpt-4o-mini"},
		"auth":          map[string]any{"header": "authorization", "value": "sk-test-key"},
	}
	req := httptest.NewRequest(http.MethodPost, "/__console/api/providers", jsonBody(createBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create provider: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// POST test.
	req = httptest.NewRequest(http.MethodPost, "/__console/api/providers/mock/test", jsonBody(map[string]any{"model": "gpt-4o-mini"}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeJSON(t, w.Body)
	if got["status"] != "ok" {
		t.Fatalf("expected status ok, got %v: %+v", got["status"], got)
	}
	if got["statusCode"] != float64(200) {
		t.Errorf("expected statusCode 200, got %v", got["statusCode"])
	}
	if got["model"] != "gpt-4o-mini" {
		t.Errorf("expected model gpt-4o-mini, got %v", got["model"])
	}
	if mock.chatPath != 1 {
		t.Errorf("expected 1 /chat/completions hit, got %d", mock.chatPath)
	}
}

func TestIntegration_ProviderTest_AuthFailure(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	// Mock that rejects all auth.
	mock := newMockUpstream(t)
	mock.expectedAuth = "correct-key"

	createBody := map[string]any{
		"channelName":   "mock",
		"type":          "openai",
		"targetBaseUrl": mock.URL + "/v1",
		"models":        []any{"gpt-4o-mini"},
		"auth":          map[string]any{"header": "authorization", "value": "wrong-key"},
	}
	req := httptest.NewRequest(http.MethodPost, "/__console/api/providers", jsonBody(createBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create provider: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/__console/api/providers/mock/test", jsonBody(map[string]any{"model": "gpt-4o-mini"}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeJSON(t, w.Body)
	if got["status"] != "error" {
		t.Fatalf("expected status error, got %v", got["status"])
	}
	if got["statusCode"] != float64(401) {
		t.Errorf("expected statusCode 401, got %v", got["statusCode"])
	}
}

func TestIntegration_ProviderTest_NoAuthConfigured(t *testing.T) {
	pool := freshDB(t)
	a := newTestAPIWithPool(t, pool)
	mux := a.Routes()

	// Create a provider WITHOUT auth.
	createBody := map[string]any{
		"channelName":   "noauth",
		"type":          "openai",
		"targetBaseUrl": "https://example.com/v1",
		"models":        []any{"gpt-4o"},
	}
	req := httptest.NewRequest(http.MethodPost, "/__console/api/providers", jsonBody(createBody))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create provider: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Test should fail with a 400 (渠道未配置认证信息).
	req = httptest.NewRequest(http.MethodPost, "/__console/api/providers/noauth/test", jsonBody(map[string]any{"model": "gpt-4o"}))
	req.AddCookie(authCookie(t))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("test noauth: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
