package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/consolestore"
	"github.com/taozhang/llmrelay/internal/cors"
	"github.com/taozhang/llmrelay/internal/logtasks"
	"github.com/taozhang/llmrelay/internal/observer"
	"github.com/taozhang/llmrelay/internal/providers"
	"github.com/taozhang/llmrelay/internal/repo"
	"github.com/taozhang/llmrelay/internal/responsesconv"
	"github.com/taozhang/llmrelay/internal/routing"
	"gorm.io/gorm"
)

// Handler is the gateway proxy engine. It is mounted as the catch-all route
// and handles all non-console, non-static requests: model routing, auth,
// upstream forwarding with failover/retry, and response observation.
//
// It is constructed once at boot with all its dependencies (pool, repos,
// configstore). The per-request state lives in local variables, so a single
// Handler is safe for concurrent use.
type Handler struct {
	gdb          *gorm.DB
	store        *configstore.Store
	keyRepo      *repo.APIKeyRepo
	settingsRepo *repo.SettingsRepo
	adminKey     string
	timeouts     *timeoutCache
	failover     *failoverCache
	httpClient   *http.Client
	requests     *consolestore.Repository
	logtasks     *logtasks.Coordinator
}

// NewHandler builds a gateway Handler. requests and logtasks enable response
// observation; pass nil to disable logging (e.g. in degraded mode).
func NewHandler(gdb *gorm.DB, store *configstore.Store, adminKey string, cfgTimeouts TimeoutSettings, requests *consolestore.Repository, lt *logtasks.Coordinator) *Handler {
	settingsRepo := repo.NewSettingsRepo(gdb)
	return &Handler{
		gdb:          gdb,
		store:        store,
		keyRepo:      repo.NewAPIKeyRepo(gdb),
		settingsRepo: settingsRepo,
		adminKey:     adminKey,
		timeouts:     newTimeoutCache(settingsRepo, cfgTimeouts),
		failover:     newFailoverCache(settingsRepo),
		requests:     requests,
		logtasks:     lt,
		httpClient: &http.Client{
			// No overall timeout — per-request first-byte timeouts are enforced
			// via context cancellation in the request loop.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ModelListHandler serves GET /v1/models (and the typed variants). It returns
// the OpenAI-shaped model list built from the routing resolver.
func (h *Handler) ModelListHandler(filterType configstore.UpstreamType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h.store.EnsureLoaded(r.Context()); err != nil {
			writeJSON(w, 503, map[string]interface{}{"error": "config unavailable"})
			return
		}
		resolver := routing.NewResolver(h.store.Snapshot())
		all := resolver.Models()
		var models []map[string]interface{}
		for _, m := range all {
			if filterType != "" && m.Type != filterType {
				continue
			}
			entry := map[string]interface{}{
				"id": m.ID, "object": "model", "created": 0, "owned_by": "ai-proxy",
			}
			if m.Context != nil {
				entry["context_window"] = *m.Context
			}
			models = append(models, entry)
		}
		cors.Apply(w.Header(), r)
		writeJSON(w, 200, map[string]interface{}{"object": "list", "data": models})
	}
}

// ServeHTTP is the proxy catch-all. It mirrors handleProxyRequest in index.ts.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.store.EnsureLoaded(r.Context()); err != nil {
		cors.Apply(w.Header(), r)
		writeJSON(w, 503, map[string]interface{}{"error": "config unavailable"})
		return
	}
	h.handleProxy(w, r)
}

// extractModelFromBody parses the "model" field from a JSON request body.
func extractModelFromBody(body []byte) string {
	var partial struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return ""
	}
	return strings.TrimSpace(partial.Model)
}

// isStreamRequest reports whether the request body has stream:true.
func isStreamRequest(body []byte) bool {
	var partial struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return false
	}
	return partial.Stream
}

