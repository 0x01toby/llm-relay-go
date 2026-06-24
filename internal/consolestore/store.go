// Package consolestore implements the request-log storage layer using GORM:
// saving request/response snapshots, listing/paginating them, and computing
// usage stats. Works across Postgres, MySQL, and SQLite (GORM translates
// placeholders and dialect-specific syntax).
package consolestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/providers"
	"github.com/taozhang/llmrelay/internal/schema"
)

// Repository owns the console_requests table.
type Repository struct {
	db         *gorm.DB
	maxRecords int // retention cap (DEBUG_DB_MAX_RECORDS)
}

// New builds a Repository against gdb. maxRecords caps how many rows are
// retained (oldest pruned periodically).
func New(gdb *gorm.DB, maxRecords int) *Repository {
	if maxRecords < 200 {
		maxRecords = 2000
	}
	return &Repository{db: gdb, maxRecords: maxRecords}
}

// RequestSnapshot is the data captured when a request arrives (the INSERT).
type RequestSnapshot struct {
	RequestID            string
	CreatedAt            int64
	RoutePrefix          string
	UpstreamType         string
	Method               string
	Path                 string
	TargetURL            string
	RequestModel         string
	APIKeyID             *string
	APIKeyName           *string
	OriginalPayload      *string
	OriginalTruncated    bool
	OriginalSummaryJSON  *string
	ForwardedPayload     *string
	ForwardedTruncated   bool
	ForwardedSummaryJSON *string
	OriginalHeadersJSON  *string
	ForwardHeadersJSON   *string
	FailoverFrom         *string
	FailoverChainJSON    *string
	OriginalRoutePrefix  *string
	OriginalRequestModel *string
	FailoverReason       *string
	RetryAttempt         int
	SourceRequestType    string
}

// ResponseSnapshot is the data captured when the response completes.
type ResponseSnapshot struct {
	RequestID            string
	ResponseStatus       *int
	ResponseStatusText   *string
	ResponseHeadersJSON  *string
	ResponsePayload      *string
	ResponseTruncated    bool
	ResponseTruncReason  *string
	ResponseBodyBytes    int
	FirstChunkAt         *int64
	FirstTokenAt         *int64
	CompletedAt          *int64
	HasStreamingContent  bool
	ResponseModel        *string
	StopReason           *string
	Usage                providers.UsageData
	QuotaChargedMicrousd int64
}

