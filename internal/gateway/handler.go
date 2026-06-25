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
	"os"
	"strings"
	"time"

	"github.com/taozhang/llmrelay/internal/catalog"
	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/consolestore"
	"github.com/taozhang/llmrelay/internal/cors"
	"github.com/taozhang/llmrelay/internal/logtasks"
	"github.com/taozhang/llmrelay/internal/observer"
	"github.com/taozhang/llmrelay/internal/pricing"
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
	pricing      priceLooker // catalog for per-request cost → quota consumption
}

// priceLooker is the subset of catalog.Service the gateway needs: resolving a
// model's per-1M-token pricing so it can charge managed API keys.
type priceLooker interface {
	LookupPricing(modelID string) *pricing.ModelPricing
}

// NewHandler builds a gateway Handler. requests and logtasks enable response
// observation; pass nil to disable logging (e.g. in degraded mode). cat supplies
// per-model pricing for quota consumption; pass nil to disable quota charging.
func NewHandler(gdb *gorm.DB, store *configstore.Store, adminKey string, cfgTimeouts TimeoutSettings, requests *consolestore.Repository, lt *logtasks.Coordinator, cat *catalog.Service) *Handler {
	settingsRepo := repo.NewSettingsRepo(gdb)
	h := &Handler{
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
	if cat != nil {
		h.pricing = cat
	}
	return h
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
	m, _ := extractModelAndStream(body)
	return m
}

// isStreamRequest reports whether the request body has stream:true.
func isStreamRequest(body []byte) bool {
	_, s := extractModelAndStream(body)
	return s
}

// extractModelAndStream parses both "model" and "stream" in a single JSON
// unmarshal (they were previously decoded in two passes over the same body).
func extractModelAndStream(body []byte) (model string, stream bool) {
	var partial struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return "", false
	}
	return strings.TrimSpace(partial.Model), partial.Stream
}

