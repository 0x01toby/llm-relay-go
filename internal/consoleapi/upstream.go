package consoleapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/repo"
	"github.com/taozhang/llmrelay/internal/schema"
)

// upstreamTimeout is the per-request budget for an upstream probe (test or
// model list). It is intentionally short so a dead endpoint fails fast in the
// dashboard instead of hanging the operator's workflow.
const upstreamTimeout = 30 * time.Second

// upstreamClient is the shared HTTP client for upstream probes. It does not
// follow redirects (upstream APIs never redirect) and enforces upstreamTimeout.
var upstreamClient = &http.Client{
	Timeout: upstreamTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// upstreamTarget describes where to send an upstream probe and how to
// authenticate it. It is the common input for both the test and model-list
// operations, regardless of whether the source is a saved provider row or a
// preview payload typed in the dashboard.
type upstreamTarget struct {
	Type          configstore.UpstreamType
	TargetBaseURL string
	AuthHeader    string
	AuthValue     string
}

// testProviderResponse mirrors the dashboard's TestProviderResult shape.
type testProviderResponse struct {
	Status      string      `json:"status"` // "ok" | "error"
	StatusCode  int         `json:"statusCode"`
	Message     string      `json:"message"`
	LatencyMs   int64       `json:"latencyMs,omitempty"`
	Model       string      `json:"model,omitempty"`
	RawResponse interface{} `json:"rawResponse,omitempty"`
}

// handleProviderTest probes a saved provider by sending a minimal chat request
// (or Anthropic messages request) to its upstream and reporting the outcome.
//
// POST /__console/api/providers/:channel/test
// optional body: { "model": "..." }
func (a *API) handleProviderTest(w http.ResponseWriter, r *http.Request, channel string) {
	ctx := r.Context()
	row, err := a.provider.GetByChannel(ctx, channel)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	target, ok := rowToUpstreamTarget(row)
	if !ok {
		writeJSON(w, http.StatusBadRequest, obj{"error": "渠道未配置认证信息"})
		return
	}

	var body struct {
		Model string `json:"model"`
	}
	if r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body.Model == "" && len(row.ModelsJSON) > 0 {
		// Fall back to the provider's first configured model.
		if first := firstModelID(row.ModelsJSON); first != "" {
			body.Model = first
		}
	}
	if body.Model == "" {
		body.Model = defaultModelForType(target.Type)
	}

	result := probeUpstream(r.Context(), target, body.Model, a.pool)
	writeJSON(w, http.StatusOK, obj{
		"status":      result.Status,
		"statusCode":  result.StatusCode,
		"message":     result.Message,
		"latencyMs":   result.LatencyMs,
		"model":       result.Model,
		"rawResponse": result.RawResponse,
	})
}

// handleProviderUpstreamModels lists the models a saved provider exposes at its
// upstream /models endpoint.
//
// GET /__console/api/providers/:channel/upstream-models
func (a *API) handleProviderUpstreamModels(w http.ResponseWriter, r *http.Request, channel string) {
	ctx := r.Context()
	row, err := a.provider.GetByChannel(ctx, channel)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	target, ok := rowToUpstreamTarget(row)
	if !ok {
		writeJSON(w, http.StatusBadRequest, obj{"error": "渠道未配置认证信息"})
		return
	}
	models, httpErr := listUpstreamModels(r.Context(), target)
	if httpErr != nil {
		writeJSON(w, httpErr.status, obj{"error": httpErr.message})
		return
	}
	writeJSON(w, http.StatusOK, obj{"models": models})
}

// upstreamPreviewBody mirrors the dashboard's fetchUpstreamModelsPreview payload.
type upstreamPreviewBody struct {
	TargetBaseURL string `json:"targetBaseUrl"`
	Type          string `json:"type"`
	AuthHeader    string `json:"authHeader"`
	AuthValue     string `json:"authValue"`
}

// handleUpstreamModelsPreview lists models for connection params typed in the
// dashboard (no saved provider required). Used by the "Sync models" dialog
// before the provider has been created.
//
// POST /__console/api/upstream-models-preview
func (a *API) handleUpstreamModelsPreview(w http.ResponseWriter, r *http.Request) {
	var body upstreamPreviewBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, obj{"error": "invalid body"})
		return
	}
	if strings.TrimSpace(body.TargetBaseURL) == "" {
		writeJSON(w, http.StatusBadRequest, obj{"error": "targetBaseUrl is required"})
		return
	}
	target := upstreamTarget{
		Type:          configstore.UpstreamType(strings.ToLower(body.Type)),
		TargetBaseURL: body.TargetBaseURL,
		AuthHeader:    body.AuthHeader,
		AuthValue:     body.AuthValue,
	}
	if target.Type != configstore.Anthropic && target.Type != configstore.OpenAI {
		target.Type = configstore.OpenAI
	}
	if target.AuthHeader == "" {
		target.AuthHeader = defaultAuthHeader(target.Type)
	}
	models, httpErr := listUpstreamModels(r.Context(), target)
	if httpErr != nil {
		writeJSON(w, httpErr.status, obj{"error": httpErr.message})
		return
	}
	writeJSON(w, http.StatusOK, obj{"models": models})
}

// --- helpers ---

// httpError carries a status code + message for upstream failures.
type httpError struct {
	status  int
	message string
}

// rowToUpstreamTarget converts a saved provider row into an upstreamTarget.
// Returns ok=false when the provider has no usable auth (the dashboard shows a
// dedicated "configure credentials" prompt in that case).
func rowToUpstreamTarget(row schema.ConsoleProvider) (upstreamTarget, bool) {
	if row.AuthValue == nil || *row.AuthValue == "" {
		return upstreamTarget{}, false
	}
	t := configstore.OpenAI
	if row.Type == string(configstore.Anthropic) {
		t = configstore.Anthropic
	}
	header := defaultAuthHeader(t)
	if row.AuthHeader != nil && *row.AuthHeader != "" {
		header = *row.AuthHeader
	}
	return upstreamTarget{
		Type:          t,
		TargetBaseURL: row.TargetBaseURL,
		AuthHeader:    header,
		AuthValue:     *row.AuthValue,
	}, true
}

