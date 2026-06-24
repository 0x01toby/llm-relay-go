package consoleapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/taozhang/llmrelay/internal/pricing"
	"github.com/taozhang/llmrelay/internal/schema"
)

// statsOverview holds the aggregate counts computed by computeOverview.
//
// Cache fields are split into "request counts" (how many requests hit/created
// cache) and "token totals" (how many tokens were cache-read/written). The two
// were previously conflated — cache_hits held SUM(cache_read_input_tokens),
// i.e. a token count — which made hit_rate = hits/total explode to thousands
// and cache_misses go negative. The dashboard shows request counts in the
// hit-rate card and token totals in the cache-read/write cards.
type statsOverview struct {
	Total        int64 // total requests
	CacheHits    int64 // requests with cache_read_input_tokens > 0
	CacheCreates int64 // requests with cache_creation_input_tokens > 0
	CacheMisses  int64 // requests with neither (Total - CacheHits - CacheCreates)
	Errors       int64 // requests with status null or >= 400
	Failovers    int64 // requests with a failover_from set
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	// Token totals surfaced in the dashboard's cache cards.
	CacheReadTokens      int64 // SUM(cache_read_input_tokens)
	CacheCreationTokens  int64 // SUM(cache_creation_input_tokens)
	CachedInputTokens    int64 // SUM(cached_input_tokens)        (OpenAI)
	ReasoningTokens      int64 // SUM(reasoning_output_tokens)
}

// computeOverview runs the dashboard's overview aggregate over console_requests
// (optionally filtered by created_after epoch-ms). Uses SUM(CASE WHEN ...)
// instead of PG's FILTER(WHERE) for portability.
func computeOverview(ctx context.Context, gdb *gorm.DB, createdAfter int64) (statsOverview, error) {
	q := gdb.WithContext(ctx).Model(&schema.ConsoleRequest{}).Select(`
		COUNT(*),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(CASE WHEN cache_read_input_tokens > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN cache_creation_input_tokens > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN failover_from IS NOT NULL AND failover_from != '' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(cache_read_input_tokens), 0),
		COALESCE(SUM(cache_creation_input_tokens), 0),
		COALESCE(SUM(cached_input_tokens), 0),
		COALESCE(SUM(reasoning_output_tokens), 0)
	`)
	if createdAfter > 0 {
		q = q.Where("created_at >= ?", createdAfter)
	}
	var total, inTok, outTok, cacheHitReqs, cacheCreateReqs, errors, failovers int64
	var cacheReadTok, cacheCreateTok, cachedInputTok, reasoningTok int64
	row := q.Row()
	if err := row.Scan(&total, &inTok, &outTok, &cacheHitReqs, &cacheCreateReqs, &errors, &failovers,
		&cacheReadTok, &cacheCreateTok, &cachedInputTok, &reasoningTok); err != nil {
		return statsOverview{}, err
	}
	misses := total - cacheHitReqs - cacheCreateReqs
	if misses < 0 {
		misses = 0
	}
	return statsOverview{
		Total: total, InputTokens: inTok, OutputTokens: outTok,
		TotalTokens: inTok + outTok,
		CacheHits: cacheHitReqs, CacheCreates: cacheCreateReqs, CacheMisses: misses,
		Errors: errors, Failovers: failovers,
		CacheReadTokens:     cacheReadTok,
		CacheCreationTokens: cacheCreateTok,
		CachedInputTokens:   cachedInputTok,
		ReasoningTokens:     reasoningTok,
	}, nil
}

// statsBucket is one row of the per-route/per-model/per-client grouping.
type statsBucket struct {
	Key          string
	Requests     int64
	InputTokens  int64
	OutputTokens int64
	CacheHits    int64
	Errors       int64
}

// computeBuckets groups console_requests by groupCol and aggregates. groupCol
// is one of route_prefix / request_model / api_key_name (controlled by the
// caller; never user input). CacheHits is a request count (requests with
// cache_read_input_tokens > 0) so the dashboard's per-bucket hit rate stays in
// [0, 100].
func computeBuckets(ctx context.Context, gdb *gorm.DB, groupCol string, createdAfter int64) ([]statsBucket, error) {
	// Aggregate columns are aliased to match the row struct's exported field
	// names so GORM's Scan maps them correctly (the raw COUNT(*)/SUM(...) names
	// wouldn't match Key/Requests/etc.).
	q := gdb.WithContext(ctx).Model(&schema.ConsoleRequest{}).Select(fmt.Sprintf(`
		%s AS key,
		COUNT(*) AS requests,
		COALESCE(SUM(input_tokens), 0) AS input_tok,
		COALESCE(SUM(output_tokens), 0) AS output_tok,
		COALESCE(SUM(CASE WHEN cache_read_input_tokens > 0 THEN 1 ELSE 0 END), 0) AS cache_r,
		COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0) AS errs
	`, groupCol))
	if createdAfter > 0 {
		q = q.Where("created_at >= ?", createdAfter)
	}
	type row struct {
		Key      string
		Requests int64
		InputTok int64
		OutputTok int64
		CacheR   int64
		Errs     int64
	}
	var rows []row
	if err := q.Group(groupCol).Order(groupCol).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]statsBucket, 0, len(rows))
	for _, r := range rows {
		out = append(out, statsBucket{
			Key: r.Key, Requests: r.Requests, InputTokens: r.InputTok,
			OutputTokens: r.OutputTok, CacheHits: r.CacheR, Errors: r.Errs,
		})
	}
	return out, nil
}