// SaveRequest upserts a request snapshot. Mirrors saveConsoleRequest.
func (r *Repository) SaveRequest(ctx context.Context, s RequestSnapshot) error {
	if s.UpstreamType == "" {
		s.UpstreamType = "anthropic"
	}
	if s.SourceRequestType == "" {
		s.SourceRequestType = "unknown"
	}
	row := schema.ConsoleRequest{
		RequestID:               s.RequestID,
		CreatedAt:               s.CreatedAt,
		RoutePrefix:             s.RoutePrefix,
		UpstreamType:            s.UpstreamType,
		Method:                  s.Method,
		Path:                    s.Path,
		TargetURL:               s.TargetURL,
		RequestModel:            s.RequestModel,
		APIKeyID:                s.APIKeyID,
		APIKeyName:              s.APIKeyName,
		OriginalPayload:         s.OriginalPayload,
		OriginalPayloadTruncated: boolToInt(s.OriginalTruncated),
		OriginalSummaryJSON:     s.OriginalSummaryJSON,
		ForwardedPayload:        s.ForwardedPayload,
		ForwardedPayloadTruncated: boolToInt(s.ForwardedTruncated),
		ForwardedSummaryJSON:    s.ForwardedSummaryJSON,
		OriginalHeadersJSON:     s.OriginalHeadersJSON,
		ForwardHeadersJSON:      s.ForwardHeadersJSON,
		FailoverFrom:            s.FailoverFrom,
		FailoverChainJSON:       s.FailoverChainJSON,
		OriginalRoutePrefix:     s.OriginalRoutePrefix,
		OriginalRequestModel:    s.OriginalRequestModel,
		FailoverReason:          s.FailoverReason,
		RetryAttempt:            s.RetryAttempt,
		SourceRequestType:       s.SourceRequestType,
	}
	// Upsert on the request_id primary key.
	res := r.db.WithContext(ctx).Create(&row)
	if res.Error != nil {
		// On duplicate primary key, do an update.
		if isDuplicateKey(res.Error) {
			if err := r.db.WithContext(ctx).Model(&schema.ConsoleRequest{}).
				Where("request_id = ?", s.RequestID).
				Updates(map[string]interface{}{
					"route_prefix":            row.RoutePrefix,
					"upstream_type":           row.UpstreamType,
					"method":                  row.Method,
					"path":                    row.Path,
					"target_url":              row.TargetURL,
					"request_model":           row.RequestModel,
					"api_key_id":              row.APIKeyID,
					"api_key_name":            row.APIKeyName,
					"original_payload":        row.OriginalPayload,
					"original_payload_truncated": row.OriginalPayloadTruncated,
					"original_summary_json":   row.OriginalSummaryJSON,
					"forwarded_payload":       row.ForwardedPayload,
					"forwarded_payload_truncated": row.ForwardedPayloadTruncated,
					"forwarded_summary_json":  row.ForwardedSummaryJSON,
					"original_headers_json":   row.OriginalHeadersJSON,
					"forward_headers_json":    row.ForwardHeadersJSON,
					"failover_from":           row.FailoverFrom,
					"failover_chain_json":     row.FailoverChainJSON,
					"original_route_prefix":   row.OriginalRoutePrefix,
					"original_request_model":  row.OriginalRequestModel,
					"failover_reason":         row.FailoverReason,
					"retry_attempt":           row.RetryAttempt,
					"source_request_type":     row.SourceRequestType,
				}).Error; err != nil {
				return fmt.Errorf("save request: %w", err)
			}
		} else {
			return fmt.Errorf("save request: %w", res.Error)
		}
	}
	return nil
}

// SaveResponse updates a request row with the response data. The row must
// already exist (SaveRequest ran first).
func (r *Repository) SaveResponse(ctx context.Context, s ResponseSnapshot) error {
	u := s.Usage
	return r.db.WithContext(ctx).Model(&schema.ConsoleRequest{}).
		Where("request_id = ?", s.RequestID).
		Updates(map[string]interface{}{
			"response_status":                 s.ResponseStatus,
			"response_status_text":            s.ResponseStatusText,
			"response_headers_json":           s.ResponseHeadersJSON,
			"response_payload":                s.ResponsePayload,
			"response_payload_truncated":      boolToInt(s.ResponseTruncated),
			"response_payload_truncation_reason": s.ResponseTruncReason,
			"response_body_bytes":             s.ResponseBodyBytes,
			"first_chunk_at":                  s.FirstChunkAt,
			"first_token_at":                  s.FirstTokenAt,
			"completed_at":                    s.CompletedAt,
			"has_streaming_content":           boolToInt(s.HasStreamingContent),
			"response_model":                  s.ResponseModel,
			"stop_reason":                     s.StopReason,
			"input_tokens":                    u.InputTokens,
			"output_tokens":                   u.OutputTokens,
			"total_tokens":                    u.TotalTokens,
			"cache_creation_input_tokens":     u.CacheCreationInputTokens,
			"cache_read_input_tokens":         u.CacheReadInputTokens,
			"cached_input_tokens":             u.CachedInputTokens,
			"reasoning_output_tokens":         u.ReasoningOutputTokens,
			"ephemeral_5m_input_tokens":       u.Ephemeral5mInputTokens,
			"ephemeral_1h_input_tokens":       u.Ephemeral1hInputTokens,
			"quota_charged_microusd":          s.QuotaChargedMicrousd,
		}).Error
}

