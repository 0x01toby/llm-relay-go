package consoleapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/taozhang/llmrelay/internal/pricing"
	"github.com/taozhang/llmrelay/internal/providers"
	"github.com/taozhang/llmrelay/internal/schema"
)

// handleRequestDetailFull assembles the full request-detail response the
// dashboard's DetailView expects: { record, analysis, ... } where record is a
// ConsoleRequestListItem-like object with nested response_timing and
// response_usage.
//
// GET /__console/api/requests/:id
func (a *API) handleRequestDetailFull(w http.ResponseWriter, r *http.Request, id string) {
	row, ok, err := a.requests.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, obj{"error": "failed to load request"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, obj{"error": "request not found"})
		return
	}

	record := buildDetailRecord(row, a.catalog)
	analysis := analyzeRequest(row)

	out := obj{
		"record":              record,
		"analysis":            analysis,
		"source_request_type": row.SourceRequestType,
		"client_label":        clientLabel(row.APIKeyName),
		"api_key_id":          row.APIKeyID,
		"api_key_name":        row.APIKeyName,
	}
	writeJSON(w, http.StatusOK, out)
}

// buildListItem converts a ConsoleRequest row into the nested
// ConsoleRequestListItem shape the dashboard's request log table reads. It
// reuses the same assembly helpers as the detail record (buildUsage,
// deltaMs, analyzeRequest) so the list and detail views stay consistent, but
// omits the heavy payload/header blobs the list does not need.
//
// This is where the list endpoint's latency/token fields are produced:
//   - response_timing.first_token_latency_ms / duration_ms (耗时)
//   - response_usage.input_tokens / output_tokens (token数)
func buildListItem(row *schema.ConsoleRequest, cat catalogLooker) obj {
	timing := obj{
		"first_chunk_latency_ms": deltaMs(row.CreatedAt, row.FirstChunkAt),
		"first_token_latency_ms": deltaMs(row.CreatedAt, row.FirstTokenAt),
		"duration_ms":            deltaMs(row.CreatedAt, row.CompletedAt),
		"generation_duration_ms": generationDurationMs(row.FirstTokenAt, row.CompletedAt),
		"response_body_bytes":    row.ResponseBodyBytes,
		"has_streaming_content":  row.HasStreamingContent != 0,
	}

	var failoverChain []string
	if row.FailoverChainJSON != nil && *row.FailoverChainJSON != "" {
		_ = json.Unmarshal([]byte(*row.FailoverChainJSON), &failoverChain)
	}
	if failoverChain == nil {
		failoverChain = []string{}
	}

	return obj{
		"request_id":                         row.RequestID,
		"created_at":                         row.CreatedAt,
		"route_prefix":                       row.RoutePrefix,
		"upstream_type":                      row.UpstreamType,
		"source_request_type":                row.SourceRequestType,
		"client_label":                       clientLabel(row.APIKeyName),
		"api_key_id":                         row.APIKeyID,
		"api_key_name":                       row.APIKeyName,
		"path":                               row.Path,
		"target_url":                         row.TargetURL,
		"request_model":                      row.RequestModel,
		"response_status":                    row.ResponseStatus,
		"response_status_text":               strOrDefault(row.ResponseStatusText, ""),
		"response_timing":                    timing,
		"response_usage":                     buildUsage(*row, cat),
		"response_payload_truncated":         row.ResponsePayloadTruncated != 0,
		"response_payload_truncation_reason": row.ResponsePayloadTruncationReason,
		"forwarded_summary":                  decodeJSONObj(row.ForwardedSummaryJSON),
		"analysis":                           analyzeRequest(*row),
		"failover_from":                      row.FailoverFrom,
		"failover_chain":                     failoverChain,
		"original_route_prefix":              row.OriginalRoutePrefix,
		"original_request_model":             row.OriginalRequestModel,
		"failover_reason":                    row.FailoverReason,
		"retry_attempt":                      row.RetryAttempt,
	}
}

