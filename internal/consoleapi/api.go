// Package consoleapi implements the /__console/* API surface used by the React
// dashboard. It is the Go port of src/console-ui.ts.
//
// All routes (except GET /api/session) require cookie authentication: the
// request must carry a valid console session cookie (see consoleauth). The
// frontend calls these with credentials: "same-origin", so the cookie contract
// must match exactly.
//
// Response shapes mirror the original. Some endpoints wrap payloads in
// {"ok":true,...} while others spread at the top level; the frontend tolerates
// both, so we normalize to {"ok":true,...} for consistency.
package consoleapi

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/consoleauth"
	"github.com/taozhang/llmrelay/internal/consolestore"
	"github.com/taozhang/llmrelay/internal/cors"
	"github.com/taozhang/llmrelay/internal/repo"
	"github.com/taozhang/llmrelay/internal/routing"
	"github.com/taozhang/llmrelay/internal/schema"
)

// API holds all the dependencies the console routes need.
type API struct {
	password    string
	pool        *pgxpool.Pool
	store       *configstore.Store
	provider    *repo.ProviderRepo
	alias       *repo.AliasRepo
	apikey      *repo.APIKeyRepo
	settings    *repo.SettingsRepo
	requests    *consolestore.Repository
}

// New builds a console API handler.
func New(pool *pgxpool.Pool, store *configstore.Store, password string, maxRecords int) *API {
	return &API{
		password: password,
		pool:     pool,
		store:    store,
		provider: repo.NewProviderRepo(pool),
		alias:    repo.NewAliasRepo(pool),
		apikey:   repo.NewAPIKeyRepo(pool),
		settings: repo.NewSettingsRepo(pool),
		requests: consolestore.New(pool, maxRecords),
	}
}

// Routes returns the HTTP handler for /__console/*. Mount it at that prefix.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/__console", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("/__console/login", a.handleLogin)
	mux.HandleFunc("/__console/logout", a.handleLogout)
	mux.HandleFunc("/__console/api/session", a.handleSession)

	// Authenticated routes.
	mux.HandleFunc("/__console/api/requests", a.requireAuth(a.handleRequests))
	mux.HandleFunc("/__console/api/requests/", a.requireAuth(a.handleRequestDetail)) // /:id
	mux.HandleFunc("/__console/api/models", a.requireAuth(a.handleModels))
	mux.HandleFunc("/__console/api/models/", a.requireAuth(a.handleModelMetadata)) // /:channel/:model/metadata
	mux.HandleFunc("/__console/api/stats", a.requireAuth(a.handleStats))
	mux.HandleFunc("/__console/api/filters", a.requireAuth(a.handleFilters))
	mux.HandleFunc("/__console/api/providers", a.requireAuth(a.handleProviders))
	mux.HandleFunc("/__console/api/providers/", a.requireAuth(a.handleProviderDetail)) // /:channel or /:channel/enabled
	mux.HandleFunc("/__console/api/keys", a.requireAuth(a.handleKeys))
	mux.HandleFunc("/__console/api/keys/", a.requireAuth(a.handleKeyDetail)) // /:id or /:id/{allowed-models,quota}
	mux.HandleFunc("/__console/api/model-aliases", a.requireAuth(a.handleAliases))
	mux.HandleFunc("/__console/api/model-aliases/", a.requireAuth(a.handleAliasDetail)) // /:id
	mux.HandleFunc("/__console/api/upstream-models-preview", a.requireAuth(a.handleUpstreamModelsPreview))
	mux.HandleFunc("/__console/api/settings/timeouts", a.requireAuth(a.handleTimeouts))
	mux.HandleFunc("/__console/api/settings/failover", a.requireAuth(a.handleFailover))

	return mux
}

// --- Auth helpers ---

func (a *API) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cors.Apply(w.Header(), r)
		if !consoleauth.IsPasswordConfigured(a.password) {
			writeJSON(w, http.StatusServiceUnavailable, obj{"error": "GATEWAY_API_KEY 未设置"})
			return
		}
		if !consoleauth.IsAuthenticated(r, a.password) {
			writeJSON(w, http.StatusUnauthorized, obj{"error": "未授权"})
			return
		}
		h(w, r)
	}
}

// --- Session / login / logout ---

