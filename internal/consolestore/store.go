// Package consolestore implements the request-log storage layer: saving request
// and response snapshots, listing/paginating them, and computing usage stats.
// It is the Go port of src/console-store.ts.
//
// The console_requests table is wide (~40 columns). This package provides
// typed RequestSnapshot/ResponseSnapshot inputs and a Repository that writes
// them. Full-text payload storage, truncation flags, token buckets, timing,
// and failover metadata are all preserved.
package consolestore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/internal/providers"
)

// Repository owns the console_requests table.
type Repository struct {
	pool          *pgxpool.Pool
	maxRecords    int   // retention cap (DEBUG_DB_MAX_RECORDS)
	lastCleanupAt int64 // throttles cleanup to once per minute
}

// New builds a Repository against pool. maxRecords caps how many rows are
// retained (oldest pruned periodically).
func New(pool *pgxpool.Pool, maxRecords int) *Repository {
	if maxRecords < 200 {
		maxRecords = 50000
	}
	return &Repository{pool: pool, maxRecords: maxRecords}
}

// RequestSnapshot is the data captured when a request arrives (the INSERT).
type RequestSnapshot struct {
	RequestID             string
	CreatedAt             int64
	RoutePrefix           string
	UpstreamType          string
	Method                string
	Path                  string
	TargetURL             string
	RequestModel          string
	APIKeyID              *string
	APIKeyName            *string
	OriginalPayload       *string
	OriginalTruncated     bool
	OriginalSummaryJSON   *string
	ForwardedPayload      *string
	ForwardedTruncated    bool
	ForwardedSummaryJSON  *string
	OriginalHeadersJSON   *string
	ForwardHeadersJSON    *string
	FailoverFrom          *string
	FailoverChainJSON     *string
	OriginalRoutePrefix   *string
	OriginalRequestModel  *string
	FailoverReason        *string
	RetryAttempt          int
	SourceRequestType     string
}

// ResponseSnapshot is the data captured when the response completes (the UPDATE).
type ResponseSnapshot struct {
	RequestID              string
	ResponseStatus         *int
	ResponseStatusText     *string
	ResponseHeadersJSON    *string
	ResponsePayload        *string
	ResponseTruncated      bool
	ResponseTruncReason    *string
	ResponseBodyBytes      int
	FirstChunkAt           *int64
	FirstTokenAt           *int64
	CompletedAt            *int64
	HasStreamingContent    bool
	ResponseModel          *string
	StopReason             *string
	Usage                  providers.UsageData
	QuotaChargedMicrousd   int64
}

