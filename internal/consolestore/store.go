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
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/providers"
	"github.com/taozhang/llmrelay/internal/schema"
)

// Repository owns the console_requests table.
type Repository struct {
	db          *gorm.DB
	maxRecords  int   // retention cap (DEBUG_DB_MAX_RECORDS)
	lastCleanup int64 // throttles cleanup to once per minute
}

// New builds a Repository against gdb. maxRecords caps how many rows are
// retained (oldest pruned periodically).
func New(gdb *gorm.DB, maxRecords int) *Repository {
	if maxRecords < 200 {
		maxRecords = 50000
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
	return r.maybeCleanup(ctx)
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
	Limit  int
	Offset int
	Route  string
	Model  string
	Status string // "success" | "error"
	Search string
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

	q := r.db.WithContext(ctx).Model(&schema.ConsoleRequest{})
	q = applyFilters(q, f)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []schema.ConsoleRequest
	if err := q.Order("created_at DESC").Limit(f.Limit).Offset(f.Offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}

	out := make([]RequestListItem, 0, len(rows))
	for _, row := range rows {
		item := RequestListItem{
			RequestID:    row.RequestID,
			CreatedAt:    row.CreatedAt,
			RoutePrefix:  row.RoutePrefix,
			UpstreamType: row.UpstreamType,
			Method:       row.Method,
			Path:         row.Path,
			TargetURL:    row.TargetURL,
			RequestModel: row.RequestModel,
			APIKeyName:   row.APIKeyName,
			Status:       row.ResponseStatus,
		}
		if row.InputTokens > 0 || row.OutputTokens > 0 {
			item.Usage = map[string]interface{}{
				"input_tokens": row.InputTokens, "output_tokens": row.OutputTokens, "total_tokens": row.TotalTokens,
			}
		}
		out = append(out, item)
	}
	return out, int(total), nil
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

// maybeCleanup prunes old rows beyond the retention cap, at most once per 60s.
func (r *Repository) maybeCleanup(ctx context.Context) error {
	now := time.Now().UnixMilli()
	if now-r.lastCleanup < 60_000 {
		return nil
	}
	r.lastCleanup = now
	// Delete all but the most recent N rows. Uses a NOT IN subquery against the
	// same table — portable across all three dialects.
	return r.db.WithContext(ctx).Where("request_id NOT IN (?)",
		r.db.WithContext(ctx).Model(&schema.ConsoleRequest{}).
			Select("request_id").
			Order("created_at DESC").
			Limit(r.maxRecords),
	).Delete(&schema.ConsoleRequest{}).Error
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