// ListFilter controls list pagination and filtering.
type ListFilter struct {
	Limit  int
	Offset int
	Route  string
	Model  string
	Status string // "success" | "error"
	Search string
	// SortBy selects the sort column: "created_at" (default), "response_status",
	// or "tokens". SortOrder is "asc" or "desc" (default "desc").
	SortBy    string
	SortOrder string
}

// List returns a paginated set of request rows plus the total count. It returns
// the raw schema rows so callers (the console API) can assemble the nested
// response_timing/response_usage shape the dashboard expects. Sort order is
// controlled by sortBy/sortOrder (defaults: created_at DESC).
//
// To avoid hauling multi-hundred-KB payload blobs on every page load, the list
// selects only the "summary" columns the dashboard table renders. The heavy
// payload/header blobs (original_payload, forwarded_payload, response_payload,
// *_headers_json) are fetched on demand by Get() when a detail view is opened.
func (r *Repository) List(ctx context.Context, f ListFilter) ([]schema.ConsoleRequest, int, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 1000 {
		f.Limit = 1000
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	q := r.db.WithContext(ctx).Model(&schema.ConsoleRequest{})
	q = applyFilters(q, f)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []schema.ConsoleRequest
	if err := q.Select(listColumns).
		Order(orderClause(f)).Limit(f.Limit).Offset(f.Offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, int(total), nil
}

// listColumns is the projection used by List: every column except the heavy
// payload/header TEXT blobs. Keeping this explicit (rather than SELECT *) means
// a 50-row page load skips ~9MB of forwarded/response payloads that the table
// view never renders. Column names mirror the DB column tags on ConsoleRequest.
var listColumns = []string{
	"request_id", "created_at", "route_prefix", "upstream_type",
	"method", "path", "target_url", "request_model",
	"api_key_id", "api_key_name",
	// truncation flags + reason are lightweight and shown as badges.
	"original_payload_truncated", "forwarded_payload_truncated",
	"response_payload_truncated", "response_payload_truncation_reason",
	// summaries (small JSON) are rendered in the list's forwarded_summary.
	"forwarded_summary_json",
	"response_status", "response_status_text",
	"response_body_bytes", "first_chunk_at", "first_token_at", "completed_at",
	"has_streaming_content", "response_model", "stop_reason",
	"input_tokens", "output_tokens", "total_tokens",
	"cache_creation_input_tokens", "cache_read_input_tokens", "cached_input_tokens",
	"reasoning_output_tokens", "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens",
	"failover_from", "failover_chain_json", "original_route_prefix",
	"original_request_model", "failover_reason", "retry_attempt",
	"source_request_type", "token_usage_estimated", "quota_charged_microusd",
}

// orderClause maps the list filter's sort key/direction to a SQL ORDER BY
// clause. sortBy is constrained to a safe allowlist (never user input injected
// verbatim). tokens sort uses (input_tokens + output_tokens).
func orderClause(f ListFilter) string {
	dir := "DESC"
	if strings.EqualFold(f.SortOrder, "asc") {
		dir = "ASC"
	}
	switch f.SortBy {
	case "response_status":
		return "response_status " + dir + ", created_at DESC"
	case "tokens":
		return "(input_tokens + output_tokens) " + dir + ", created_at DESC"
	default: // "created_at" or anything unknown
		return "created_at " + dir
	}
}

// applyFilters adds the route/model/status/search filters to the query. Uses
// LOWER(col) LIKE LOWER(?) for case-insensitive search (cross-dialect; PG's
// ILIKE is not portable).
func applyFilters(q *gorm.DB, f ListFilter) *gorm.DB {
	if f.Route != "" {
		q = q.Where("route_prefix = ?", f.Route)
	}
	if f.Model != "" {
		q = q.Where("COALESCE(response_model, request_model) = ?", f.Model)
	}
	if f.Status == "success" {
		q = q.Where("response_status >= 200 AND response_status < 300")
	} else if f.Status == "error" {
		q = q.Where("response_status IS NULL OR response_status >= 400")
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		like := "%" + s + "%"
		q = q.Where(
			"(LOWER(request_id) LIKE LOWER(?) OR LOWER(path) LIKE LOWER(?) OR LOWER(route_prefix) LIKE LOWER(?) OR LOWER(request_model) LIKE LOWER(?))",
			like, like, like, like,
		)
	}
	return q
}

// Cleanup prunes old rows beyond the retention cap. It first counts (cheap,
// via the created_at index); only when the cap is exceeded does it delete. The
// delete uses a created_at threshold derived from the Nth-newest row so it can
// lean on the created_at index instead of a correlated NOT-IN subquery.
//
// Cleanup is invoked by a periodic background scheduler (see internal/server),
// not on every SaveRequest, so the request write path stays free of retention
// work. The context lets a graceful shutdown abort a long delete promptly.
func (r *Repository) Cleanup(ctx context.Context) error {
	var total int64
	if err := r.db.WithContext(ctx).Model(&schema.ConsoleRequest{}).Count(&total).Error; err != nil {
		return err
	}
	if total <= int64(r.maxRecords) {
		return nil // nothing to prune
	}

	// Find the created_at of the Nth-newest row (the cutoff). Rows older than it
	// are excess and get deleted. The subquery walks the created_at index.
	var cutoff int64
	if err := r.db.WithContext(ctx).Model(&schema.ConsoleRequest{}).
		Select("created_at").
		Order("created_at DESC").
		Offset(r.maxRecords).
		Limit(1).
		Row().Scan(&cutoff); err != nil {
		return err
	}
	// Delete strictly-older rows (>= cutoff kept, to avoid over-deleting ties).
	res := r.db.WithContext(ctx).
		Where("created_at < ?", cutoff).
		Delete(&schema.ConsoleRequest{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		log.Printf("[console] cleanup pruned %d row(s) beyond cap %d", res.RowsAffected, r.maxRecords)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isDuplicateKey reports whether err indicates a unique/primary-key conflict
// (used to fall back from INSERT to UPDATE in SaveRequest). Covers PG, MySQL,
// and SQLite phrasings.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "unique") && strings.Contains(msg, "constraint")
}

// Get returns the full row for requestID, or false if absent. Used by the
// request-detail endpoint.
func (r *Repository) Get(ctx context.Context, requestID string) (schema.ConsoleRequest, bool, error) {
	var row schema.ConsoleRequest
	err := r.db.WithContext(ctx).Where("request_id = ?", requestID).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return schema.ConsoleRequest{}, false, nil
		}
		return schema.ConsoleRequest{}, false, err
	}
	return row, true, nil
}

// UsageStats holds aggregated usage for the console dashboard.
type UsageStats struct {
	TotalRequests     int64
	TotalErrors       int64
	TotalInputTokens  int64
	TotalOutputTokens int64
	TotalCost         float64
}

// Stats computes aggregate usage over all requests (optionally filtered by
// created_after, in epoch ms). Uses SUM(CASE WHEN ...) instead of PG's
// FILTER(WHERE ...) for portability.
func (r *Repository) Stats(ctx context.Context, createdAfter int64) (UsageStats, error) {
	q := r.db.WithContext(ctx).Model(&schema.ConsoleRequest{}).
		Select(`
			count(*),
			COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0)
		`)
	if createdAfter > 0 {
		q = q.Where("created_at >= ?", createdAfter)
	}
	var s UsageStats
	var errs int64
	row := q.Row()
	if err := row.Scan(&s.TotalRequests, &errs, &s.TotalInputTokens, &s.TotalOutputTokens); err != nil {
		return s, err
	}
	s.TotalErrors = errs
	return s, nil
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