// SaveRequest upserts a request snapshot (INSERT … ON CONFLICT DO UPDATE so a
// retry reusing a requestId overwrites cleanly). Mirrors saveConsoleRequest.
func (r *Repository) SaveRequest(ctx context.Context, s RequestSnapshot) error {
	if s.UpstreamType == "" {
		s.UpstreamType = "anthropic"
	}
	if s.SourceRequestType == "" {
		s.SourceRequestType = "unknown"
	}
	origTrunc := 0
	if s.OriginalTruncated {
		origTrunc = 1
	}
	fwdTrunc := 0
	if s.ForwardedTruncated {
		fwdTrunc = 1
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO console_requests
		  (request_id, created_at, route_prefix, upstream_type, method, path, target_url,
		   request_model, api_key_id, api_key_name,
		   original_payload, original_payload_truncated, original_summary_json,
		   forwarded_payload, forwarded_payload_truncated, forwarded_summary_json,
		   original_headers_json, forward_headers_json,
		   failover_from, failover_chain_json, original_route_prefix, original_request_model,
		   failover_reason, retry_attempt, source_request_type)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		ON CONFLICT (request_id) DO UPDATE SET
		  route_prefix = EXCLUDED.route_prefix,
		  upstream_type = EXCLUDED.upstream_type,
		  method = EXCLUDED.method,
		  path = EXCLUDED.path,
		  target_url = EXCLUDED.target_url,
		  request_model = EXCLUDED.request_model,
		  api_key_id = EXCLUDED.api_key_id,
		  api_key_name = EXCLUDED.api_key_name,
		  original_payload = EXCLUDED.original_payload,
		  original_payload_truncated = EXCLUDED.original_payload_truncated,
		  original_summary_json = EXCLUDED.original_summary_json,
		  forwarded_payload = EXCLUDED.forwarded_payload,
		  forwarded_payload_truncated = EXCLUDED.forwarded_payload_truncated,
		  forwarded_summary_json = EXCLUDED.forwarded_summary_json,
		  original_headers_json = EXCLUDED.original_headers_json,
		  forward_headers_json = EXCLUDED.forward_headers_json,
		  failover_from = EXCLUDED.failover_from,
		  failover_chain_json = EXCLUDED.failover_chain_json,
		  original_route_prefix = EXCLUDED.original_route_prefix,
		  original_request_model = EXCLUDED.original_request_model,
		  failover_reason = EXCLUDED.failover_reason,
		  retry_attempt = EXCLUDED.retry_attempt,
		  source_request_type = EXCLUDED.source_request_type
	`,
		s.RequestID, s.CreatedAt, s.RoutePrefix, s.UpstreamType, s.Method, s.Path, s.TargetURL,
		s.RequestModel, s.APIKeyID, s.APIKeyName,
		s.OriginalPayload, origTrunc, s.OriginalSummaryJSON,
		s.ForwardedPayload, fwdTrunc, s.ForwardedSummaryJSON,
		s.OriginalHeadersJSON, s.ForwardHeadersJSON,
		s.FailoverFrom, s.FailoverChainJSON, s.OriginalRoutePrefix, s.OriginalRequestModel,
		s.FailoverReason, s.RetryAttempt, s.SourceRequestType,
	)
	if err != nil {
		return fmt.Errorf("save request: %w", err)
	}
	return r.maybeCleanup(ctx)
}

// SaveResponse updates a request row with the response data. Mirrors
// saveConsoleResponse. The row must already exist (SaveRequest ran first).
func (r *Repository) SaveResponse(ctx context.Context, s ResponseSnapshot) error {
	respTrunc := 0
	if s.ResponseTruncated {
		respTrunc = 1
	}
	hasStream := 0
	if s.HasStreamingContent {
		hasStream = 1
	}
	u := s.Usage
	_, err := r.pool.Exec(ctx, `
		UPDATE console_requests SET
		  response_status = $2,
		  response_status_text = $3,
		  response_headers_json = $4,
		  response_payload = $5,
		  response_payload_truncated = $6,
		  response_payload_truncation_reason = $7,
		  response_body_bytes = $8,
		  first_chunk_at = $9,
		  first_token_at = $10,
		  completed_at = $11,
		  has_streaming_content = $12,
		  response_model = $13,
		  stop_reason = $14,
		  input_tokens = $15,
		  output_tokens = $16,
		  total_tokens = $17,
		  cache_creation_input_tokens = $18,
		  cache_read_input_tokens = $19,
		  cached_input_tokens = $20,
		  reasoning_output_tokens = $21,
		  ephemeral_5m_input_tokens = $22,
		  ephemeral_1h_input_tokens = $23,
		  quota_charged_microusd = $24
		WHERE request_id = $1
	`,
		s.RequestID, s.ResponseStatus, s.ResponseStatusText, s.ResponseHeadersJSON,
		s.ResponsePayload, respTrunc, s.ResponseTruncReason, s.ResponseBodyBytes,
		s.FirstChunkAt, s.FirstTokenAt, s.CompletedAt, hasStream,
		s.ResponseModel, s.StopReason,
		u.InputTokens, u.OutputTokens, u.TotalTokens,
		u.CacheCreationInputTokens, u.CacheReadInputTokens, u.CachedInputTokens,
		u.ReasoningOutputTokens, u.Ephemeral5mInputTokens, u.Ephemeral1hInputTokens,
		s.QuotaChargedMicrousd,
	)
	return err
}

// DetailRow is the full console_requests row, used by the request-detail
// endpoint. It carries every column so the API layer can assemble the nested
// response_timing / response_usage objects the dashboard expects.
type DetailRow struct {
	RequestID             string
	CreatedAt             int64
	RoutePrefix           string
	UpstreamType          string
	Method                string
	Path                  string
	TargetURL             string
	RequestModel          string
	APIKeyID              *string
	APIKeyName            *string
	OriginalPayload       *string
	OriginalTruncated     bool
	OriginalSummaryJSON   *string
	ForwardedPayload      *string
	ForwardedTruncated    bool
	ForwardedSummaryJSON  *string
	OriginalHeadersJSON   *string
	ForwardHeadersJSON    *string
	ResponseHeadersJSON   *string
	ResponseStatus        *int
	ResponseStatusText    *string
	ResponsePayload       *string
	ResponseTruncated     bool
	ResponseTruncReason   *string
	ResponseBodyBytes     int
	FirstChunkAt          *int64
	FirstTokenAt          *int64
	CompletedAt           *int64
	HasStreamingContent   bool
	ResponseModel         *string
	StopReason            *string
	InputTokens           int64
	OutputTokens          int64
	TotalTokens           int64
	CacheCreationTokens   int64
	CacheReadTokens       int64
	CachedInputTokens     int64
	ReasoningOutputTokens int64
	FailoverFrom          *string
	FailoverChainJSON     *string
	OriginalRoutePrefix   *string
	OriginalRequestModel  *string
	FailoverReason        *string
	RetryAttempt          int
	SourceRequestType     string
	UsageEstimated        bool
}

// Get returns the full row for requestID, or false if absent.
func (r *Repository) Get(ctx context.Context, requestID string) (DetailRow, bool, error) {
	var d DetailRow
	var origTrunc, fwdTrunc, respTrunc, hasStream, usageEst int
	err := r.pool.QueryRow(ctx, `
		SELECT request_id, created_at, route_prefix, upstream_type, method, path,
		       target_url, request_model, api_key_id, api_key_name,
		       original_payload, original_payload_truncated, original_summary_json,
		       forwarded_payload, forwarded_payload_truncated, forwarded_summary_json,
		       original_headers_json, forward_headers_json, response_headers_json,
		       response_status, response_status_text,
		       response_payload, response_payload_truncated, response_payload_truncation_reason,
		       response_body_bytes, first_chunk_at, first_token_at, completed_at,
		       has_streaming_content, response_model, stop_reason,
		       input_tokens, output_tokens, total_tokens,
		       cache_creation_input_tokens, cache_read_input_tokens, cached_input_tokens,
		       reasoning_output_tokens,
		       failover_from, failover_chain_json, original_route_prefix, original_request_model,
		       failover_reason, retry_attempt, source_request_type, token_usage_estimated
		FROM console_requests
		WHERE request_id = $1
	`, requestID).Scan(
		&d.RequestID, &d.CreatedAt, &d.RoutePrefix, &d.UpstreamType, &d.Method, &d.Path,
		&d.TargetURL, &d.RequestModel, &d.APIKeyID, &d.APIKeyName,
		&d.OriginalPayload, &origTrunc, &d.OriginalSummaryJSON,
		&d.ForwardedPayload, &fwdTrunc, &d.ForwardedSummaryJSON,
		&d.OriginalHeadersJSON, &d.ForwardHeadersJSON, &d.ResponseHeadersJSON,
		&d.ResponseStatus, &d.ResponseStatusText,
		&d.ResponsePayload, &respTrunc, &d.ResponseTruncReason,
		&d.ResponseBodyBytes, &d.FirstChunkAt, &d.FirstTokenAt, &d.CompletedAt,
		&hasStream, &d.ResponseModel, &d.StopReason,
		&d.InputTokens, &d.OutputTokens, &d.TotalTokens,
		&d.CacheCreationTokens, &d.CacheReadTokens, &d.CachedInputTokens,
		&d.ReasoningOutputTokens,
		&d.FailoverFrom, &d.FailoverChainJSON, &d.OriginalRoutePrefix, &d.OriginalRequestModel,
		&d.FailoverReason, &d.RetryAttempt, &d.SourceRequestType, &usageEst,
	)
	if err != nil {
		if isPgNoRows(err) {
			return DetailRow{}, false, nil
		}
		return DetailRow{}, false, err
	}
	d.OriginalTruncated = origTrunc != 0
	d.ForwardedTruncated = fwdTrunc != 0
	d.ResponseTruncated = respTrunc != 0
	d.HasStreamingContent = hasStream != 0
	d.UsageEstimated = usageEst != 0
	return d, true, nil
}

// isPgNoRows reports whether err is a pgx "no rows" error. Defined here to keep
// the package self-contained (the migrate/repo packages have their own copies).
func isPgNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows")
}

// RequestListItem is one row in the console request list.
type RequestListItem struct {
	RequestID    string                 `json:"request_id"`
	CreatedAt    int64                  `json:"created_at"`
	RoutePrefix  string                 `json:"route_prefix"`
	UpstreamType string                 `json:"upstream_type"`
	Method       string                 `json:"method"`
	Path         string                 `json:"path"`
	TargetURL    string                 `json:"target_url"`
	RequestModel string                 `json:"request_model"`
	APIKeyName   *string                `json:"api_key_name"`
	Status       *int                   `json:"response_status"`
	Usage        map[string]interface{} `json:"usage,omitempty"`
}

// ListFilter controls list pagination and filtering.
type ListFilter struct {
	Limit   int
	Offset  int
	Route   string
	Model   string
	Status  string // "success" | "error"
	Search  string
}

// List returns a paginated request list plus the total count.
func (r *Repository) List(ctx context.Context, f ListFilter) ([]RequestListItem, int, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	where, args := buildWhere(f)
	argN := len(args)

	countQ := "SELECT count(*) FROM console_requests"
	if where != "" {
		countQ += " WHERE " + where
	}
	var total int
	if err := r.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listQ := `
		SELECT request_id, created_at, route_prefix, upstream_type, method, path,
		       target_url, request_model, api_key_name, response_status,
		       input_tokens, output_tokens, total_tokens
		FROM console_requests
	`
	if where != "" {
		listQ += " WHERE " + where
	}
	listQ += " ORDER BY created_at DESC LIMIT $" + itoa(argN+1) + " OFFSET $" + itoa(argN+2)
	args = append(args, f.Limit, f.Offset)

	rows, err := r.pool.Query(ctx, listQ, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []RequestListItem
	for rows.Next() {
		var item RequestListItem
		var inTok, outTok, totTok int64
		if err := rows.Scan(&item.RequestID, &item.CreatedAt, &item.RoutePrefix, &item.UpstreamType,
			&item.Method, &item.Path, &item.TargetURL, &item.RequestModel, &item.APIKeyName,
			&item.Status, &inTok, &outTok, &totTok); err != nil {
			return nil, 0, err
		}
		if inTok > 0 || outTok > 0 {
			item.Usage = map[string]interface{}{
				"input_tokens": inTok, "output_tokens": outTok, "total_tokens": totTok,
			}
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// buildWhere constructs a WHERE clause + args from the filter. Mirrors a subset
// of buildRequestWhere (route/model/status/search).
func buildWhere(f ListFilter) (string, []interface{}) {
	var conds []string
	var args []interface{}
	n := 0
	add := func(cond string, val interface{}) {
		n++
		conds = append(conds, fmt.Sprintf(cond, "$"+itoa(n)))
		args = append(args, val)
	}
	if f.Route != "" {
		add("route_prefix = %s", f.Route)
	}
	if f.Model != "" {
		add("COALESCE(response_model, request_model) = %s", f.Model)
	}
	if f.Status == "success" {
		conds = append(conds, "response_status >= 200 AND response_status < 300")
	} else if f.Status == "error" {
		conds = append(conds, "(response_status IS NULL OR response_status >= 400)")
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		// Four columns share the same search term; bind it four times.
		n++
		ph := "$" + itoa(n)
		conds = append(conds, fmt.Sprintf(
			"(request_id ILIKE '%%' || %s || '%%' OR path ILIKE '%%' || %s || '%%' OR route_prefix ILIKE '%%' || %s || '%%' OR request_model ILIKE '%%' || %s || '%%')",
			ph, ph, ph, ph))
		args = append(args, s)
	}
	return strings.Join(conds, " AND "), args
}

// maybeCleanup prunes old rows beyond the retention cap, at most once per 60s.
// Mirrors cleanupOldRows.
func (r *Repository) maybeCleanup(ctx context.Context) error {
	now := time.Now().UnixMilli()
	if now-r.lastCleanupAt < 60_000 {
		return nil
	}
	r.lastCleanupAt = now
	_, err := r.pool.Exec(ctx, `
		DELETE FROM console_requests
		WHERE request_id NOT IN (
		  SELECT request_id FROM console_requests ORDER BY created_at DESC LIMIT $1
		)
	`, r.maxRecords)
	return err
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// UsageStats holds aggregated usage for the console dashboard.
type UsageStats struct {
	TotalRequests int64
	TotalErrors   int64
	TotalInputTokens  int64
	TotalOutputTokens int64
	TotalCost     float64
}

// Stats computes aggregate usage over all requests (optionally filtered by
// created_after, in epoch ms). A simplified port of buildUsageStats.
func (r *Repository) Stats(ctx context.Context, createdAfter int64) (UsageStats, error) {
	q := `
		SELECT count(*),
		       count(*) FILTER (WHERE response_status IS NULL OR response_status >= 400),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0)
		FROM console_requests
	`
	args := []interface{}{}
	if createdAfter > 0 {
		q += " WHERE created_at >= $1"
		args = append(args, createdAfter)
	}
	var s UsageStats
	err := r.pool.QueryRow(ctx, q, args...).Scan(
		&s.TotalRequests, &s.TotalErrors, &s.TotalInputTokens, &s.TotalOutputTokens,
	)
	return s, err
}

// MarshalSummary serializes a summary map to JSON text for storage.
func MarshalSummary(m map[string]interface{}) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
