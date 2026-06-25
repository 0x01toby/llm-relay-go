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
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/catalog"
	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/consoleauth"
	"github.com/taozhang/llmrelay/internal/consolestore"
	"github.com/taozhang/llmrelay/internal/cors"
	"github.com/taozhang/llmrelay/internal/db"
	"github.com/taozhang/llmrelay/internal/pricing"
	"github.com/taozhang/llmrelay/internal/repo"
	"github.com/taozhang/llmrelay/internal/routing"
	"github.com/taozhang/llmrelay/internal/schema"
)

// API holds all the dependencies the console routes need.
type API struct {
	password  string
	gdb       *gorm.DB
	dialect   db.Dialect
	store     *configstore.Store
	catalog   catalogLooker
	provider  *repo.ProviderRepo
	alias     *repo.AliasRepo
	apikey    *repo.APIKeyRepo
	settings  *repo.SettingsRepo
	requests  *consolestore.Repository
	maxRecords int // retention cap, surfaced in the stats overview
}

// catalogLooker is the subset of catalog.Service the API needs: looking up a
// model's pricing. Defined as an interface so tests can stub it.
type catalogLooker interface {
	LookupPricing(modelID string) *pricing.ModelPricing
	LookupContext(modelID string) int
	// PricingMap returns every priced model (keyed by model id) for the Models
	// page. Implementations may return nil when no catalog is configured.
	PricingMap() map[string]map[string]interface{}
}

// New builds a console API handler. dialect is the detected backend, surfaced
// in the stats response's storage_backend field. cat supplies per-model
// pricing used to compute the cost column; pass nil to disable cost.
func New(gdb *gorm.DB, dialect db.Dialect, store *configstore.Store, cat *catalog.Service, password string, maxRecords int) *API {
	a := &API{
		password:   password,
		gdb:        gdb,
		dialect:    dialect,
		store:      store,
		provider:   repo.NewProviderRepo(gdb),
		alias:      repo.NewAliasRepo(gdb),
		apikey:     repo.NewAPIKeyRepo(gdb),
		settings:   repo.NewSettingsRepo(gdb),
		requests:   consolestore.New(gdb, maxRecords),
		maxRecords: maxRecords,
	}
	if cat != nil {
		a.catalog = cat
	}
	return a
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
	rows, total, err := a.requests.List(r.Context(), f)
	if err != nil {
		log.Printf("[console] list requests: %v", err)
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to list requests"})
		return
	}
	items := make([]obj, 0, len(rows))
	for i := range rows {
		items = append(items, buildListItem(&rows[i], a.catalog))
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
	f := consolestore.ListFilter{
		Limit:     limit,
		Offset:    offset,
		Route:     q.Get("route"),
		Model:     q.Get("model"),
		Status:    q.Get("status"),
		Search:    q.Get("search"),
		SortBy:    q.Get("sort_by"),
		SortOrder: q.Get("sort_order"),
	}
	if f.SortBy == "" {
		f.SortBy = "created_at"
	}
	if f.SortOrder == "" {
		f.SortOrder = "desc"
	}
	return f
}

// --- Stats & filters ---

// handleStats aggregates console_requests for the dashboard. Supports a `range`
// query param (1h, 24h, 7d, 30d). Returns the shape expected by the React
// dashboard's useUsageStats hook.
func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := rollupFilter{
		CreatedAfter: parseRangeMs(q.Get("range")),
		Route:        q.Get("route"),
		Model:        q.Get("model"),
		Client:       q.Get("client"),
	}
	ctx := r.Context()

	// All stats read from the pre-aggregated request_stats_5m rollup table
	// (populated by the background scheduler), so they stay accurate even after
	// old console_requests rows are pruned. Cost columns are pre-priced at
	// rollup time, so no per-row pricing pass is needed here. The route/model/
	// client filters narrow every query (overview, buckets, timeseries).
	overview, err := computeOverviewFromRollup(ctx, a.gdb, f)
	if err != nil {
		log.Printf("[console] stats overview: %v", err)
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to compute stats"})
		return
	}
	// hit_rate is the share of requests that read from cache, as a 0–100
	// percent (the dashboard renders it via toFixed(1) + "%").
	hitRate := 0.0
	if overview.Total > 0 {
		hitRate = float64(overview.CacheHits) / float64(overview.Total) * 100
	}
	cacheMisses := overview.Total - overview.CacheHits - overview.CacheCreates
	if cacheMisses < 0 {
		cacheMisses = 0
	}

	// Each bucket table groups by its own dimension; the other two filter
	// dimensions still apply. E.g. grouping by route while filtering model=X
	// shows per-route breakdown of that model's usage.
	routeBuckets, _ := computeBucketsFromRollup(ctx, a.gdb, "route_prefix", f)
	modelBuckets, _ := computeBucketsFromRollup(ctx, a.gdb, "request_model", f)
	clientBuckets, _ := computeBucketsFromRollup(ctx, a.gdb, "api_key_name", f)

	routeObj := rollupBucketsToObj(routeBuckets)
	modelObj := rollupBucketsToObj(modelBuckets)
	clientObj := rollupBucketsToObj(clientBuckets)
	ts := computeTimeseriesFromRollup(ctx, a.gdb, f)

	// Latency averages from pre-aggregated sums.
	var avgFirstChunk, avgFirstToken, avgDuration, avgGeneration *float64
	if overview.CountTimed > 0 {
		d := float64(overview.SumDurationMs) / float64(overview.CountTimed)
		avgDuration = &d
		// first-token avg uses count_timed as the denominator too (a timed
		// request has a first-token timestamp in the common case).
		ft := float64(overview.SumFirstTokenMs) / float64(overview.CountTimed)
		avgFirstToken = &ft
		avgFirstChunk = &ft // same column in rollup; distinct on the request row
		// generation = pure output time (completed - first_token).
		g := float64(overview.SumGenerationMs) / float64(overview.CountTimed)
		avgGeneration = &g
	}
	totalCost := overview.InputCost + overview.OutputCost + overview.CacheReadCost + overview.CacheWriteCost

	writeJSON(w, http.StatusOK, obj{
		"overview": obj{
			"total":                          overview.Total,
			"cache_hits":                     overview.CacheHits,
			"cache_creates":                  overview.CacheCreates,
			"cache_misses":                   cacheMisses,
			"errors":                         overview.Errors,
			"failovers":                      overview.Failovers,
			"hit_rate":                       hitRate,
			"total_input_tokens":             overview.InputTokens,
			"total_output_tokens":            overview.OutputTokens,
			"total_tokens":                   overview.InputTokens + overview.OutputTokens,
			"total_cache_read_tokens":        overview.CacheReadTokens,
			"total_cache_creation_tokens":    overview.CacheCreationTokens,
			"total_cached_input_tokens":      overview.CachedInputTokens,
			"total_reasoning_output_tokens":  overview.ReasoningTokens,
			"total_cost":                     roundUSD(totalCost),
			"total_input_cost":               roundUSD(overview.InputCost),
			"total_output_cost":              roundUSD(overview.OutputCost),
			"total_cache_read_cost":          roundUSD(overview.CacheReadCost),
			"total_cache_write_cost":         roundUSD(overview.CacheWriteCost),
			"avg_first_chunk_ms":             avgFirstChunk,
			"avg_first_token_ms":             avgFirstToken,
			"avg_duration_ms":                avgDuration,
			"avg_generation_ms":              avgGeneration,
			"storage_backend":                a.dialect.String(),
			"retention_max_records":          a.maxRecords,
		},
		"stats": obj{
			"routes":  routeObj,
			"models":  modelObj,
			"clients": clientObj,
		},
		"filters": obj{
			"routes":  extractKeys(routeObj, "key"),
			"models":  extractKeys(modelObj, "key"),
			"clients": extractKeys(clientObj, "key"),
		},
		"timeseries": ts,
	})
}