// bucketsToObj converts statsBuckets into the dashboard's bucket shape.
func bucketsToObj(buckets []statsBucket) []obj {
	out := make([]obj, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, obj{
			"key": b.Key, "label": b.Key,
			"requests":           b.Requests,
			"errors":             b.Errors,
			"cache_hits":         b.CacheHits,
			"cache_creates":      0,
			"total_input_tokens": b.InputTokens,
			"total_output_tokens": b.OutputTokens,
		})
	}
	return out
}

// tsPoint is one bucket of the time-series.
type tsPoint struct {
	BucketStart int64
	BucketLabel string
	Requests    int64
	Errors      int64
	Tokens      int64
}

// timeseriesBucketMs returns the bucket size (ms) computeTimeseries will use
// for the given range. MUST stay in sync with computeTimeseries so the cost
// pass buckets requests into the same buckets the chart renders.
func timeseriesBucketMs(createdAfter int64) int64 {
	now := time.Now().UnixMilli()
	switch {
	case createdAfter > now-int64(time.Hour/time.Millisecond):
		return 60 * 1000
	case createdAfter > now-int64(7*24*time.Hour/time.Millisecond):
		return 300 * 1000
	default:
		return 3600 * 1000
	}
}

// computeTimeseries buckets requests by time based on the active range. Bucket
// size: 1min (1h), 5min (24h/72h), 1h (7d), 1d (all-time/30d). Generates empty
// buckets so the dashboard always shows a contiguous series.
func computeTimeseries(ctx context.Context, gdb *gorm.DB, createdAfter int64) []obj {
	bucketSec := int64(3600) // default 1h
	now := time.Now().UnixMilli()
	switch {
	case createdAfter > now-int64(time.Hour/time.Millisecond):
		bucketSec = 60
	case createdAfter > now-int64(7*24*time.Hour/time.Millisecond):
		bucketSec = 300
	}
	// If createdAfter is 0 (all time), use a coarse 1-day bucket and limit rows.
	if createdAfter == 0 {
		createdAfter = now - 30*24*3600*1000 // last 30 days max
		bucketSec = 86400
	}
	// Generate empty buckets so the dashboard always shows a contiguous series.
	points := []obj{}
	for t := alignDown(createdAfter, bucketSec*1000); t < now; t += bucketSec * 1000 {
		points = append(points, obj{
			"bucket_start": t,
			"bucket_label": time.UnixMilli(t).Format("01-02 15:04"),
			"requests":     0, "errors": 0, "total_tokens": 0, "total_cost": 0,
		})
	}
	// Fill in actual data. Integer-division bucketing on epoch-ms works across
	// all three dialects.
	type actual struct {
		BucketStart int64
		Requests    int64
		Tokens      int64
		Errors      int64
	}
	var rows []actual
	err := gdb.WithContext(ctx).Model(&schema.ConsoleRequest{}).Select(fmt.Sprintf(`
		(created_at / (%d * 1000)) * (%d * 1000) AS bucket_start,
		COUNT(*) AS requests,
		COALESCE(SUM(input_tokens + output_tokens), 0) AS tokens,
		COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0) AS errors
	`, bucketSec, bucketSec)).
		Where("created_at >= ?", createdAfter).
		Group("bucket_start").
		Order("bucket_start").
		Scan(&rows).Error
	if err != nil {
		return points
	}
	// Index by bucket_start for fast updates.
	index := map[int64]int{}
	for i, p := range points {
		if bs, ok := p["bucket_start"].(int64); ok {
			index[bs] = i
		}
	}
	for _, r := range rows {
		bs := alignDown(r.BucketStart, bucketSec*1000)
		if i, ok := index[bs]; ok {
			points[i]["requests"] = r.Requests
			points[i]["errors"] = r.Errors
			points[i]["total_tokens"] = r.Tokens
			points[i]["total_cost"] = 0 // catalog lookup not implemented
		}
	}
	return points
}

func alignDown(ms int64, granularity int64) int64 {
	if granularity == 0 {
		return ms
	}
	return (ms / granularity) * granularity
}

// costBreakdown holds the USD cost split into its four pricing components. The
// dashboard's "成本拆分" (cost breakdown) card renders these four plus total.
type costBreakdown struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

func (c costBreakdown) Total() float64 {
	return c.Input + c.Output + c.CacheRead + c.CacheWrite
}

// costRow is a lightweight projection of one request, just the columns needed
// to price it. Pulling only these (no payloads/headers) keeps the cost pass
// cheap relative to the row size.
type costRow struct {
	CreatedAt    int64
	RequestModel string
	ResponseModel *string
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheCreate  int64
}

