package consoleapi

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/schema"
)

// This file holds the dashboard stats queries that read from the pre-aggregated
// request_stats_5m rollup table (populated by internal/statsstore). Replacing
// the direct console_requests scans with these means stats stay accurate after
// old log rows are pruned by the retention cap, and the heavy pricing work was
// already done at rollup time (cost columns are pre-computed).

// rollupOverview is the aggregate computed from request_stats_5m. It mirrors
// statsOverview but adds cost + latency components and is populated in one
// SUM query (no per-row pricing pass needed).
type rollupOverview struct {
	Total        int64
	CacheHits    int64
	CacheCreates int64
	Errors       int64
	Failovers    int64
	InputTokens  int64
	OutputTokens int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	CachedInputTokens   int64
	ReasoningTokens     int64
	InputCost      float64
	OutputCost     float64
	CacheReadCost  float64
	CacheWriteCost float64
	SumDurationMs    int64
	SumFirstTokenMs  int64
	CountTimed       int64
}

func computeOverviewFromRollup(ctx context.Context, gdb *gorm.DB, createdAfter int64) (rollupOverview, error) {
	q := gdb.WithContext(ctx).Model(&schema.RequestStats5m{}).Select(`
		COALESCE(SUM(requests), 0),
		COALESCE(SUM(cache_hits), 0),
		COALESCE(SUM(cache_creates), 0),
		COALESCE(SUM(errors), 0),
		COALESCE(SUM(failovers), 0),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cache_read_tokens), 0),
		COALESCE(SUM(cache_creation_tokens), 0),
		COALESCE(SUM(cached_input_tokens), 0),
		COALESCE(SUM(reasoning_tokens), 0),
		COALESCE(SUM(input_cost_usd), 0),
		COALESCE(SUM(output_cost_usd), 0),
		COALESCE(SUM(cache_read_cost_usd), 0),
		COALESCE(SUM(cache_write_cost_usd), 0),
		COALESCE(SUM(sum_duration_ms), 0),
		COALESCE(SUM(sum_first_token_ms), 0),
		COALESCE(SUM(count_timed), 0)
	`)
	if createdAfter > 0 {
		q = q.Where("bucket_start >= ?", createdAfter)
	}
	var ov rollupOverview
	row := q.Row()
	if err := row.Scan(
		&ov.Total, &ov.CacheHits, &ov.CacheCreates, &ov.Errors, &ov.Failovers,
		&ov.InputTokens, &ov.OutputTokens, &ov.CacheReadTokens, &ov.CacheCreationTokens,
		&ov.CachedInputTokens, &ov.ReasoningTokens,
		&ov.InputCost, &ov.OutputCost, &ov.CacheReadCost, &ov.CacheWriteCost,
		&ov.SumDurationMs, &ov.SumFirstTokenMs, &ov.CountTimed,
	); err != nil {
		return rollupOverview{}, err
	}
	return ov, nil
}

// rollupBucket mirrors statsBucket plus cost/latency, for the grouped tables.
type rollupBucket struct {
	Key         string
	Requests    int64
	Errors      int64
	CacheHits   int64
	InputTokens int64
	OutputTokens int64
	InputCost   float64
	OutputCost  float64
	CacheReadCost float64
	CacheWriteCost float64
	TotalCost   float64
	SumDurationMs int64
	CountTimed    int64
}

// computeBucketsFromRollup groups request_stats_5m by groupCol and aggregates.
// groupCol is route_prefix / request_model / api_key_name (caller-controlled).
func computeBucketsFromRollup(ctx context.Context, gdb *gorm.DB, groupCol string, createdAfter int64) ([]rollupBucket, error) {
	q := gdb.WithContext(ctx).Model(&schema.RequestStats5m{}).Select(fmt.Sprintf(`
		%s AS key,
		COALESCE(SUM(requests), 0) AS requests,
		COALESCE(SUM(errors), 0) AS errors,
		COALESCE(SUM(cache_hits), 0) AS cache_hits,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(input_cost_usd + output_cost_usd + cache_read_cost_usd + cache_write_cost_usd), 0) AS total_cost,
		COALESCE(SUM(sum_duration_ms), 0) AS sum_duration_ms,
		COALESCE(SUM(count_timed), 0) AS count_timed
	`, groupCol))
	if createdAfter > 0 {
		q = q.Where("bucket_start >= ?", createdAfter)
	}
	type row struct {
		Key           string
		Requests      int64
		Errors        int64
		CacheHits     int64
		InputTokens   int64
		OutputTokens  int64
		TotalCost     float64
		SumDurationMs int64
		CountTimed    int64
	}
	var rows []row
	if err := q.Group(groupCol).Order(groupCol).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]rollupBucket, 0, len(rows))
	for _, r := range rows {
		out = append(out, rollupBucket{
			Key: r.Key, Requests: r.Requests, Errors: r.Errors, CacheHits: r.CacheHits,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			TotalCost: r.TotalCost, SumDurationMs: r.SumDurationMs, CountTimed: r.CountTimed,
		})
	}
	return out, nil
}