// handleFilters returns the distinct filter values used by the dashboard
// (routes, models, api_key_names). Lighter than /stats — no aggregation.
func (a *API) handleFilters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clients := distinctColumn(ctx, a.gdb, "api_key_name")
	// Replace empty api_key_name with the conventional "__anonymous__" sentinel
	// the dashboard uses to represent unauthenticated traffic.
	for i, c := range clients {
		if c == "" {
			clients[i] = "__anonymous__"
		}
	}
	clientLabels := make([]obj, 0, len(clients))
	for _, n := range clients {
		if n != "" {
			clientLabels = append(clientLabels, obj{"value": n, "label": n})
		}
	}
	writeJSON(w, http.StatusOK, obj{
		"ok":      true,
		"routes":  distinctColumn(ctx, a.gdb, "route_prefix"),
		"models":  distinctColumn(ctx, a.gdb, "request_model"),
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

	// Overrides are per-(channel, model) from the DB; pricing comes from the
	// in-memory catalog keyed by model id.
	overrides, _ := loadModelOverrides(r.Context(), a.gdb)
	var pricing map[string]map[string]interface{}
	if a.catalog != nil {
		pricing = a.catalog.PricingMap()
	}

	out := obj{"openai": []obj{}, "anthropic": []obj{}}
	for _, m := range models {
		entry := obj{
			"id":          m.ID,
			"channelName": m.ChannelName,
			"type":        string(m.Type),
		}
		key := m.ChannelName + ":" + m.ID
		// Context length precedence: manual override > provider config > catalog
		// (models.dev). The provider config (m.Context) is usually empty because
		// operators rarely type it in; the catalog fills it in automatically.
		if ov, ok := overrides[key]; ok && ov.Context != nil {
			entry["context"] = *ov.Context
		} else if m.Context != nil {
			entry["context"] = *m.Context
		} else if a.catalog != nil {
			if ctxWindow := a.catalog.LookupContext(m.ID); ctxWindow > 0 {
				entry["context"] = ctxWindow
			}
		}
		if ov, ok := overrides[key]; ok {
			entry["override"] = overrideEntryToObj(ov)
		}
		// Pricing is keyed by model id (catalog is cross-provider).
		if p, ok := pricing[m.ID]; ok {
			entry["pricing"] = p
		}
		// Models with channelName="virtual-route" are aliases — group by the
		// model's resolved type from the alias target. The resolver already
		// stamped the right type via the first target's channel.
		out[string(m.Type)] = append(out[string(m.Type)].([]obj), entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// overrideEntryToObj converts a modelOverrideEntry into the dashboard's override
// object shape { context?, pricing?, updatedAt }.
func overrideEntryToObj(e modelOverrideEntry) obj {
	out := obj{"updatedAt": e.UpdatedAt}
	if e.Context != nil {
		out["context"] = *e.Context
	}
	if e.Pricing != nil {
		out["pricing"] = e.Pricing
	}
	return out
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
	if err := upsertModelMetadata(r.Context(), a.gdb, channel, model, ctxWindow, pricingJSON); err != nil {
		log.Printf("[console] upsert model metadata: %v", err)
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to save"})
		return
	}
	// Refresh routing config cache so the new metadata is visible.
	_ = a.store.Refresh(r.Context())
	writeJSON(w, http.StatusOK, obj{
		"id":          model,
		"channelName": channel,
		"type":        "openai",
		"context":     ctxWindow,
		"pricing":     body.Pricing,
		"override": obj{
			"context":   ctxWindow,
			"pricing":   body.Pricing,
			"updatedAt": now,
		},
	})
}