// computeCosts prices every request in range using the in-memory catalog and
// returns both the overall breakdown and a per-time-bucket breakdown. One query
// feeds the overview cost cards and the cost line on the time-series chart, so
// we don't walk the table twice. Requests whose model has no catalog pricing
// are skipped (their components stay 0 — same policy as the per-request list).
func computeCosts(ctx context.Context, gdb *gorm.DB, cat catalogLooker, createdAfter, bucketMs int64) (costBreakdown, map[int64]costBreakdown) {
	total := costBreakdown{}
	byBucket := map[int64]costBreakdown{}
	if cat == nil {
		return total, byBucket
	}

	var rows []costRow
	q := gdb.WithContext(ctx).Model(&schema.ConsoleRequest{}).Select(
		"created_at", "request_model", "response_model",
		"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens",
	)
	if createdAfter > 0 {
		q = q.Where("created_at >= ?", createdAfter)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return total, byBucket
	}

	for _, row := range rows {
		// Resolve the model id: prefer the model the upstream actually used
		// (response_model), fall back to the requested one.
		modelID := row.RequestModel
		if row.ResponseModel != nil && *row.ResponseModel != "" {
			modelID = *row.ResponseModel
		}
		p := cat.LookupPricing(modelID)
		if p == nil {
			continue
		}
		cb := priceRow(row, p)
		total.Input += cb.Input
		total.Output += cb.Output
		total.CacheRead += cb.CacheRead
		total.CacheWrite += cb.CacheWrite

		if bucketMs > 0 {
			bk := alignDown(row.CreatedAt, bucketMs)
			cur := byBucket[bk]
			cur.Input += cb.Input
			cur.Output += cb.Output
			cur.CacheRead += cb.CacheRead
			cur.CacheWrite += cb.CacheWrite
			byBucket[bk] = cur
		}
	}
	return total, byBucket
}

// priceRow computes the four cost components for one request from its token
// counts and the model's per-1M-token prices.
func priceRow(row costRow, p *pricing.ModelPricing) costBreakdown {
	cb := costBreakdown{}
	if p.Input != nil {
		cb.Input = float64(row.InputTokens) / pricing.TokensPerMillion * *p.Input
	}
	if p.Output != nil {
		cb.Output = float64(row.OutputTokens) / pricing.TokensPerMillion * *p.Output
	}
	if p.CacheRead != nil && row.CacheRead > 0 {
		cb.CacheRead = float64(row.CacheRead) / pricing.TokensPerMillion * *p.CacheRead
	}
	if p.CacheWrite != nil && row.CacheCreate > 0 {
		cb.CacheWrite = float64(row.CacheCreate) / pricing.TokensPerMillion * *p.CacheWrite
	}
	return cb
}

// distinctColumn returns the distinct non-null, non-empty values of col from
// console_requests (col is controlled by the caller; never user input).
func distinctColumn(ctx context.Context, gdb *gorm.DB, col string) []string {
	type row struct {
		Val string
	}
	var rows []row
	if err := gdb.WithContext(ctx).Model(&schema.ConsoleRequest{}).
		Where(col+" IS NOT NULL AND "+col+" != ''").
		Distinct(col).
		Order(col).
		Scan(&rows).Error; err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Val)
	}
	return out
}

// modelOverrideEntry is one row of model_metadata_overrides for the models page.
type modelOverrideEntry struct {
	ChannelName string
	ModelID     string
	Context     *int
	Pricing     map[string]interface{}
	UpdatedAt   int64
}

// loadModelOverrides returns a map keyed by "channelName:modelId" → override obj.
func loadModelOverrides(ctx context.Context, gdb *gorm.DB) (map[string]modelOverrideEntry, error) {
	var rows []schema.ModelMetadataOverride
	if err := gdb.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]modelOverrideEntry{}
	for _, r := range rows {
		entry := modelOverrideEntry{
			ChannelName: r.ChannelName,
			ModelID:     r.ModelID,
			Context:     r.ContextWindow,
			UpdatedAt:   r.UpdatedAt,
		}
		if r.PricingJSON != nil && *r.PricingJSON != "" {
			var p map[string]interface{}
			if err := json.Unmarshal([]byte(*r.PricingJSON), &p); err == nil {
				entry.Pricing = p
			}
		}
		out[r.ChannelName+":"+r.ModelID] = entry
	}
	return out, nil
}

// upsertModelMetadata upserts a per-(channel, model) override. Uses GORM's
// OnConflict clause so the upsert syntax is generated per-dialect (PG/SQLite
// ON CONFLICT, MySQL ON DUPLICATE KEY UPDATE). Requires the composite unique
// index idx_model_metadata_channel_model declared on the model.
func upsertModelMetadata(ctx context.Context, gdb *gorm.DB, channel, model string, contextWindow *int, pricingJSON *string) error {
	now := time.Now().UnixMilli()
	row := schema.ModelMetadataOverride{
		ChannelName:   channel,
		ModelID:       model,
		ContextWindow: contextWindow,
		PricingJSON:   pricingJSON,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	return gdb.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "channel_name"}, {Name: "model_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"context_window", "pricing_json", "updated_at"}),
	}).Create(&row).Error
}