// handleProxy drives the full request pipeline for one inbound request.
func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	resolver := routing.NewResolver(h.store.Snapshot())

	// Read the request body once (we need it for model extraction, auth, and
	// forwarding).
	var rawBody []byte
	if r.Method == http.MethodPost {
		b, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			cors.Apply(w.Header(), r)
			writeJSON(w, 400, map[string]interface{}{"error": "failed to read request body"})
			return
		}
		rawBody = b
	}

	requestedModel := extractModelFromBody(rawBody)
	search := r.URL.RawQuery
	if search != "" {
		search = "?" + search
	}

	// Resolve the initial route.
	pathname := r.URL.Path
	typeForced := parseTypeForcedPrefix(pathname)
	lookupPath := pathname
	if typeForced != "" {
		// Strip /openai or /anthropic prefix.
		lookupPath = strings.TrimPrefix(pathname, "/"+string(typeForced))
	}

	explicit := resolver.ResolveRoute(lookupPath, search)
	var candidates []*routing.RouteResult
	if explicit != nil {
		candidates = []*routing.RouteResult{explicit}
	} else {
		candidates = resolver.ResolveRoutesByModel(lookupPath, search, requestedModel, typeForced)
	}

	if len(candidates) == 0 {
		cors.Apply(w.Header(), r)
		msg := "未找到有效的服务配置"
		if requestedModel != "" {
			msg = "模型 '" + requestedModel + "' 未配置或不可用"
		}
		writeJSON(w, 400, map[string]interface{}{"error": msg})
		return
	}

	// Authenticate against the initial route's provider type.
	authResult := AuthenticateGateway(r.Header, candidates[0].Type, requestedModel, h.adminKey, h.keyRepo)
	if authResult.ErrorResponse != nil && !authResult.OK {
		cors.Apply(w.Header(), r)
		authResult.ErrorResponse(w)
		return
	}
	// Quota/model restriction errors (OK=true but ErrorResponse set).
	if authResult.ErrorResponse != nil {
		cors.Apply(w.Header(), r)
		authResult.ErrorResponse(w)
		return
	}

	// Failover setup.
	ts := h.timeouts.Get(r.Context())
	policy := h.failover.Get(r.Context())
	streamReq := isStreamRequest(rawBody)

	// Build the full candidate list: initial + same-model repeats + custom
	// fallbacks + any-model. The loop consumes them in order.
	allCandidates := h.buildFailoverCandidates(resolver, candidates, explicit, requestedModel, lookupPath, search, typeForced, policy)
	attempted := map[string]bool{}

	for idx, route := range allCandidates {
		key := route.ChannelName + ":" + route.ResolvedModel + ":" + route.TargetURL
		if attempted[key] && idx > 0 {
			continue
		}
		attempted[key] = true

		// Retry within this route up to policy.RetryAttempts.
		maxTries := 1
		if explicit == nil {
			maxTries = policy.RetryAttempts + 1
		}
		for try := 0; try < maxTries; try++ {
			status, retried := h.forwardOnce(w, r, route, rawBody, ts, streamReq, policy, requestedModel, authResult.APIKey)
			if !retried {
				return // response written
			}
			if try+1 >= maxTries {
				// Exhausted retries on this route; if there's a next candidate,
				// the outer loop continues. Otherwise we already wrote an error.
				if idx == len(allCandidates)-1 && status != 0 {
					// Last candidate failed terminally; forwardOnce wrote the error.
					return
				}
			}
		}
	}
}

// buildFailoverCandidates assembles the ordered list of routes to try.
func (h *Handler) buildFailoverCandidates(resolver *routing.Resolver, initial []*routing.RouteResult, explicit *routing.RouteResult, model, pathname, search string, forcedType configstore.UpstreamType, policy FailoverPolicy) []*routing.RouteResult {
	if explicit != nil || !policy.Enabled || policy.MaxFallbackAttempts <= 0 {
		return initial
	}
	out := append([]*routing.RouteResult{}, initial...)

	// Custom model fallbacks.
	if fbModels := CustomFallbackModels(policy, model); len(fbModels) > 0 {
		out = append(out, resolver.ResolveRoutesForFallbackModels(pathname, search, fbModels, forcedType)...)
	}
	// Site-policy fallbacks.
	switch policy.ModelFallbackMode {
	case FallbackAnyModel:
		out = append(out, resolver.ResolveRoutesForAnyModelFallback(pathname, search, forcedType)...)
	case FallbackSameModel:
		out = append(out, initial...)
	}
	return dedupeCandidates(out)
}

