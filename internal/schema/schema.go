// Package schema defines the GORM models that map to the database tables.
//
// The column-to-field mapping mirrors the original PostgreSQL schema (and the
// Drizzle schema before it). To support multiple database backends (Postgres,
// MySQL, SQLite) via GORM AutoMigrate, the models keep portable types:
//   - Booleans are integer 0/1 (NOT native BOOLEAN) — call site converts.
//   - Timestamps are epoch-ms (int64).
//   - JSON columns are plain text — marshaled/unmarshaled in Go.
//
// AutoMigrate creates tables/indexes additively; it never drops columns.
package schema

// ConsoleProvider maps the console_providers table.
type ConsoleProvider struct {
	ChannelName       string  `gorm:"column:channel_name;primaryKey;type:varchar(255)"`
	ProviderUUID      string  `gorm:"column:provider_uuid;type:varchar(64);default:''"`
	Type              string  `gorm:"column:type;type:varchar(32)"`
	TargetBaseURL     string  `gorm:"column:target_base_url;type:varchar(512)"`
	SystemPrompt      *string `gorm:"column:system_prompt;type:text"`
	ModelsJSON        string  `gorm:"column:models_json;type:text"`
	Priority          int     `gorm:"column:priority;default:0"`
	AuthHeader        *string `gorm:"column:auth_header;type:varchar(32)"`
	AuthValue         *string `gorm:"column:auth_value;type:varchar(512)"`
	ExtraFieldsJSON   string  `gorm:"column:extra_fields_json;type:text"`
	RoutingVisibility string  `gorm:"column:routing_visibility;type:varchar(32);default:direct"`
	Enabled           int     `gorm:"column:enabled;default:1"`
	CreatedAt         int64   `gorm:"column:created_at;index"`
	UpdatedAt         int64   `gorm:"column:updated_at;index"`
}

// TableName fixes the table name (GORM would otherwise pluralize to
// console_providerss).
func (ConsoleProvider) TableName() string { return "console_providers" }

// ModelAlias maps the model_aliases table.
type ModelAlias struct {
	ID          int     `gorm:"column:id;primaryKey;autoIncrement"`
	Alias       string  `gorm:"column:alias;type:varchar(255);uniqueIndex"`
	Provider    string  `gorm:"column:provider"`
	Model       string  `gorm:"column:model"`
	TargetsJSON string  `gorm:"column:targets_json;type:text"`
	Description *string `gorm:"column:description"`
	Visible     int     `gorm:"column:visible;default:1"`
	Enabled     int     `gorm:"column:enabled;default:1"`
	CreatedAt   int64   `gorm:"column:created_at;index"`
	UpdatedAt   int64   `gorm:"column:updated_at"`
}

// TableName fixes the table name.
func (ModelAlias) TableName() string { return "model_aliases" }

// ConsoleAPIKey maps the console_api_keys table.
type ConsoleAPIKey struct {
	ID                string  `gorm:"column:id;primaryKey;type:varchar(64)"`
	Name              string  `gorm:"column:name;type:varchar(255)"`
	KeyHash           string  `gorm:"column:key_hash;type:varchar(128);index"`
	KeyValue          string  `gorm:"column:key_value;type:varchar(128)"`
	Prefix            string  `gorm:"column:prefix;type:varchar(32)"`
	CreatedAt         int64   `gorm:"column:created_at;index"`
	LastUsedAt        *int64  `gorm:"column:last_used_at"`
	Revoked           int     `gorm:"column:revoked;default:0"`
	AllowedModelsJSON string  `gorm:"column:allowed_models_json;type:text"`
	CostQuotaMicrousd *int64  `gorm:"column:cost_quota_microusd"`
	CostUsedMicrousd  int64   `gorm:"column:cost_used_microusd;default:0"`
}

// TableName fixes the table name.
func (ConsoleAPIKey) TableName() string { return "console_api_keys" }

