// Package schema defines the Go structs that map to the PostgreSQL tables.
//
// The column-to-field mapping mirrors src/db/schema.ts. Booleans are stored as
// integer 0/1 in Postgres (per the original Drizzle schema); the structs use
// the raw int types and callers convert where a real bool is needed. All
// timestamps are epoch-ms (int64).
package schema

// ConsoleProvider maps the console_providers table.
type ConsoleProvider struct {
	ChannelName        string
	ProviderUUID       string
	Type               string
	TargetBaseURL      string
	SystemPrompt       *string
	ModelsJSON         string
	Priority           int
	AuthHeader         *string
	AuthValue          *string
	ExtraFieldsJSON    string
	RoutingVisibility  string
	Enabled            int
	CreatedAt          int64
	UpdatedAt          int64
}

// ModelAlias maps the model_aliases table.
type ModelAlias struct {
	ID          int
	Alias       string
	Provider    string
	Model       string
	TargetsJSON string
	Description *string
	Visible     int
	Enabled     int
	CreatedAt   int64
	UpdatedAt   int64
}

// ConsoleAPIKey maps the console_api_keys table.
type ConsoleAPIKey struct {
	ID                 string
	Name               string
	KeyHash            string
	KeyValue           string
	Prefix             string
	CreatedAt          int64
	LastUsedAt         *int64
	Revoked            int
	AllowedModelsJSON  string
	CostQuotaMicrousd  *int64
	CostUsedMicrousd   int64
}

// ConsoleRequest maps the console_requests table (the observability log). This
// is a wide row (~40 columns); only the fields needed for P2 plumbing are
// listed. The full request-log repository is built in P5.
type ConsoleRequest struct {
	RequestID      string
	CreatedAt      int64
	RoutePrefix    string
	UpstreamType   string
	Method         string
	Path           string
	TargetURL      string
	RequestModel   string
	APIKeyID       *string
	APIKeyName     *string
	UpstreamTypeD  string // unused; kept for column alignment documentation
	ResponseStatus *int
}

// ModelCatalogCache maps the model_catalog_cache table.
type ModelCatalogCache struct {
	ModelID       string
	ContextWindow *int
	PricingJSON   *string
	FetchedAt     int64
}

// ModelMetadataOverride maps the model_metadata_overrides table.
type ModelMetadataOverride struct {
	ID           int
	ChannelName  string
	ModelID      string
	ContextWindow *int
	PricingJSON   *string
	CreatedAt    int64
	UpdatedAt    int64
}

// GatewaySetting maps the gateway_settings table (key/value JSON store).
type GatewaySetting struct {
	Key       string
	ValueJSON string
	UpdatedAt int64
}

// Settings keys.
const (
	SettingsKeyTimeouts = "gateway.timeouts"
	SettingsKeyFailover = "gateway.failover"
)