func dedupeCandidates(routes []*routing.RouteResult) []*routing.RouteResult {
	seen := map[string]bool{}
	out := routes[:0]
	for _, rt := range routes {
		key := rt.ChannelName + ":" + rt.ResolvedModel + ":" + rt.TargetURL
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, rt)
	}
	return out
}

// forwardOnce sends the request to one route and writes the response. Returns
// (statusCode, shouldRetry). When shouldRetry is true, no response has been
// written yet and the caller should try the next candidate/retry.
func (h *Handler) forwardOnce(w http.ResponseWriter, r *http.Request, route *routing.RouteResult, rawBody []byte, ts TimeoutSettings, streamReq bool, policy FailoverPolicy, requestedModel string, apiKey *AuthenticatedAPIKey) (int, bool) {
	// Build the upstream request.
	forwardBody := rawBody
	targetURL := route.TargetURL

	// Responses → Chat Completions conversion.
	if r.Method == http.MethodPost && route.Type == configstore.OpenAI && responsesconv.IsOpenAIResponsesEndpointPath(r.URL.Path) {
		if route.ResponsesMode == configstore.ResponsesDisabled {
			cors.Apply(w.Header(), r)
			responsesconv.WriteErrorResponse(w, responsesconv.CompatError{
				Status: 400, Message: "Responses endpoint is disabled for this provider.",
			})
			return 400, false
		}
		if route.ResponsesMode == configstore.ResponsesChatCompat {
			conv := responsesconv.ConvertResponsesRequestToChatCompletions(string(rawBody), &responsesconv.RequestOptions{TargetURL: targetURL})
			if !conv.OK {
				cors.Apply(w.Header(), r)
				responsesconv.WriteErrorResponse(w, conv.Error)
				return 400, false
			}
			forwardBody = []byte(conv.Body)
			targetURL = responsesconv.RewriteResponsesTargetURLToChatCompletions(route.TargetURL)
		}
	}

	// Rewrite model if alias resolved to a different model.
	if route.ResolvedModel != "" && requestedModelFromBody(forwardBody) != route.ResolvedModel {
		forwardBody = rewriteModel(forwardBody, route.ResolvedModel)
	}

	// Build the upstream HTTP request.
	ctx := r.Context()
	timeoutMs := SelectFirstByteTimeout(r.URL.Path, targetURL, ts, streamReq)
	if timeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()
	}

	var bodyReader io.Reader
	if r.Method == http.MethodPost {
		bodyReader = bytes.NewReader(forwardBody)
	}
	upReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bodyReader)
	if err != nil {
		h.writeTerminalError(w, route, 502, "Upstream request failed", err.Error())
		return 502, false
	}
	upReq.Header = BuildForwardHeaders(r.Header, route.Auth)
	if r.Method == http.MethodPost {
		upReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.httpClient.Do(upReq)
	log.Printf("[DEBUG] httpClient.Do: err=%v resp=%v targetURL=%s", err, resp != nil, targetURL)
	if err != nil {
		// Network/timeout error: decide whether to retry.
		isTimeout := ctx.Err() == context.DeadlineExceeded
		trigger := FailoverTrigger{Kind: TriggerNetworkError}
		if isTimeout {
			trigger.Kind = TriggerTimeout
		}
		if ShouldTriggerFailover(policy, trigger) {
			log.Printf("[REQ_FAILOVER] %s route=%s reason=%s", r.URL.Path, route.ChannelName, DescribeTrigger(trigger))
			return 0, true
		}
		if isTimeout {
			h.writeTerminalError(w, route, 504, "Upstream timeout", fmt.Sprintf("No first byte received within %ds", timeoutMs/1000))
		} else {
			h.writeTerminalError(w, route, 502, "Upstream request failed", err.Error())
		}
		return 0, false
	}

	// Status-based failover (non-streaming only).
	if !streamReq && resp.StatusCode >= 400 && ShouldTriggerFailover(policy, FailoverTrigger{Kind: TriggerStatus, Status: resp.StatusCode}) {
		resp.Body.Close()
		log.Printf("[REQ_FAILOVER_STATUS] %s route=%s status=%d", r.URL.Path, route.ChannelName, resp.StatusCode)
		return resp.StatusCode, true
	}

	// Apply Responses → Chat response conversion if the request was converted.
	if r.Method == http.MethodPost && route.Type == configstore.OpenAI && route.ResponsesMode == configstore.ResponsesChatCompat && responsesconv.IsOpenAIResponsesEndpointPath(r.URL.Path) {
		converted := responsesconv.TransformResponse(resp)
		resp = converted
	}

	// Stream the response to the client, capturing it for observation.
	h.streamResponse(w, r, resp, route, forwardBody, rawBody, requestedModel, apiKey)
	resp.Body.Close()
	return resp.StatusCode, false
}