// ConsoleRequest maps the console_requests table (the observability log). This
// is the widest table (~40 columns); all are listed so AutoMigrate can create
// it and so the detail/list queries can scan into it.
type ConsoleRequest struct {
	RequestID                    string  `gorm:"column:request_id;primaryKey;type:varchar(128)"`
	CreatedAt                    int64   `gorm:"column:created_at;index"`
	RoutePrefix                  string  `gorm:"column:route_prefix;index"`
	UpstreamType                 string  `gorm:"column:upstream_type;default:anthropic"`
	Method                       string  `gorm:"column:method"`
	Path                         string  `gorm:"column:path"`
	TargetURL                    string  `gorm:"column:target_url"`
	RequestModel                 string  `gorm:"column:request_model;index"`
	APIKeyID                     *string `gorm:"column:api_key_id"`
	APIKeyName                   *string `gorm:"column:api_key_name"`
	OriginalPayload              *string `gorm:"column:original_payload"`
	OriginalPayloadTruncated     int     `gorm:"column:original_payload_truncated;default:0"`
	OriginalSummaryJSON          *string `gorm:"column:original_summary_json"`
	ForwardedPayload             *string `gorm:"column:forwarded_payload"`
	ForwardedPayloadTruncated    int     `gorm:"column:forwarded_payload_truncated;default:0"`
	ForwardedSummaryJSON         *string `gorm:"column:forwarded_summary_json"`
	OriginalHeadersJSON          *string `gorm:"column:original_headers_json"`
	ForwardHeadersJSON           *string `gorm:"column:forward_headers_json"`
	ResponseHeadersJSON          *string `gorm:"column:response_headers_json"`
	ResponseStatus               *int    `gorm:"column:response_status;index"`
	ResponseStatusText           *string `gorm:"column:response_status_text"`
	ResponsePayload              *string `gorm:"column:response_payload"`
	ResponsePayloadTruncated     int     `gorm:"column:response_payload_truncated;default:0"`
	ResponsePayloadTruncationReason *string `gorm:"column:response_payload_truncation_reason"`
	ResponseBodyBytes            int     `gorm:"column:response_body_bytes;default:0"`
	FirstChunkAt                 *int64  `gorm:"column:first_chunk_at"`
	FirstTokenAt                 *int64  `gorm:"column:first_token_at"`
	CompletedAt                  *int64  `gorm:"column:completed_at"`
	HasStreamingContent          int     `gorm:"column:has_streaming_content;default:0"`
	ResponseModel                *string `gorm:"column:response_model;index"`
	StopReason                   *string `gorm:"column:stop_reason"`
	InputTokens                  int     `gorm:"column:input_tokens;default:0"`
	OutputTokens                 int     `gorm:"column:output_tokens;default:0"`
	TotalTokens                  int     `gorm:"column:total_tokens;default:0"`
	CacheCreationInputTokens     int     `gorm:"column:cache_creation_input_tokens;default:0"`
	CacheReadInputTokens         int     `gorm:"column:cache_read_input_tokens;default:0"`
	CachedInputTokens            int     `gorm:"column:cached_input_tokens;default:0"`
	ReasoningOutputTokens        int     `gorm:"column:reasoning_output_tokens;default:0"`
	Ephemeral5mInputTokens       int     `gorm:"column:ephemeral_5m_input_tokens;default:0"`
	Ephemeral1hInputTokens       int     `gorm:"column:ephemeral_1h_input_tokens;default:0"`
	FailoverFrom                 *string `gorm:"column:failover_from"`
	FailoverChainJSON            *string `gorm:"column:failover_chain_json"`
	OriginalRoutePrefix          *string `gorm:"column:original_route_prefix"`
	OriginalRequestModel         *string `gorm:"column:original_request_model"`
	FailoverReason               *string `gorm:"column:failover_reason"`
	RetryAttempt                 int     `gorm:"column:retry_attempt;default:0"`
	SourceRequestType            string  `gorm:"column:source_request_type;default:unknown"`
	TokenUsageEstimated          int     `gorm:"column:token_usage_estimated;default:0"`
	QuotaChargedMicrousd         int64   `gorm:"column:quota_charged_microusd;default:0"`
}

// TableName fixes the table name.
func (ConsoleRequest) TableName() string { return "console_requests" }

// ModelMetadataOverride maps the model_metadata_overrides table.
type ModelMetadataOverride struct {
	ID            int      `gorm:"column:id;primaryKey;autoIncrement"`
	ChannelName   string   `gorm:"column:channel_name;type:varchar(255);uniqueIndex:idx_model_metadata_channel_model,priority:1"`
	ModelID       string   `gorm:"column:model_id;type:varchar(255);uniqueIndex:idx_model_metadata_channel_model,priority:2"`
	ContextWindow *int     `gorm:"column:context_window"`
	PricingJSON   *string  `gorm:"column:pricing_json;type:text"`
	CreatedAt     int64    `gorm:"column:created_at"`
	UpdatedAt     int64    `gorm:"column:updated_at"`
}