func (a *API) handleSession(w http.ResponseWriter, r *http.Request) {
	cors.Apply(w.Header(), r)
	writeJSON(w, http.StatusOK, obj{
		"authenticated": consoleauth.IsAuthenticated(r, a.password),
		"enabled":       consoleauth.IsPasswordConfigured(a.password),
	})
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	cors.Apply(w.Header(), r)
	if !consoleauth.IsPasswordConfigured(a.password) {
		writeJSON(w, http.StatusServiceUnavailable, obj{"error": "GATEWAY_API_KEY 未设置"})
		return
	}
	var body struct {
		Password   string `json:"password"`
		GatewayKey string `json:"gatewayKey"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	cred := body.Password
	if cred == "" {
		cred = body.GatewayKey
	}
	if cred != a.password {
		writeJSON(w, http.StatusUnauthorized, obj{"error": "密码错误"})
		return
	}
	consoleauth.SetSessionCookieWithValue(w, consoleauth.AuthToken(a.password))
	writeJSON(w, http.StatusOK, obj{"authenticated": true, "ok": true})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	cors.Apply(w.Header(), r)
	consoleauth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, obj{"authenticated": false, "ok": true})
}

// --- Requests ---

func (a *API) handleRequests(w http.ResponseWriter, r *http.Request) {
	f := parseListFilter(r)
	items, total, err := a.requests.List(r.Context(), f)
	if err != nil {
		log.Printf("[console] list requests: %v", err)
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to list requests"})
		return
	}
	writeJSON(w, http.StatusOK, obj{"ok": true, "requests": items, "total": total})
}

func (a *API) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	// /__console/api/requests/:id
	id := strings.TrimPrefix(r.URL.Path, "/__console/api/requests/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, obj{"error": "not found"})
		return
	}
	a.handleRequestDetailFull(w, r, id)
}

// --- Providers ---

func (a *API) handleProviders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid body"})
			return
		}
		var m providerMutation
		if err := json.Unmarshal(body, &m); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid json"})
			return
		}
		if m.ChannelName == "" {
			writeJSON(w, http.StatusBadRequest, obj{"error": "channelName is required"})
			return
		}
		in, err := buildProviderInput(m, true)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		if err := a.provider.Upsert(ctx, m.ChannelName, in); err != nil {
			writeProviderError(w, err)
			return
		}
		_ = a.store.Refresh(ctx)
		row, err := a.provider.GetByChannel(ctx, m.ChannelName)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to load created provider"})
			return
		}
		writeJSON(w, http.StatusCreated, buildProviderInfo(row, 0))
		return
	}

	rows, err := a.provider.List(ctx)
	if err != nil {
		log.Printf("[console] list providers: %v", err)
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to list providers"})
		return
	}
	// Also load aliases so we can show the alias list per channel.
	aliases, _ := a.alias.List(ctx)
	aliasByChannel := map[string]int{}
	for _, al := range aliases {
		if al.Enabled != 0 {
			aliasByChannel[al.Provider]++
		}
	}
	out := make([]obj, 0, len(rows))
	for _, row := range rows {
		out = append(out, buildProviderInfo(row, aliasByChannel[row.ChannelName]))
	}
	writeJSON(w, http.StatusOK, obj{"providers": out})
}

// handleProviderDetail returns a single provider by channel name. Used for the
// provider edit form prefill.
func (a *API) handleProviderDetailByName(w http.ResponseWriter, r *http.Request, channel string) {
	row, err := a.provider.GetByChannel(r.Context(), channel)
	if err != nil {
		writeJSON(w, http.StatusNotFound, obj{"error": "provider not found"})
		return
	}
	writeJSON(w, http.StatusOK, buildProviderInfo(row, 0))
}

func (a *API) handleProviderDetail(w http.ResponseWriter, r *http.Request) {
	// Handles /:channel, /:channel/enabled, /:channel/test, etc.
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/__console/api/providers/"), "/")
	channel := parts[0]
	if channel == "" {
		writeJSON(w, http.StatusNotFound, obj{"error": "not found"})
		return
	}
	ctx := r.Context()
	switch {
	case len(parts) >= 2 && parts[1] == "enabled" && r.Method == http.MethodPatch:
		var body struct{ Enabled bool `json:"enabled"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid body"})
			return
		}
		if err := a.provider.SetEnabled(ctx, channel, body.Enabled); err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, obj{"ok": true})
	case len(parts) >= 2 && parts[1] == "test" && r.Method == http.MethodPost:
		a.handleProviderTest(w, r, channel)
		return
	case len(parts) >= 2 && parts[1] == "upstream-models" && r.Method == http.MethodGet:
		a.handleProviderUpstreamModels(w, r, channel)
		return
	case r.Method == http.MethodDelete:
		if err := a.provider.Delete(ctx, channel); err != nil {
			writeProviderError(w, err)
			return
		}
		_ = a.store.Refresh(ctx)
		writeJSON(w, http.StatusOK, obj{"ok": true})
	case r.Method == http.MethodPatch:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid body"})
			return
		}
		var m providerMutation
		if err := json.Unmarshal(body, &m); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid json"})
			return
		}
		existing, err := a.provider.GetByChannel(ctx, channel)
		if err != nil {
			writeProviderError(w, err)
			return
		}
		nextName := m.ChannelName
		if nextName == "" {
			nextName = channel
		}
		in, err := buildProviderInput(m, false)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		in.Enabled = existing.Enabled != 0
		if m.Auth == nil && existing.AuthHeader != nil && existing.AuthValue != nil {
			in.AuthHeader = existing.AuthHeader
			in.AuthValue = existing.AuthValue
		}
		if err := a.provider.Update(ctx, channel, nextName, in); err != nil {
			writeProviderError(w, err)
			return
		}
		_ = a.store.Refresh(ctx)
		row, err := a.provider.GetByChannel(ctx, nextName)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to load updated provider"})
			return
		}
		writeJSON(w, http.StatusOK, buildProviderInfo(row, 0))
		return
	default:
		// GET /__console/api/providers/:channel — return full provider info.
		if r.Method == http.MethodGet {
			a.handleProviderDetailByName(w, r, channel)
			return
		}
		writeJSON(w, http.StatusMethodNotAllowed, obj{"error": "method not allowed"})
	}
}

// --- Keys ---