// requestLogMeta carries the data needed to record a request + response log.
type requestLogMeta struct {
	requestID    string
	createdAt    int64
	route        *routing.RouteResult
	method       string
	path         string
	requestModel string
	originalBody []byte // the inbound request body as received (pre-conversion)
	forwardBody  []byte // the body actually sent upstream (may differ after conversion/model rewrite)
	apiKey       *AuthenticatedAPIKey
}

// streamResponse copies the upstream response to the client (flushing for SSE)
// while a background observer captures the body for the console log.
func (h *Handler) streamResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, route *routing.RouteResult, forwardBody []byte, rawBody []byte, requestModel string, apiKey *AuthenticatedAPIKey) {
	cors.Apply(w.Header(), r)
	// Copy headers (minus hop-by-hop).
	for k, vs := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	contentType := resp.Header.Get("Content-Type")
	// SSE: ensure no buffering.
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/event-stream") {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)

	// Build the request-log metadata (used for the INSERT).
	createdAt := time.Now().UnixMilli()
	requestID := generateRequestID()
	meta := requestLogMeta{
		requestID: requestID, createdAt: createdAt, route: route,
		method: r.Method, path: r.URL.Path, requestModel: requestModel,
		originalBody: rawBody, forwardBody: forwardBody, apiKey: apiKey,
	}

	// Persist the request snapshot asynchronously (the INSERT).
	h.recordRequest(meta, r, forwardBody, rawBody)

	// Tee-split the response body: client gets ClientBody(), observer gets a copy.
	var clientBody io.Reader = resp.Body
	var capturer *observer.Capturer
	if h.requests != nil && r.Method == http.MethodPost {
		capturer = observer.NewCapturer(resp.Body, route.Type, contentType, createdAt)
		clientBody = capturer.ClientBody()
	}

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		n, err := clientBody.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}

	// If we captured the response, record it asynchronously.
	if capturer != nil {
		capturer.Close()
		h.recordResponse(requestID, route, resp, capturer)
	}
}

// recordRequest asynchronously inserts the request snapshot into console_requests.
func (h *Handler) recordRequest(meta requestLogMeta, r *http.Request, forwardBody []byte, rawBody []byte) {
	if h.requests == nil || h.logtasks == nil {
		return
	}
	fwdPayload := string(forwardBody)
	fwdTruncated := false
	if len(fwdPayload) > consolestorePayloadLimit {
		fwdPayload = fwdPayload[:consolestorePayloadLimit]
		fwdTruncated = true
	}
	snap := consolestore.RequestSnapshot{
		RequestID:    meta.requestID,
		CreatedAt:    meta.createdAt,
		RoutePrefix:  meta.route.ChannelName,
		UpstreamType: string(meta.route.Type),
		Method:       meta.method,
		Path:         meta.path,
		TargetURL:    meta.route.TargetURL,
		RequestModel: meta.requestModel,
		ForwardedPayload:   &fwdPayload,
		ForwardedTruncated: fwdTruncated,
		SourceRequestType:  "generic",
	}
	// Preserve the original inbound body (before any Responses→Chat conversion
	// or model rewrite) so the detail view can show what the client sent. This
	// is only meaningful for POST bodies; GET/DELETE have none.
	if len(rawBody) > 0 {
		orig := string(rawBody)
		if len(orig) > consolestorePayloadLimit {
			orig = orig[:consolestorePayloadLimit]
			snap.OriginalTruncated = true
		}
		snap.OriginalPayload = &orig
	}
	if meta.apiKey != nil {
		snap.APIKeyID = &meta.apiKey.ID
		snap.APIKeyName = &meta.apiKey.Name
	}
	h.logtasks.TrackRequestWrite(meta.requestID, func() {
		ctx := context.Background()
		if err := h.requests.SaveRequest(ctx, snap); err != nil {
			log.Printf("[console] save request %s: %v", meta.requestID, err)
		}
	})
}