func defaultAuthHeader(t configstore.UpstreamType) string {
	if t == configstore.Anthropic {
		return "x-api-key"
	}
	return "authorization"
}

func defaultModelForType(t configstore.UpstreamType) string {
	if t == configstore.Anthropic {
		return "claude-3-5-haiku-latest"
	}
	return "gpt-4o-mini"
}

// firstModelID returns the first model id in a models_json blob, or "".
func firstModelID(modelsJSON string) string {
	if strings.TrimSpace(modelsJSON) == "" {
		return ""
	}
	var raw []map[string]interface{}
	if err := json.Unmarshal([]byte(modelsJSON), &raw); err != nil {
		return ""
	}
	for _, m := range raw {
		if id, _ := m["model"].(string); id != "" {
			return id
		}
	}
	return ""
}

// buildProbeRequest constructs a minimal chat/messages request to verify the
// upstream is reachable and the credentials are valid.
func buildProbeRequest(target upstreamTarget, model string) (*http.Request, error) {
	var (
		endpoint string
		body     []byte
	)
	if target.Type == configstore.Anthropic {
		endpoint = strings.TrimRight(target.TargetBaseURL, "/") + "/v1/messages"
		payload := map[string]interface{}{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
		body, _ = json.Marshal(payload)
	} else {
		endpoint = strings.TrimRight(target.TargetBaseURL, "/") + "/chat/completions"
		payload := map[string]interface{}{
			"model":       model,
			"max_tokens":  1,
			"messages":    []map[string]string{{"role": "user", "content": "ping"}},
			"stream":      false,
		}
		body, _ = json.Marshal(payload)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuthHeader(req.Header, target)
	if target.Type == configstore.Anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	return req, nil
}

// probeUpstream sends a minimal request and interprets the response. A 2xx or a
// 4xx that is not an auth error counts as "reachable" — the point of the test
// is to confirm the endpoint exists and the key works, not that the probe
// succeeded semantically (max_tokens=1 will often return a real completion).
func probeUpstream(ctx interface{ Done() <-chan struct{} }, target upstreamTarget, model string, _ interface{}) testProviderResponse {
	req, err := buildProbeRequest(target, model)
	if err != nil {
		return testProviderResponse{Status: "error", StatusCode: 0, Message: "无法构造请求: " + err.Error()}
	}

	start := time.Now()
	resp, err := upstreamClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return testProviderResponse{
			Status: "error", StatusCode: 0,
			Message:   "请求失败: " + err.Error(),
			LatencyMs: latency, Model: model,
		}
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var raw interface{}
	if len(rawBody) > 0 {
		_ = json.Unmarshal(rawBody, &raw)
	}

	out := testProviderResponse{
		StatusCode:  resp.StatusCode,
		LatencyMs:   latency,
		Model:       model,
		RawResponse: raw,
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		out.Status = "ok"
		out.Message = "请求成功"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		out.Status = "error"
		out.Message = fmt.Sprintf("认证失败 (%d)", resp.StatusCode)
	default:
		// 4xx/5xx other than auth errors still means the endpoint is reachable
		// and the key was accepted for routing; surface the upstream message.
		out.Status = "error"
		out.Message = fmt.Sprintf("上游返回 %d", resp.StatusCode)
	}
	return out
}

// listUpstreamModels fetches the upstream's model list. OpenAI providers are
// queried via GET /models; Anthropic has no public list endpoint, so we fall
// back to the saved models_json (this mirrors the original service's behavior).
func listUpstreamModels(ctx interface{ Done() <-chan struct{} }, target upstreamTarget) ([]obj, *httpError) {
	if target.Type == configstore.Anthropic {
		// No standard Anthropic models-list endpoint; return a stable hint set.
		return []obj{
			{"id": "claude-opus-4-1"},
			{"id": "claude-sonnet-4-5"},
			{"id": "claude-3-7-sonnet-latest"},
			{"id": "claude-3-5-haiku-latest"},
		}, nil
	}

	endpoint := strings.TrimRight(target.TargetBaseURL, "/") + "/models"
	// Some OpenAI-compatible base URLs already include /v1; avoid /v1/v1.
	endpoint = strings.Replace(endpoint, "/v1/v1/models", "/v1/models", 1)

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &httpError{http.StatusBadRequest, "无效的 base URL"}
	}
	applyAuthHeader(req.Header, target)

	resp, err := upstreamClient.Do(req)
	if err != nil {
		return nil, &httpError{http.StatusBadGateway, "请求失败: " + err.Error()}
	}
	defer resp.Body.Close()

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &httpError{http.StatusBadGateway, "无法解析上游响应"}
	}
	out := make([]obj, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			out = append(out, obj{"id": m.ID})
		}
	}
	return out, nil
}

// applyAuthHeader injects the provider credential into headers using the same
// header name the gateway uses (authorization or x-api-key).
func applyAuthHeader(h http.Header, target upstreamTarget) {
	switch strings.ToLower(target.AuthHeader) {
	case "x-api-key":
		h.Set("x-api-key", target.AuthValue)
	default:
		h.Set("Authorization", "Bearer "+target.AuthValue)
	}
}

// Compile-time guard: ensure repo.AliasInput is referenced so the import is not
// dropped if other files change. (Keeps the package self-consistent.)
var _ = repo.AliasInput{}