// rollupBucketsToObj converts rollupBuckets into the dashboard's bucket shape,
// matching what bucket-table.tsx reads (including total_cost, avg_duration_ms).
func rollupBucketsToObj(buckets []rollupBucket) []obj {
	out := make([]obj, 0, len(buckets))
	for _, b := range buckets {
		entry := obj{
			"key":                b.Key,
			"label":              b.Key,
			"requests":           b.Requests,
			"errors":             b.Errors,
			"cache_hits":         b.CacheHits,
			"cache_creates":      0,
			"total_input_tokens": b.InputTokens,
			"total_output_tokens": b.OutputTokens,
			"total_tokens":       b.InputTokens + b.OutputTokens,
			"total_cost":         b.TotalCost,
		}
		if b.CountTimed > 0 {
			entry["avg_duration_ms"] = b.SumDurationMs / b.CountTimed
		} else {
			entry["avg_duration_ms"] = nil
		}
		out = append(out, entry)
	}
	return out
}

// computeTimeseriesFromRollup buckets the 5m rollup rows up to the target
// granularity for the selected range, generating empty contiguous buckets like
// computeTimeseries does. targetMs is the desired bucket width (e.g. 5min/30min/
// 1h/24h); since every rollup row is 5m, we GROUP BY alignDown(bucket_start,
// targetMs).
func computeTimeseriesFromRollup(ctx context.Context, gdb *gorm.DB, createdAfter int64) []obj {
	bucketMs := timeseriesTargetBucketMs(createdAfter)
	now := time.Now().UnixMilli()
	// For all-time, cap to last 30 days (matches the old behavior).
	if createdAfter == 0 {
		createdAfter = now - 30*24*3600*1000
	}

	// Generate empty contiguous buckets.
	points := []obj{}
	for t := alignDown(createdAfter, bucketMs); t < now; t += bucketMs {
		points = append(points, obj{
			"bucket_start": t,
			"bucket_label": tsLabel(t, bucketMs),
			"requests":     0, "errors": 0, "total_tokens": 0, "total_cost": 0,
		})
	}

	// Aggregate 5m rollup rows up to the target bucket.
	type actual struct {
		BucketStart int64
		Requests    int64
		Tokens      int64
		Errors      int64
		Cost        float64
	}
	var rows []actual
	err := gdb.WithContext(ctx).Model(&schema.RequestStats5m{}).Select(fmt.Sprintf(`
		(bucket_start / %d) * %d AS bucket_start,
		COALESCE(SUM(requests), 0) AS requests,
		COALESCE(SUM(input_tokens + output_tokens), 0) AS tokens,
		COALESCE(SUM(errors), 0) AS errors,
		COALESCE(SUM(input_cost_usd + output_cost_usd + cache_read_cost_usd + cache_write_cost_usd), 0) AS cost
	`, bucketMs, bucketMs)).
		Where("bucket_start >= ?", createdAfter).
		Group("bucket_start").
		Order("bucket_start").
		Scan(&rows).Error
	if err != nil {
		return points
	}

	// Index by bucket_start for fast merge.
	index := map[int64]int{}
	for i, p := range points {
		if bs, ok := p["bucket_start"].(int64); ok {
			index[bs] = i
		}
	}
	for _, r := range rows {
		bs := alignDown(r.BucketStart, bucketMs)
		if i, ok := index[bs]; ok {
			points[i]["requests"] = r.Requests
			points[i]["errors"] = r.Errors
			points[i]["total_tokens"] = r.Tokens
			points[i]["total_cost"] = r.Cost
		}
	}
	return points
}

// timeseriesTargetBucketMs picks the chart bucket size for a range. Because the
// rollup is at 5m, anything >= 5m can be produced by grouping. 1h range → 5m
// buckets; 24h/72h → 30m; 7d → 1h; 30d/all → 24h.
func timeseriesTargetBucketMs(createdAfter int64) int64 {
	now := time.Now().UnixMilli()
	switch {
	case createdAfter == 0:
		return 24 * 3600 * 1000 // all-time: 1 day
	case createdAfter > now-int64(2*time.Hour/time.Millisecond):
		return 5 * 60 * 1000 // ≤2h: 5 min
	case createdAfter > now-int64(7*24*time.Hour/time.Millisecond):
		return 30 * 60 * 1000 // ≤7d: 30 min
	default:
		return 3600 * 1000 // ≤30d: 1 hour
	}
}

// tsLabel formats a bucket start for the chart axis, granularity-aware.
func tsLabel(ms int64, bucketMs int64) string {
	if bucketMs >= 24*3600*1000 {
		return time.UnixMilli(ms).Format("01-02")
	}
	return time.UnixMilli(ms).Format("01-02 15:04")
}