// buildDetailRecord converts a ConsoleRequest row into the nested record shape
// DetailView reads. It assembles response_timing (latencies) and response_usage
// (tokens) from the flat DB columns, and decodes the *_headers_json /
// *_summary_json blobs.
func buildDetailRecord(row schema.ConsoleRequest, cat catalogLooker) obj {
	timing := obj{
		"first_chunk_latency_ms": deltaMs(row.CreatedAt, row.FirstChunkAt),
		"first_token_latency_ms": deltaMs(row.CreatedAt, row.FirstTokenAt),
		"duration_ms":            deltaMs(row.CreatedAt, row.CompletedAt),
		"generation_duration_ms": generationDurationMs(row.FirstTokenAt, row.CompletedAt),
		"response_body_bytes":    row.ResponseBodyBytes,
		"has_streaming_content":  row.HasStreamingContent != 0,
	}

	usage := buildUsage(row, cat)

	var failoverChain []string
	if row.FailoverChainJSON != nil && *row.FailoverChainJSON != "" {
		_ = json.Unmarshal([]byte(*row.FailoverChainJSON), &failoverChain)
	}
	if failoverChain == nil {
		failoverChain = []string{}
	}

	return obj{
		"request_id":                         row.RequestID,
		"created_at":                         row.CreatedAt,
		"route_prefix":                       row.RoutePrefix,
		"upstream_type":                      row.UpstreamType,
		"source_request_type":                row.SourceRequestType,
		"client_label":                       clientLabel(row.APIKeyName),
		"api_key_id":                         row.APIKeyID,
		"api_key_name":                       row.APIKeyName,
		"path":                               row.Path,
		"target_url":                         row.TargetURL,
		"request_model":                      row.RequestModel,
		"response_status":                    row.ResponseStatus,
		"response_status_text":               strOrDefault(row.ResponseStatusText, ""),
		"response_timing":                    timing,
		"response_usage":                     usage,
		"response_payload":                   strOrDefault(row.ResponsePayload, ""),
		"response_payload_truncated":         row.ResponsePayloadTruncated != 0,
		"response_payload_truncation_reason": row.ResponsePayloadTruncationReason,
		"original_payload":                   strOrDefault(row.OriginalPayload, ""),
		"original_payload_truncated":         row.OriginalPayloadTruncated != 0,
		"forwarded_payload":                  strOrDefault(row.ForwardedPayload, ""),
		"forwarded_payload_truncated":        row.ForwardedPayloadTruncated != 0,
		"original_headers":                   decodeJSONObj(row.OriginalHeadersJSON),
		"forward_headers":                    decodeJSONObj(row.ForwardHeadersJSON),
		"response_headers":                   decodeJSONObj(row.ResponseHeadersJSON),
		"forwarded_summary":                  decodeJSONObj(row.ForwardedSummaryJSON),
		"analysis":                           analyzeRequest(row),
		"failover_from":                      row.FailoverFrom,
		"failover_chain":                     failoverChain,
		"original_route_prefix":              row.OriginalRoutePrefix,
		"original_request_model":             row.OriginalRequestModel,
		"failover_reason":                    row.FailoverReason,
		"retry_attempt":                      row.RetryAttempt,
	}
}

// buildUsage assembles the response_usage object from the token columns. When
// cat is non-nil it also computes the USD cost from the per-model pricing in the
// catalog (input/output/cache_read/cache_write) and attaches it as "cost" and a
// "cost_breakdown". Pass nil to skip cost (e.g. when pricing is unavailable).
func buildUsage(row schema.ConsoleRequest, cat catalogLooker) obj {
	cachedInput := row.CachedInputTokens
	u := obj{
		"input_tokens":                row.InputTokens,
		"output_tokens":               row.OutputTokens,
		"total_tokens":                row.TotalTokens,
		"cache_creation_input_tokens": row.CacheCreationInputTokens,
		"cache_read_input_tokens":     row.CacheReadInputTokens,
		"cached_input_tokens":         row.CachedInputTokens,
		"reasoning_output_tokens":     row.ReasoningOutputTokens,
		"uncached_input_tokens":       row.InputTokens - cachedInput,
		"total_input_tokens":          row.InputTokens,
		"total_output_tokens":         row.OutputTokens,
		"total_cache_creation_tokens": row.CacheCreationInputTokens,
		"total_cache_read_tokens":     row.CacheReadInputTokens,
		"estimated":                   row.TokenUsageEstimated != 0,
	}
	if row.ResponseModel != nil {
		u["model"] = *row.ResponseModel
	}
	if row.StopReason != nil {
		u["stop_reason"] = *row.StopReason
	}
	if cat != nil {
		modelID := row.RequestModel
		if row.ResponseModel != nil && *row.ResponseModel != "" {
			modelID = *row.ResponseModel
		}
		if cost, breakdown, ok := computeCost(row, cat, modelID); ok {
			u["cost"] = cost
			u["cost_breakdown"] = breakdown
			u["cost_pricing"] = pricingSource(modelID, cat.LookupPricing(modelID))
		}
	}
	return u
}