func (a *API) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Name      string  `json:"name"`
			CostQuota *int64  `json:"cost_quota"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		created, err := a.apikey.Create(r.Context(), body.Name, body.CostQuota)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		// Return the { key, record } shape the dashboard expects: record is the
		// full ManagedApiKey (same shape as the list endpoint) so the frontend
		// can splice it straight into the table.
		writeJSON(w, http.StatusCreated, obj{
			"key":    created.RawKey,
			"record": buildKeyInfo(created.Row),
		})
		return
	}
	keys, err := a.apikey.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to list keys"})
		return
	}
	out := make([]obj, 0, len(keys))
	for _, k := range keys {
		out = append(out, buildKeyInfo(k))
	}
	writeJSON(w, http.StatusOK, obj{"keys": out})
}

// buildKeyInfo converts a console_api_keys row into the ManagedApiKey shape the
// dashboard expects (used by both the list and create endpoints so the shapes
// stay identical).
func buildKeyInfo(k schema.ConsoleAPIKey) obj {
	snap := repo.BuildQuotaSnapshot(k.CostQuotaMicrousd, k.CostUsedMicrousd)
	return obj{
		"id":             k.ID,
		"name":           k.Name,
		"prefix":         k.Prefix,
		"created_at":     k.CreatedAt,
		"last_used_at":   k.LastUsedAt,
		"allowed_models": repo.ParseAllowedModels(k.AllowedModelsJSON),
		"cost_quota":     snap.CostQuota,
		"cost_used":      snap.CostUsed,
		"quota_exhausted": snap.QuotaExhausted,
	}
}

func (a *API) handleKeyDetail(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/__console/api/keys/"), "/")
	id := parts[0]
	ctx := r.Context()
	switch {
	case len(parts) >= 2 && parts[1] == "allowed-models" && r.Method == http.MethodPatch:
		var body struct{ Models []string `json:"models"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		updated, err := a.apikey.SetAllowedModels(ctx, id, body.Models)
		if err != nil {
			writeJSON(w, http.StatusNotFound, obj{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, obj{"id": updated.ID, "name": updated.Name})
	case len(parts) >= 2 && parts[1] == "quota" && r.Method == http.MethodPatch:
		var body struct{ CostQuota *int64 `json:"cost_quota"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		updated, err := a.apikey.SetCostQuota(ctx, id, body.CostQuota)
		if err != nil {
			writeJSON(w, http.StatusNotFound, obj{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, obj{"id": updated.ID, "name": updated.Name})
	case r.Method == http.MethodDelete:
		if err := a.apikey.Delete(ctx, id); err != nil {
			writeJSON(w, http.StatusNotFound, obj{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, obj{"ok": true})
	case r.Method == http.MethodPatch:
		var body struct{ Name string `json:"name"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		updated, err := a.apikey.Rename(ctx, id, body.Name)
		if err != nil {
			writeJSON(w, http.StatusNotFound, obj{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, obj{"id": updated.ID, "name": updated.Name})
	default:
		k, err := a.apikey.Get(ctx, id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, obj{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, obj{"id": k.ID, "name": k.Name, "key": k.KeyValue})
	}
}

// --- Aliases ---

func (a *API) handleAliases(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid body"})
			return
		}
		var m aliasMutation
		if err := json.Unmarshal(body, &m); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid json"})
			return
		}
		if m.Alias == "" {
			writeJSON(w, http.StatusBadRequest, obj{"error": "alias is required"})
			return
		}
		in := buildAliasInput(m)
		created, err := a.alias.Create(ctx, in)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		_ = a.store.Refresh(ctx)
		writeJSON(w, http.StatusCreated, buildAliasInfo(created))
		return
	}

	rows, err := a.alias.List(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to list aliases"})
		return
	}
	out := make([]obj, 0, len(rows))
	for _, row := range rows {
		out = append(out, buildAliasInfo(row))
	}
	writeJSON(w, http.StatusOK, obj{"aliases": out})
}

func (a *API) handleAliasDetail(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/__console/api/model-aliases/"), "/")
	idStr := parts[0]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, obj{"error": "无效的 id"})
		return
	}
	ctx := r.Context()
	switch {
	case len(parts) >= 2 && parts[1] == "enabled" && r.Method == http.MethodPatch:
		var body struct{ Enabled bool `json:"enabled"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, err := a.alias.SetEnabled(ctx, id, body.Enabled); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		_ = a.store.Refresh(ctx)
		writeJSON(w, http.StatusOK, obj{"ok": true})
	case r.Method == http.MethodDelete:
		if err := a.alias.Delete(ctx, id); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		_ = a.store.Refresh(ctx)
		writeJSON(w, http.StatusOK, obj{"ok": true})
	case r.Method == http.MethodPatch:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid body"})
			return
		}
		var m aliasMutation
		if err := json.Unmarshal(body, &m); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": "invalid json"})
			return
		}
		in := buildAliasInput(m)
		updated, err := a.alias.Update(ctx, id, in)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		_ = a.store.Refresh(ctx)
		writeJSON(w, http.StatusOK, buildAliasInfo(updated))
	default:
		writeJSON(w, http.StatusOK, obj{"id": id})
	}
}

// --- Settings (returns the full view shape expected by the dashboard) ---

// codeDefaultTimeouts mirrors CODE_DEFAULT_GATEWAY_TIMEOUTS.
func codeDefaultTimeouts() obj {
	return obj{
		"defaultFirstByteTimeoutMs": 300000,
		"streamFirstByteTimeoutMs":  30000,
		"imageFirstByteTimeoutMs":   300000,
		"responseIdleTimeoutMs":     300000,
	}
}

// codeDefaultFailoverPolicy mirrors CODE_DEFAULT_GATEWAY_FAILOVER_POLICY.
func codeDefaultFailoverPolicy() obj {
	return obj{
		"enabled":               true,
		"retryAttempts":         1,
		"modelFallbackMode":     "same_model",
		"maxFallbackAttempts":   2,
		"customModelFallbacks":  []obj{},
		"retryOnTimeout":        true,
		"retryOnNetworkError":   true,
		"retryOnStatusCodes":    []interface{}{408, 429},
		"retryOnStatusRanges":   []interface{}{"5xx"},
	}
}

func (a *API) handleTimeouts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		if _, err := a.settings.Upsert(r.Context(), "gateway.timeouts", string(b)); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		// Return the full updated view so the frontend stays in sync.
		a.writeTimeoutView(w, r, body)
		return
	}
	val, updated, err := a.settings.Get(r.Context(), "gateway.timeouts")
	stored := map[string]interface{}{}
	if err == nil {
		_ = json.Unmarshal([]byte(val), &stored)
	}
	merged := mergePolicyWithDefaults(stored, codeDefaultTimeouts())
	merged["updatedAt"] = updated
	if err != nil {
		merged["updatedAt"] = nil
	}
	merged["defaults"] = codeDefaultTimeouts()
	merged["limits"] = obj{
		"firstByte":   obj{"min": 1000, "max": 900000},
		"responseIdle": obj{"min": 0, "max": 3600000, "allowZero": true},
	}
	merged["ok"] = true
	a.writeJSONStatus(w, http.StatusOK, merged)
}

func (a *API) handleFailover(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		if _, err := a.settings.Upsert(r.Context(), "gateway.failover", string(b)); err != nil {
			writeJSON(w, http.StatusBadRequest, obj{"error": err.Error()})
			return
		}
		a.writeFailoverView(w, r, body)
		return
	}
	val, updated, err := a.settings.Get(r.Context(), "gateway.failover")
	stored := map[string]interface{}{}
	if err == nil {
		_ = json.Unmarshal([]byte(val), &stored)
	}
	merged := mergePolicyWithDefaults(stored, codeDefaultFailoverPolicy())
	merged["updatedAt"] = updated
	if err != nil {
		merged["updatedAt"] = nil
	}
	merged["defaults"] = codeDefaultFailoverPolicy()
	merged["limits"] = obj{
		"retryAttempts":               obj{"min": 0, "max": 5},
		"maxFallbackAttempts":         obj{"min": 0, "max": 20},
		"customModelFallbackRules":    obj{"min": 0, "max": 100},
		"customModelFallbacksPerRule": obj{"min": 1, "max": 50},
	}
	merged["ok"] = true
	a.writeJSONStatus(w, http.StatusOK, merged)
}

func (a *API) writeTimeoutView(w http.ResponseWriter, r *http.Request, policy obj) {
	view := mergePolicyWithDefaults(policy, codeDefaultTimeouts())
	mergeInto(view, obj{
		"defaults": codeDefaultTimeouts(),
		"limits": obj{
			"firstByte":   obj{"min": 1000, "max": 900000},
			"responseIdle": obj{"min": 0, "max": 3600000, "allowZero": true},
		},
		"updatedAt": time.Now().UnixMilli(),
	})
	a.writeJSONStatus(w, http.StatusOK, view)
}

func (a *API) writeFailoverView(w http.ResponseWriter, r *http.Request, policy obj) {
	view := mergePolicyWithDefaults(policy, codeDefaultFailoverPolicy())
	mergeInto(view, obj{
		"defaults": codeDefaultFailoverPolicy(),
		"limits": obj{
			"retryAttempts":               obj{"min": 0, "max": 5},
			"maxFallbackAttempts":         obj{"min": 0, "max": 20},
			"customModelFallbackRules":    obj{"min": 0, "max": 100},
			"customModelFallbacksPerRule": obj{"min": 1, "max": 50},
		},
		"updatedAt": time.Now().UnixMilli(),
	})
	a.writeJSONStatus(w, http.StatusOK, view)
}

func (a *API) writeJSONStatus(w http.ResponseWriter, status int, body obj) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// mergePolicyWithDefaults starts with the code defaults, then overlays the
// supplied overrides. Used so PATCHes get the full shape even when only
// some fields are set.
func mergePolicyWithDefaults(overrides, defaults obj) obj {
	out := obj{}
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// mergeInto copies src entries into dst.
func mergeInto(dst, src obj) {
	for k, v := range src {
		dst[k] = v
	}
}

// --- helpers ---

type obj = map[string]interface{}

func writeJSON(w http.ResponseWriter, status int, body obj) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeProviderError(w http.ResponseWriter, err error) {
	msg := err.Error()
	status := http.StatusBadRequest
	if strings.Contains(msg, "不存在") {
		status = http.StatusNotFound
	}
	writeJSON(w, status, obj{"error": msg})
}

func parseListFilter(r *http.Request) consolestore.ListFilter {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	return consolestore.ListFilter{
		Limit:  limit,
		Offset: offset,
		Route:  q.Get("route"),
		Model:  q.Get("model"),
		Status: q.Get("status"),
		Search: q.Get("search"),
	}
}

// quiet unused-import guard for context (used in some handlers).
var _ = context.Background

// --- Stats & filters (minimal implementations for the dashboard) ---

// handleStats aggregates console_requests for the dashboard. Supports a `range`
// query param (1h, 24h, 7d, 30d). Returns the shape expected by the React
// dashboard's `useUsageStats` hook.
//
// Note: this is a simplified port of console-store.ts:buildUsageStats — it
// omits per-model cost (no catalog lookup), failover chain analysis, and
// per-cache-point details. A full port would replicate those aggregates.
func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	createdAfter := parseRangeMs(r.URL.Query().Get("range"))
	ctx := r.Context()

	// Overview.
	var overview struct {
		Total            int64
		CacheHits        int64
		CacheCreates     int64
		CacheMisses      int64
		Errors           int64
		Failovers        int64
		InputTokens      int64
		OutputTokens     int64
		TotalTokens      int64
	}
	overviewQuery := `
		SELECT
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_read_input_tokens), 0),
			COALESCE(SUM(cache_creation_input_tokens), 0),
			COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN failover_from IS NOT NULL AND failover_from != '' THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM console_requests
	`
	args := []interface{}{}
	if createdAfter > 0 {
		overviewQuery += " WHERE created_at >= $1"
		args = append(args, createdAfter)
	}
	var total, inputTok, outputTok, cacheRead, cacheCreate, errors, failovers int64
	err := a.pool.QueryRow(ctx, overviewQuery, args...).Scan(
		&inputTok, &outputTok, &cacheRead, &cacheCreate, &errors, &failovers, &total,
	)
	if err != nil {
		log.Printf("[console] stats overview: %v", err)
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to compute stats"})
		return
	}
	overview.Total = total
	overview.InputTokens = inputTok
	overview.OutputTokens = outputTok
	overview.TotalTokens = inputTok + outputTok
	overview.CacheHits = cacheRead
	overview.CacheCreates = cacheCreate
	overview.CacheMisses = total - cacheRead - cacheCreate
	if overview.CacheMisses < 0 {
		overview.CacheMisses = 0
	}
	overview.Errors = errors
	overview.Failovers = failovers
	hitRate := 0.0
	if total > 0 {
		hitRate = float64(cacheRead) / float64(total)
	}

	// Per-route and per-model buckets.
	bucketQuery := func(groupCol string) string {
		q := "SELECT " + groupCol + ", COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_input_tokens), 0), COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0) FROM console_requests"
		if createdAfter > 0 {
			q += " WHERE created_at >= $1"
		}
		q += " GROUP BY " + groupCol + " ORDER BY " + groupCol
		return q
	}
	mkBuckets := func(q string) []obj {
		rows, err := a.pool.Query(ctx, q, args...)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []obj
		for rows.Next() {
			var key string
			var requests, inTok, outTok, cacheR, errs int64
			if err := rows.Scan(&key, &requests, &inTok, &outTok, &cacheR, &errs); err != nil {
				continue
			}
			out = append(out, obj{
				"key": key, "label": key,
				"requests": requests, "errors": errs,
				"cache_hits": cacheR, "cache_creates": 0,
				"total_input_tokens": inTok, "total_output_tokens": outTok,
			})
		}
		return out
	}
	routeBuckets := mkBuckets(bucketQuery("route_prefix"))
	modelBuckets := mkBuckets(bucketQuery("request_model"))
	clientBuckets := mkBuckets(bucketQuery("api_key_name"))

	// Time-series: bucket by minute (1h), 5min (24h), hour (7d/30d).
	ts := buildTimeseries(ctx, a.pool, createdAfter)

	writeJSON(w, http.StatusOK, obj{
		"overview": obj{
			"total":              overview.Total,
			"cache_hits":         overview.CacheHits,
			"cache_creates":      overview.CacheCreates,
			"cache_misses":       overview.CacheMisses,
			"errors":             overview.Errors,
			"failovers":          overview.Failovers,
			"hit_rate":           hitRate,
			"total_input_tokens":  overview.InputTokens,
			"total_output_tokens": overview.OutputTokens,
			"total_tokens":       overview.TotalTokens,
			"storage_backend":    "postgresql",
			"retention_max_records": 50000,
		},
		"stats": obj{
			"routes":  routeBuckets,
			"models":  modelBuckets,
			"clients": clientBuckets,
		},
		"filters": obj{
			"routes":  extractKeys(routeBuckets, "key"),
			"models":  extractKeys(modelBuckets, "key"),
			"clients": extractKeys(clientBuckets, "key"),
		},
		"timeseries": ts,
	})
}

// handleFilters returns the distinct filter values used by the dashboard
// (routes, models, api_key_names). Lighter than /stats — no aggregation.
func (a *API) handleFilters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := func(col string) []string {
		rows, err := a.pool.Query(ctx, "SELECT DISTINCT "+col+" FROM console_requests WHERE "+col+" IS NOT NULL AND "+col+" != '' ORDER BY "+col)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err == nil && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	clients := q("api_key_name")
	// Replace null api_key_name with the conventional "__anonymous__" sentinel
	// the dashboard uses to represent unauthenticated traffic.
	for i, c := range clients {
		if c == "" {
			clients[i] = "__anonymous__"
		}
	}
	rows, _ := a.pool.Query(ctx, `
		SELECT DISTINCT api_key_name FROM console_requests
		WHERE api_key_name IS NOT NULL AND api_key_name != '' ORDER BY api_key_name
	`)
	clientLabels := []obj{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err == nil && n != "" {
				clientLabels = append(clientLabels, obj{"value": n, "label": n})
			}
		}
	}
	writeJSON(w, http.StatusOK, obj{
		"ok":      true,
		"routes":  q("route_prefix"),
		"models":  q("request_model"),
		"clients": clientLabels,
	})
}

// parseRangeMs converts the dashboard's `range` query param to a createdAfter
// epoch-ms cutoff. Returns 0 for unknown / empty (means "all time").
func parseRangeMs(s string) int64 {
	if s == "" {
		return 0
	}
	now := time.Now().UnixMilli()
	switch s {
	case "1h":
		return now - int64(time.Hour/time.Millisecond)
	case "24h":
		return now - int64(24*time.Hour/time.Millisecond)
	case "72h":
		return now - int64(72*time.Hour/time.Millisecond)
	case "7d":
		return now - int64(7*24*time.Hour/time.Millisecond)
	case "30d":
		return now - int64(30*24*time.Hour/time.Millisecond)
	}
	return 0
}

// buildTimeseries buckets requests by time based on the active range. Mirror's
// the original's bucket-size selection (1min/1h/1d) at a minimum level.
func buildTimeseries(ctx context.Context, pool *pgxpool.Pool, createdAfter int64) []obj {
	bucketSec := int64(3600) // default 1h
	now := time.Now().UnixMilli()
	switch {
	case createdAfter > now-int64(time.Hour/time.Millisecond):
		bucketSec = 60
	case createdAfter > now-int64(7*24*time.Hour/time.Millisecond):
		bucketSec = 300
	}
	// If createdAfter is 0 (all time), use a coarse 1-day bucket and limit rows.
	if createdAfter == 0 {
		createdAfter = now - 30*24*3600*1000 // last 30 days max
		bucketSec = 86400
	}
	// Generate empty buckets so the dashboard always shows a contiguous series.
	var points []obj
	for t := alignDown(createdAfter, bucketSec*1000); t < now; t += bucketSec * 1000 {
		points = append(points, obj{
			"bucket_start": t,
			"bucket_label": time.UnixMilli(t).Format("01-02 15:04"),
			"requests":     0, "errors": 0, "total_tokens": 0, "total_cost": 0,
		})
	}
	// Fill in actual data.
	rows, err := pool.Query(ctx, `
		SELECT
		  (created_at / ($1 * 1000)) * ($1 * 1000) AS bucket_start,
		  COUNT(*),
		  COALESCE(SUM(input_tokens + output_tokens), 0),
		  COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0)
		FROM console_requests
		WHERE created_at >= $2
		GROUP BY bucket_start
		ORDER BY bucket_start
	`, bucketSec, createdAfter)
	if err != nil {
		return points
	}
	defer rows.Close()
	// Index by bucket_start for fast updates.
	index := map[int64]int{}
	for i, p := range points {
		if bs, ok := p["bucket_start"].(int64); ok {
			index[bs] = i
		}
	}
	for rows.Next() {
		var bs int64
		var requests, tokens, errs int64
		if err := rows.Scan(&bs, &requests, &tokens, &errs); err != nil {
			continue
		}
		bs = alignDown(bs, bucketSec*1000)
		if i, ok := index[bs]; ok {
			points[i]["requests"] = requests
			points[i]["errors"] = errs
			points[i]["total_tokens"] = tokens
			points[i]["total_cost"] = 0 // catalog lookup not implemented
		}
	}
	return points
}

func alignDown(ms int64, granularity int64) int64 {
	return (ms / granularity) * granularity
}

func extractKeys(buckets []obj, key string) []string {
	out := make([]string, 0, len(buckets))
	for _, b := range buckets {
		if k, ok := b[key].(string); ok && k != "" {
			out = append(out, k)
		}
	}
	return out
}

// buildProviderInfo converts a console_providers row + alias count into the
// ProviderInfo shape the React dashboard expects. Mirrors the original
// rowToConfigEntry + buildProviderInfo.
func buildProviderInfo(row schema.ConsoleProvider, aliasCount int) obj {
	t := "openai"
	if row.Type == "anthropic" {
		t = "anthropic"
	}

	systemPrompt := ""
	if row.SystemPrompt != nil {
		systemPrompt = *row.SystemPrompt
	}
	models := parseModelsForAPI(row.ModelsJSON)

	auth := obj{}
	if row.AuthHeader != nil && row.AuthValue != nil {
		auth["header"] = *row.AuthHeader
		auth["value"] = *row.AuthValue
	}

	extra := map[string]interface{}{}
	if row.ExtraFieldsJSON != "" {
		_ = json.Unmarshal([]byte(row.ExtraFieldsJSON), &extra)
	}

	visibility := "direct"
	if row.RoutingVisibility == "explicit_only" {
		visibility = "explicit_only"
	}

	responsesMode := "native"
	if v, ok := extra["responsesMode"].(string); ok {
		responsesMode = v
	}

	out := obj{
		"channelName":       row.ChannelName,
		"type":              t,
		"targetBaseUrl":     row.TargetBaseURL,
		"systemPrompt":      systemPrompt,
		"priority":          row.Priority,
		"enabled":           row.Enabled != 0,
		"routingVisibility": visibility,
		"models":            models,
		"auth":              auth,
		"responsesMode":     responsesMode,
		"extraFields":       extra,
		"providerUuid":      row.ProviderUUID,
		"aliasCount":        aliasCount,
	}
	// Auth can be "null" too if unset — prefer explicit null for the field
	// the frontend uses to decide whether to show "Edit credentials".
	if row.AuthValue == nil {
		out["auth"] = nil
	}
	return out
}

// parseModelsForAPI decodes models_json into the array shape the dashboard
// expects: [{ model: string, context?: number, ... }, ...].
func parseModelsForAPI(raw string) []obj {
	if strings.TrimSpace(raw) == "" {
		return []obj{}
	}
	var raw2 []map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &raw2); err != nil {
		return []obj{}
	}
	out := make([]obj, 0, len(raw2))
	for _, m := range raw2 {
		entry := obj{}
		for k, v := range m {
			entry[k] = v
		}
		out = append(out, entry)
	}
	return out
}

// providerMutation mirrors the dashboard's ProviderMutationPayload.
type providerMutation struct {
	ChannelName       string          `json:"channelName"`
	Type              string          `json:"type"`
	TargetBaseURL     string          `json:"targetBaseUrl"`
	SystemPrompt      *string         `json:"systemPrompt"`
	Models            []json.RawMessage `json:"models"`
	Priority          int             `json:"priority"`
	RoutingVisibility *string         `json:"routingVisibility"`
	Auth              *struct {
		Header string `json:"header"`
		Value  string `json:"value"`
	} `json:"auth"`
	ResponsesMode *string                `json:"responsesMode"`
	ExtraFields   map[string]interface{} `json:"extraFields"`
}

// buildProviderInput turns a dashboard mutation payload into the repo input
// shape. For creates it supplies sensible defaults; for updates the caller
// should preserve the existing enabled state.
func buildProviderInput(m providerMutation, isCreate bool) (repo.ProviderInput, error) {
	in := repo.ProviderInput{
		Type:              m.Type,
		TargetBaseURL:     m.TargetBaseURL,
		SystemPrompt:      m.SystemPrompt,
		Models:            normalizeProviderModels(m.Models),
		Priority:          m.Priority,
		RoutingVisibility: "",
		Enabled:           isCreate,
		ExtraFields:       map[string]interface{}{},
	}
	if in.Type == "" {
		in.Type = "openai"
	}
	if m.RoutingVisibility != nil && *m.RoutingVisibility != "" {
		in.RoutingVisibility = *m.RoutingVisibility
	}
	// Accept auth with only a value: when the header is omitted the dashboard
	// means "use the default for this provider type" (authorization for openai,
	// x-api-key for anthropic). Without this, the common "auto" auth-header
	// selection would silently drop the credential.
	if m.Auth != nil && m.Auth.Value != "" {
		header := m.Auth.Header
		if header == "" {
			header = "authorization"
			if in.Type == string(configstore.Anthropic) {
				header = "x-api-key"
			}
		}
		in.AuthHeader = &header
		in.AuthValue = &m.Auth.Value
	}
	extra := m.ExtraFields
	if extra == nil {
		extra = map[string]interface{}{}
	}
	if m.ResponsesMode != nil && *m.ResponsesMode != "" {
		extra["responsesMode"] = *m.ResponsesMode
	}
	in.ExtraFields = extra
	return in, nil
}

func normalizeProviderModels(raw []json.RawMessage) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(raw))
	for _, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			out = append(out, map[string]interface{}{"model": s})
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(r, &obj); err == nil {
			out = append(out, obj)
		}
	}
	return out
}

// aliasMutation mirrors the dashboard's ModelAliasMutationPayload.
type aliasMutation struct {
	Alias       string            `json:"alias"`
	Provider    string            `json:"provider"`
	Model       string            `json:"model"`
	Targets     []repo.AliasTarget `json:"targets"`
	Description *string           `json:"description"`
	Visible     *bool             `json:"visible"`
	Enabled     *bool             `json:"enabled"`
}

// buildAliasInput turns a dashboard mutation payload into the repo input shape.
// If targets are absent but provider+model are present, it falls back to a
// single target so the legacy single-target alias form keeps working.
func buildAliasInput(m aliasMutation) repo.AliasInput {
	targets := m.Targets
	if len(targets) == 0 && m.Provider != "" && m.Model != "" {
		targets = []repo.AliasTarget{{Provider: m.Provider, Model: m.Model}}
	}
	return repo.AliasInput{
		Alias:       m.Alias,
		Provider:    m.Provider,
		Model:       m.Model,
		Targets:     targets,
		Description: m.Description,
		Visible:     m.Visible,
		Enabled:     m.Enabled,
	}
}

// buildAliasInfo converts a model_aliases row into the shape the dashboard
// expects. Mirrors the inline construction in handleAliases.
func buildAliasInfo(row schema.ModelAlias) obj {
	targets, _ := repo.ParseTargets(row)
	desc := ""
	if row.Description != nil {
		desc = *row.Description
	}
	return obj{
		"id":          row.ID,
		"alias":       row.Alias,
		"provider":    row.Provider,
		"model":       row.Model,
		"targets":     targets,
		"description": desc,
		"visible":     row.Visible != 0,
		"enabled":     row.Enabled != 0,
		"created_at":  row.CreatedAt,
		"updated_at":  row.UpdatedAt,
	}
}

// --- Models (enriched with pricing/overrides) ---

// handleModels returns the full model list (with pricing/override metadata)
// grouped by upstream type. Matches the dashboard's `fetchModels` contract.
func (a *API) handleModels(w http.ResponseWriter, r *http.Request) {
	if err := a.store.EnsureLoaded(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, obj{"error": "config unavailable"})
		return
	}
	resolver := routing.NewResolver(a.store.Snapshot())
	models := resolver.Models()

	// Load overrides and per-model pricing from DB.
	overrides, _ := loadModelOverrides(r.Context(), a.pool)
	pricing, _ := loadModelPricing(r.Context(), a.pool)

	out := obj{"openai": []obj{}, "anthropic": []obj{}}
	for _, m := range models {
		entry := obj{
			"id":          m.ID,
			"channelName": m.ChannelName,
			"type":        string(m.Type),
		}
		if m.Context != nil {
			entry["context"] = *m.Context
		}
		key := m.ChannelName + ":" + m.ID
		if p, ok := pricing[key]; ok {
			entry["pricing"] = p
		}
		if ov, ok := overrides[key]; ok {
			entry["override"] = ov
		}
		// Models with channelName="virtual-route" are aliases — group by the
		// model's resolved type from the alias target. The resolver already
		// stamped the right type via the first target's channel.
		out[string(m.Type)] = append(out[string(m.Type)].([]obj), entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleModelMetadata upserts a per-(channel, model) override. Used by the
// Models page "Edit metadata" form to set context/pricing per model.
//
// PUT /__console/api/models/:channel/:model/metadata
func (a *API) handleModelMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, obj{"error": "method not allowed"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/__console/api/models/"), "/")
	if len(parts) < 3 || parts[2] != "metadata" {
		writeJSON(w, http.StatusNotFound, obj{"error": "not found"})
		return
	}
	channel, err := url.PathUnescape(parts[0])
	if err != nil || channel == "" {
		writeJSON(w, http.StatusBadRequest, obj{"error": "invalid channel"})
		return
	}
	model, err := url.PathUnescape(parts[1])
	if err != nil || model == "" {
		writeJSON(w, http.StatusBadRequest, obj{"error": "invalid model"})
		return
	}
	var body struct {
		Context *int                    `json:"context"`
		Pricing map[string]interface{} `json:"pricing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, obj{"error": "invalid body"})
		return
	}
	var ctxWindow *int
	if body.Context != nil {
		ctxWindow = body.Context
	}
	var pricingJSON *string
	if body.Pricing != nil {
		b, _ := json.Marshal(body.Pricing)
		s := string(b)
		pricingJSON = &s
	}
	now := time.Now().UnixMilli()
	// Upsert: if exists, update; else insert.
	_, err = a.pool.Exec(r.Context(), `
		INSERT INTO model_metadata_overrides (channel_name, model_id, context_window, pricing_json, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (channel_name, model_id) DO UPDATE SET
		  context_window = EXCLUDED.context_window,
		  pricing_json = EXCLUDED.pricing_json,
		  updated_at = EXCLUDED.updated_at
	`, channel, model, ctxWindow, pricingJSON, now)
	if err != nil {
		log.Printf("[console] upsert model metadata: %v", err)
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to save"})
		return
	}
	// Refresh routing config cache so the new metadata is visible.
	_ = a.store.Refresh(r.Context())
	writeJSON(w, http.StatusOK, obj{
		"id":          model,
		"channelName": channel,
		"type":        "openai", // The frontend uses a simpler GatewayModel — pick from DB.
		"context":     ctxWindow,
		"pricing":     body.Pricing,
		"override": obj{
			"context":   ctxWindow,
			"pricing":   body.Pricing,
			"updatedAt": now,
		},
	})
}