// TableName fixes the table name.
func (ModelMetadataOverride) TableName() string { return "model_metadata_overrides" }

// GatewaySetting maps the gateway_settings table (key/value JSON store).
type GatewaySetting struct {
	Key       string `gorm:"column:key;primaryKey;type:varchar(128)"`
	ValueJSON string `gorm:"column:value_json;type:text"`
	UpdatedAt int64  `gorm:"column:updated_at"`
}

// TableName fixes the table name.
func (GatewaySetting) TableName() string { return "gateway_settings" }

// Settings keys.
const (
	SettingsKeyTimeouts = "gateway.timeouts"
	SettingsKeyFailover = "gateway.failover"
)

// RequestStats5m is one 5-minute rollup bucket of request metrics, keyed by
// (bucket_start, route_prefix, request_model, api_key_name). It is populated by
// a periodic background job (internal/statsstore) so the Usage/Monitor dashboards
// can SUM these rows instead of scanning console_requests — which means stats
// stay accurate even after old log rows are pruned by the retention cap.
//
// Larger time windows (30m/1h/24h) are produced by grouping multiple 5m rows.
// Cost columns are pre-computed with the catalog at rollup time, so queries
// never need to re-price. Each row also tracks max_request_created_at so the
// rollup cursor (which request rows have been processed) can be derived from
// the table itself — no separate cursor store needed.
type RequestStats5m struct {
	ID               int64   `gorm:"column:id;primaryKey;autoIncrement"`
	BucketStart      int64   `gorm:"column:bucket_start;uniqueIndex:idx_request_stats_5m_dim"`
	RoutePrefix      string  `gorm:"column:route_prefix;uniqueIndex:idx_request_stats_5m_dim"`
	RequestModel     string  `gorm:"column:request_model;uniqueIndex:idx_request_stats_5m_dim"`
	APIKeyName       string  `gorm:"column:api_key_name;uniqueIndex:idx_request_stats_5m_dim"`
	Requests         int64   `gorm:"column:requests"`
	Errors           int64   `gorm:"column:errors"`
	Failovers        int64   `gorm:"column:failovers"`
	CacheHits        int64   `gorm:"column:cache_hits"`
	CacheCreates     int64   `gorm:"column:cache_creates"`
	InputTokens      int64   `gorm:"column:input_tokens"`
	OutputTokens     int64   `gorm:"column:output_tokens"`
	CacheReadTokens  int64   `gorm:"column:cache_read_tokens"`
	CacheCreateTokens int64  `gorm:"column:cache_creation_tokens"`
	CachedInputTokens int64  `gorm:"column:cached_input_tokens"`
	ReasoningTokens  int64   `gorm:"column:reasoning_tokens"`
	InputCostUSD     float64 `gorm:"column:input_cost_usd"`
	OutputCostUSD    float64 `gorm:"column:output_cost_usd"`
	CacheReadCostUSD float64 `gorm:"column:cache_read_cost_usd"`
	CacheWriteCostUSD float64 `gorm:"column:cache_write_cost_usd"`
	// Latency aggregates (so avg cards have data without a separate scan).
	SumDurationMs     int64 `gorm:"column:sum_duration_ms"`
	SumFirstTokenMs   int64 `gorm:"column:sum_first_token_ms"`
	SumGenerationMs   int64 `gorm:"column:sum_generation_ms"`
	CountTimed        int64 `gorm:"column:count_timed"`
	// Cursor: the largest console_requests.created_at folded into this row. The
	// rollup job advances its watermark to MAX(max_request_created_at) so it only
	// re-scans rows seen since the last tick.
	MaxRequestCreatedAt int64 `gorm:"column:max_request_created_at;index"`
	CreatedAt           int64 `gorm:"column:created_at"`
}

// TableName fixes the table name.
func (RequestStats5m) TableName() string { return "request_stats_5m" }

// AllModels returns every model so AutoMigrate can create/migrate them all.
// Note: model_catalog_cache is intentionally absent — the catalog is held in
// memory only (see internal/catalog).
func AllModels() []interface{} {
	return []interface{}{
		&ConsoleProvider{},
		&ModelAlias{},
		&ConsoleAPIKey{},
		&ConsoleRequest{},
		&ModelMetadataOverride{},
		&GatewaySetting{},
		&RequestStats5m{},
	}
}