// handleProxy drives the full request pipeline for one inbound request.
func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	resolver := routing.NewResolver(h.store.Snapshot())

	// Read the request body once (we need it for model extraction, auth, and
	// forwarding). Cap at maxRequestBodyBytes to prevent OOM from oversized
	// payloads — an honest LLM request is well under this limit.
	const maxRequestBodyBytes = 20 * 1024 * 1024 // 20 MiB
	var rawBody []byte
	if r.Method == http.MethodPost {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
		_ = r.Body.Close()
		if err != nil {
			cors.Apply(w.Header(), r)
			writeJSON(w, 400, map[string]interface{}{"error": "failed to read request body"})
			return
		}
		if len(b) > maxRequestBodyBytes {
			cors.Apply(w.Header(), r)
			writeJSON(w, 413, map[string]interface{}{"error": "request body too large", "limit_bytes": maxRequestBodyBytes})
			return
		}
		rawBody = b
	}

	// Parse model + stream in a single JSON pass; pass them down to avoid
	// re-parsing the body multiple times (was 3-4x before).
	requestedModel, streamReq := extractModelAndStream(rawBody)
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
	// streamReq was already parsed at the top of handleProxy.

	// Build the full candidate list: initial + same-model repeats + custom
	// fallbacks + any-model. The loop consumes them in order.
	allCandidates := h.buildFailoverCandidates(resolver, candidates, explicit, requestedModel, lookupPath, search, typeForced, policy)
	attempted := map[string]bool{}
	wroteResponse := false // tracks whether any forwardOnce wrote a response

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
				return // response written successfully or terminally
			}
			if status != 0 {
				// forwardOnce wrote a terminal error (non-network failure).
				wroteResponse = true
			}
			if try+1 >= maxTries {
				if idx == len(allCandidates)-1 && wroteResponse {
					return // last candidate, terminal error already written
				}
			}
		}
	}

	// All candidates exhausted via network errors (status=0, no response written).
	// Send a terminal 502 so the client gets a real error instead of empty 200.
	if !wroteResponse {
		cors.Apply(w.Header(), r)
		writeJSON(w, 502, map[string]interface{}{
			"type": "upstream_error",
			"error": map[string]interface{}{
				"type":    "all_upstreams_failed",
				"message": "All upstream routes failed",
			},
		})
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

// dedupeCandidates removes duplicate routes by (channel, model, targetURL).
// NOTE: it reuses the input slice's backing array (out := routes[:0]). This is
// safe because the only caller (buildFailoverCandidates) builds a fresh slice
// via append and never reads it after this call. Do not reuse this function on
// borrowed slices without copying first.
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

	// Rewrite model if alias resolved to a different model. requestedModel was
	// already parsed once at the top of handleProxy; compare against that
	// instead of re-parsing forwardBody's JSON.
	if route.ResolvedModel != "" && requestedModel != route.ResolvedModel {
		forwardBody = rewriteModel(forwardBody, route.ResolvedModel)
	}

	// Build the upstream HTTP request.
	//
	// First-byte timeout: we create a cancellable context for the upstream
	// request, but enforce the first-byte deadline via a timer that calls cancel
	// only if the response headers haven't arrived yet. Once headers arrive
	// (httpClient.Do returns), we stop the timer so the context stays alive for
	// the full streaming read — otherwise a 30s first-byte timeout would
	// truncate any stream that takes longer than 30s to complete, even though
	// the first byte arrived in milliseconds.
	ctx := r.Context()
	timeoutMs := SelectFirstByteTimeout(r.URL.Path, targetURL, ts, streamReq)
	reqCtx, reqCancel := context.WithCancel(ctx)
	defer reqCancel() // covers ALL return paths; safe no-op once timer is stopped
	var firstByteTimer *time.Timer
	if timeoutMs > 0 {
		firstByteTimer = time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
			reqCancel()
		})
	}

	var bodyReader io.Reader
	if r.Method == http.MethodPost {
		bodyReader = bytes.NewReader(forwardBody)
	}
	upReq, err := http.NewRequestWithContext(reqCtx, r.Method, targetURL, bodyReader)
	if err != nil {
		if firstByteTimer != nil {
			firstByteTimer.Stop()
		}
		h.writeTerminalError(w, route, 502, "Upstream request failed", err.Error())
		return 502, false
	}
	upReq.Header = BuildForwardHeaders(r.Header, route.Auth)
	if r.Method == http.MethodPost {
		upReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.httpClient.Do(upReq)
	if err != nil {
		// Did the first-byte timer fire? Stop() returns false if it already ran.
		timerFired := firstByteTimer != nil && !firstByteTimer.Stop()
		isTimeout := timerFired
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

	// Got the response headers (first byte). Stop the first-byte timer so the
	// request context stays alive for the full streaming read. The body now
	// lives as long as the client's request context (r.Context()).
	if firstByteTimer != nil {
		firstByteTimer.Stop()
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
	// Wrap the body reader with an idle timeout so a stalled upstream (data
	// stops flowing mid-stream) doesn't hold the request open forever.
	// ResponseIdleMs comes from the gateway timeout settings (default 300s).
	idleMs := h.timeouts.Get(r.Context()).ResponseIdleMs
	reader := newIdleReader(clientBody, idleMs)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			// Detect client disconnect: if Write fails the client is gone, so
			// stop reading upstream (saves bandwidth) and let the observer
			// goroutine finish via capturer.Close() below.
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
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
		h.recordResponse(requestID, route, resp, capturer, apiKey)
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
func (h *Handler) recordResponse(requestID string, route *routing.RouteResult, resp *http.Response, capturer *observer.Capturer, apiKey *AuthenticatedAPIKey) {
	if h.requests == nil || h.logtasks == nil {
		return
	}
	status := resp.StatusCode
	statusText := resp.Status
	h.logtasks.Track(func() {
		// Wait for the request INSERT to land first (ordered writes per requestID).
		// Add a timeout guard: if the INSERT goroutine panics or is starved, we
		// don't want to block forever.
		select {
		case <-h.logtasks.WaitForRequest(requestID):
		case <-time.After(30 * time.Second):
			log.Printf("[console] timed out waiting for request INSERT %s", requestID)
			return
		}

		// Wait for the observer to finish parsing the stream. Add a timeout
		// guard so a stuck capturer can't block this goroutine indefinitely.
		var result observer.Result
		var ok bool
		select {
		case result, ok = <-capturer.ObserveDone():
		case <-time.After(30 * time.Second):
			log.Printf("[console] observe timeout for %s", requestID)
			return
		}
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

		// Consume the managed API key's quota: price the response's token usage
		// via the catalog and atomically increment cost_used_microusd. Admin
		// requests (no apiKey) skip this. This is what makes configured quotas
		// actually enforce — without it cost_used stays at 0 forever.
		if apiKey != nil && apiKey.ID != "" {
			h.chargeQuota(ctx, apiKey.ID, result.Usage)
		}
	})
}

// chargeQuota prices one response's usage and increments the key's spent total.
func (h *Handler) chargeQuota(ctx context.Context, keyID string, usage providers.UsageData) {
	if h.pricing == nil || h.keyRepo == nil {
		return
	}
	modelID := usage.Model
	if modelID == "" {
		return
	}
	p := h.pricing.LookupPricing(modelID)
	if p == nil {
		return
	}
	cost := pricing.CalculateCost(usage, p)
	// Convert USD → micro-USD for the integer column.
	microusd := int64(cost * repo.MicroUSDPerUSD)
	if microusd <= 0 {
		return
	}
	if err := h.keyRepo.IncrementCostUsed(ctx, keyID, microusd); err != nil {
		log.Printf("[console] quota charge %s: %v", keyID, err)
	}
}

// consolestorePayloadLimit caps how much of a payload we store.
const consolestorePayloadLimit = 5 * 1024 * 1024

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// generateRequestID produces a unique ID (16 hex chars = 8 random bytes).
// The original used 4 bytes (8 hex chars) which has a ~50% collision chance at
// ~65K concurrent requests (birthday paradox); 8 bytes makes collisions
// astronomically unlikely.
func generateRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := [16]byte{}
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
	// Fast path: if the model already matches, skip the expensive full
	// unmarshal+marshal cycle (saves a map allocation for every request).
	var probe struct {
		Model json.RawMessage `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return body
	}
	var current string
	_ = json.Unmarshal(probe.Model, &current)
	if current == model {
		return body
	}
	// Only do the full round-trip when the model actually needs changing.
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

// idleReader wraps an io.Reader with a per-read idle timeout. If no data
// arrives within idleMs between reads, Read returns os.ErrDeadlineExceeded.
// This enforces the gateway's responseIdleTimeoutMs setting on streaming
// responses so a stalled upstream can't hold a request open forever.
// A idleMs <= 0 disables the timeout (passthrough).
//
// Implementation: a single long-lived reader goroutine feeds results into a
// buffered channel. Each Read() call drains one result with a timer. This
// avoids the goroutine-per-read pattern (which leaked goroutines when the
// timer fired before the read completed).
type idleReader struct {
	src    io.Reader
	idleMs int64
	result chan readResult
}

type readResult struct {
	n   int
	err error
	buf []byte
}

func newIdleReader(src io.Reader, idleMs int64) *idleReader {
	r := &idleReader{src: src, idleMs: idleMs, result: make(chan readResult, 1)}
	// Single long-lived reader goroutine. It reads from src sequentially and
	// pushes each result into the buffered channel. When Read() times out and
	// the caller stops draining, this goroutine eventually completes its
	// blocking src.Read() and sends the final result into the buffered channel
	// (buffered=1 so the send never blocks) then exits — no leak.
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := r.src.Read(buf)
			r.result <- readResult{n: n, err: err, buf: buf[:n]}
			if err != nil {
				return
			}
			// Allocate a fresh buffer for the next read so the caller can
			// hold a reference to the returned slice.
			buf = make([]byte, 8192)
		}
	}()
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	if r.idleMs <= 0 {
		// No timeout: just drain the result channel.
		res := <-r.result
		copy(p, res.buf)
		return res.n, res.err
	}
	timer := time.NewTimer(time.Duration(r.idleMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case res := <-r.result:
		copy(p, res.buf)
		return res.n, res.err
	case <-timer.C:
		return 0, os.ErrDeadlineExceeded
	}
}