// loadModelOverrides returns a map keyed by "channelName:modelId" → override obj.
func loadModelOverrides(ctx context.Context, pool *pgxpool.Pool) (map[string]obj, error) {
	rows, err := pool.Query(ctx, `SELECT channel_name, model_id, context_window, pricing_json, updated_at FROM model_metadata_overrides`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]obj{}
	for rows.Next() {
		var ch, model string
		var ctxWindow *int
		var pricingJSON *string
		var updatedAt int64
		if err := rows.Scan(&ch, &model, &ctxWindow, &pricingJSON, &updatedAt); err != nil {
			continue
		}
		entry := obj{"updatedAt": updatedAt}
		if ctxWindow != nil {
			entry["context"] = *ctxWindow
		}
		if pricingJSON != nil && *pricingJSON != "" {
			var p map[string]interface{}
			if err := json.Unmarshal([]byte(*pricingJSON), &p); err == nil {
				entry["pricing"] = p
			}
		}
		out[ch+":"+model] = entry
	}
	return out, nil
}

// loadModelPricing returns a map keyed by "channelName:modelId" → pricing obj.
func loadModelPricing(ctx context.Context, pool *pgxpool.Pool) (map[string]obj, error) {
	// Pricing is sourced from model_catalog_cache (one row per model).
	rows, err := pool.Query(ctx, `SELECT model_id, pricing_json FROM model_catalog_cache WHERE pricing_json IS NOT NULL AND pricing_json != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]obj{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			continue
		}
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue
		}
		out[id] = p // keyed by model id only (no channel)
	}
	return out, nil
}
