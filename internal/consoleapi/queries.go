package consoleapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/taozhang/llmrelay/internal/schema"
)

// statsOverview holds the aggregate counts computed by computeOverview.
type statsOverview struct {
	Total        int64
	CacheHits    int64
	CacheCreates int64
	CacheMisses  int64
	Errors       int64
	Failovers    int64
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// computeOverview runs the dashboard's overview aggregate over console_requests
// (optionally filtered by created_after epoch-ms). Uses SUM(CASE WHEN ...)
// instead of PG's FILTER(WHERE) for portability.
func computeOverview(ctx context.Context, gdb *gorm.DB, createdAfter int64) (statsOverview, error) {
	q := gdb.WithContext(ctx).Model(&schema.ConsoleRequest{}).Select(`
		COUNT(*),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cache_read_input_tokens), 0),
		COALESCE(SUM(cache_creation_input_tokens), 0),
		COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN failover_from IS NOT NULL AND failover_from != '' THEN 1 ELSE 0 END), 0)
	`)
	if createdAfter > 0 {
		q = q.Where("created_at >= ?", createdAfter)
	}
	var total, inTok, outTok, cacheRead, cacheCreate, errors, failovers int64
	row := q.Row()
	if err := row.Scan(&total, &inTok, &outTok, &cacheRead, &cacheCreate, &errors, &failovers); err != nil {
		return statsOverview{}, err
	}
	misses := total - cacheRead - cacheCreate
	if misses < 0 {
		misses = 0
	}
	return statsOverview{
		Total: total, InputTokens: inTok, OutputTokens: outTok,
		TotalTokens: inTok + outTok, CacheHits: cacheRead, CacheCreates: cacheCreate,
		CacheMisses: misses, Errors: errors, Failovers: failovers,
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
// caller; never user input).
func computeBuckets(ctx context.Context, gdb *gorm.DB, groupCol string, createdAfter int64) ([]statsBucket, error) {
	q := gdb.WithContext(ctx).Model(&schema.ConsoleRequest{}).Select(fmt.Sprintf(`
		%s,
		COUNT(*),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cache_read_input_tokens), 0),
		COALESCE(SUM(CASE WHEN response_status IS NULL OR response_status >= 400 THEN 1 ELSE 0 END), 0)
	`, groupCol))
	if createdAfter > 0 {
		q = q.Where("created_at >= ?", createdAfter)
	}
	type row struct {
		Key      string
		Requests int64
		InputTok int64
		OutputTo int64
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
			OutputTokens: r.OutputTo, CacheHits: r.CacheR, Errors: r.Errs,
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

// loadModelPricing returns a map keyed by model id → pricing obj, sourced from
// model_catalog_cache.
func loadModelPricing(ctx context.Context, gdb *gorm.DB) (map[string]map[string]interface{}, error) {
	var rows []schema.ModelCatalogCache
	if err := gdb.WithContext(ctx).Where("pricing_json IS NOT NULL AND pricing_json != ''").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]map[string]interface{}{}
	for _, r := range rows {
		if r.PricingJSON == nil || *r.PricingJSON == "" {
			continue
		}
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(*r.PricingJSON), &p); err == nil {
			out[r.ModelID] = p
		}
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