// computeCost derives the USD cost for a request row using the catalog's
// per-model pricing. Returns ok=false when the model has no pricing (so the
// caller omits the cost fields rather than showing a misleading 0).
func computeCost(row schema.ConsoleRequest, cat catalogLooker, modelID string) (cost float64, breakdown obj, ok bool) {
	p := cat.LookupPricing(modelID)
	if p == nil {
		return 0, nil, false
	}
	usage := providers.UsageData{
		InputTokens:              int64(row.InputTokens),
		OutputTokens:             int64(row.OutputTokens),
		CachedInputTokens:        int64(row.CachedInputTokens),
		CacheReadInputTokens:     int64(row.CacheReadInputTokens),
		CacheCreationInputTokens: int64(row.CacheCreationInputTokens),
	}
	buckets := pricing.BucketsForUsage(usage)
	var inputCost, outputCost, cacheReadCost, cacheWriteCost float64
	if p.Input != nil {
		inputCost = float64(buckets.Input) / pricing.TokensPerMillion * *p.Input
	}
	if p.Output != nil {
		outputCost = float64(buckets.Output) / pricing.TokensPerMillion * *p.Output
	}
	if p.CacheRead != nil && buckets.CacheRead > 0 {
		cacheReadCost = float64(buckets.CacheRead) / pricing.TokensPerMillion * *p.CacheRead
	}
	if p.CacheWrite != nil && buckets.CacheWrite > 0 {
		cacheWriteCost = float64(buckets.CacheWrite) / pricing.TokensPerMillion * *p.CacheWrite
	}
	total := inputCost + outputCost + cacheReadCost + cacheWriteCost
	return total, obj{
		"upstream_type":         row.UpstreamType,
		"uncached_input_tokens": buckets.Input,
		"cache_read_tokens":     buckets.CacheRead,
		"cache_write_tokens":    buckets.CacheWrite,
		"input_cost":            roundUSD(inputCost),
		"output_cost":           roundUSD(outputCost),
		"cache_read_cost":       roundUSD(cacheReadCost),
		"cache_write_cost":      roundUSD(cacheWriteCost),
		"total_cost":            roundUSD(total),
	}, true
}

// roundUSD rounds to 6 decimal places (sub-cent precision for display).
func roundUSD(v float64) float64 {
	return float64(int64(v*1e6+0.5)) / 1e6
}

// pricingSource renders the ModelPricing used for the cost calc (the dashboard
// shows this as cost_pricing). Returns nil if pricing is absent.
func pricingSource(modelID string, p *pricing.ModelPricing) obj {
	if p == nil {
		return nil
	}
	out := obj{"model": modelID}
	if p.Input != nil {
		out["input"] = *p.Input
	}
	if p.Output != nil {
		out["output"] = *p.Output
	}
	if p.CacheRead != nil {
		out["cache_read"] = *p.CacheRead
	}
	if p.CacheWrite != nil {
		out["cache_write"] = *p.CacheWrite
	}
	return out
}

// analyzeRequest builds the ConsoleAnalysis { cache_state, summary } shown in
// the detail header badge.
func analyzeRequest(row schema.ConsoleRequest) obj {
	cacheState := "none"
	switch {
	case row.CacheReadInputTokens > 0:
		cacheState = "hit"
	case row.CacheCreationInputTokens > 0:
		cacheState = "write"
	}

	var sb strings.Builder
	sb.WriteString(row.Method)
	sb.WriteString(" ")
	sb.WriteString(row.Path)
	if row.RequestModel != "" {
		sb.WriteString(" · ")
		sb.WriteString(row.RequestModel)
	}
	if row.ResponseStatus != nil {
		sb.WriteString(" · ")
		sb.WriteString(itoa(*row.ResponseStatus))
	}

	return obj{
		"cache_state": cacheState,
		"summary":     sb.String(),
	}
}

// deltaMs returns the ms difference between start and end (nil-aware).
func deltaMs(start int64, end *int64) *int64 {
	if end == nil {
		return nil
	}
	d := *end - start
	return &d
}

// generationDurationMs is the time between first token and completion.
func generationDurationMs(firstToken, completedAt *int64) *int64 {
	if firstToken == nil || completedAt == nil {
		return nil
	}
	d := *completedAt - *firstToken
	return &d
}

func clientLabel(name *string) string {
	if name != nil && *name != "" {
		return *name
	}
	return "generic"
}

func strOrDefault(s *string, def string) string {
	if s != nil {
		return *s
	}
	return def
}

// decodeJSONObj parses a JSON-object blob into a map; returns {} on any failure
// so the dashboard's `record.original_headers ?? {}` always has an object.
func decodeJSONObj(raw *string) obj {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return obj{}
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		return obj{}
	}
	if m == nil {
		return obj{}
	}
	return m
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