// recordResponse asynchronously updates the request row with response data.
// It waits for the INSERT (via logtasks serialization) before updating.
func (h *Handler) recordResponse(requestID string, route *routing.RouteResult, resp *http.Response, capturer *observer.Capturer) {
	if h.requests == nil || h.logtasks == nil {
		return
	}
	status := resp.StatusCode
	statusText := resp.Status
	h.logtasks.Track(func() {
		// Wait for the request INSERT to land first (ordered writes per requestID).
		<-h.logtasks.WaitForRequest(requestID)

		result, ok := <-capturer.ObserveDone()
		if !ok {
			return
		}
		snap := consolestore.ResponseSnapshot{
			RequestID:          requestID,
			ResponseStatus:    &status,
			ResponseStatusText: &statusText,
			ResponseBodyBytes:  result.BodyBytes,
			FirstChunkAt:       result.FirstChunkAt,
			FirstTokenAt:       result.FirstTokenAt,
			CompletedAt:        result.CompletedAt,
			HasStreamingContent: result.HasStreaming,
			Usage:              result.Usage,
			ResponseModel:      strPtrOrNil(result.Usage.Model),
			StopReason:         strPtrOrNil(result.Usage.StopReason),
		}
		if result.Body != "" {
			payload := result.Body
			snap.ResponsePayload = &payload
			snap.ResponseTruncated = result.Truncated
			if result.TruncationReason != "" {
				reason := string(result.TruncationReason)
				snap.ResponseTruncReason = &reason
			}
		}
		ctx := context.Background()
		if err := h.requests.SaveResponse(ctx, snap); err != nil {
			log.Printf("[console] save response %s: %v", requestID, err)
		}
	})
}

// consolestorePayloadLimit caps how much of a payload we store.
const consolestorePayloadLimit = 5 * 1024 * 1024

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// generateRequestID produces a short unique ID (8 hex chars), mirroring the
// original's crypto.randomUUID().slice(0, 8).
func generateRequestID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := [8]byte{}
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0xf]
	}
	return string(out[:])
}

func (h *Handler) writeTerminalError(w http.ResponseWriter, route *routing.RouteResult, status int, message, details string) {
	cors.Apply(w.Header(), nil)
	writeGatewayError(w, route.Type, status, message, details)
}

func isHopByHop(header string) bool {
	low := strings.ToLower(header)
	for _, h := range hopByHopHeaders {
		if low == h {
			return true
		}
	}
	return false
}

func requestedModelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var partial struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return ""
	}
	return partial.Model
}

func rewriteModel(body []byte, model string) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = model
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// parseTypeForcedPrefix detects /openai/* or /anthropic/* type-forcing.
func parseTypeForcedPrefix(pathname string) configstore.UpstreamType {
	if strings.HasPrefix(pathname, "/openai/") {
		return configstore.OpenAI
	}
	if strings.HasPrefix(pathname, "/anthropic/") {
		return configstore.Anthropic
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// UsageOfProvider is a convenience that delegates to the providers package,
// used by the response observer (P5 will wire the full observation).
func UsageOfProvider(body string, t configstore.UpstreamType) providers.UsageData {
	return providers.ParseUsage(body, providers.UpstreamType(t))
}
