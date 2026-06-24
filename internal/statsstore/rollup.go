// Package statsstore owns the pre-aggregated request statistics table
// (request_stats_5m). A periodic background job folds new console_requests rows
// into 5-minute buckets keyed by (route, model, client), pre-pricing each with
// the in-memory catalog, so the Usage/Monitor dashboards can read accurate
// long-term stats even after old log rows are pruned by the retention cap.
package statsstore

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/pricing"
	"github.com/taozhang/llmrelay/internal/schema"
)

const (
	// bucketMs is the 5-minute bucket width (epoch-ms). All rollup rows are at
	// this granularity; larger windows are produced by grouping multiple rows.
	bucketMs = 5 * 60 * 1000
	// tokensPerMillion is the divisor shared with pricing.CalculateCost /
	// priceRow — prices are per 1M tokens.
	tokensPerMillion = 1_000_000.0
)

// priceLooker is the subset of catalog.Service the rollup needs: resolving a
// model's per-1M-token pricing. Defined locally so this package doesn't import
// consoleapi (which would be a cycle) and stays testable with a stub.
type priceLooker interface {
	LookupPricing(modelID string) *pricing.ModelPricing
}

// Rollup folds new console_requests rows into request_stats_5m. It tracks its
// progress via max_request_created_at stored on each rollup row, so it only
// re-scans rows seen since the last tick (incremental).
type Rollup struct {
	db  *gorm.DB
	cat priceLooker
}

// NewRollup builds a Rollup. cat may be nil to disable cost computation.
func NewRollup(gdb *gorm.DB, cat priceLooker) *Rollup {
	return &Rollup{db: gdb, cat: cat}
}

// rollupRow is a lightweight projection of one console_requests row, just the
// columns the rollup needs (no payloads/headers).
type rollupRow struct {
	CreatedAt           int64
	RoutePrefix         string
	RequestModel        string
	ResponseModel       *string
	APIKeyName          *string
	ResponseStatus      *int
	FailoverFrom        *string
	CacheReadTokens     int64
	CacheCreateTokens   int64
	InputTokens         int64
	OutputTokens        int64
	CachedInputTokens   int64
	ReasoningTokens     int64
	CompletedAt         *int64
	FirstTokenAt        *int64
}

// aggregate is the in-memory accumulator for one (bucket, route, model, client) key.
type aggregate struct {
	Requests            int64
	Errors              int64
	Failovers           int64
	CacheHits           int64
	CacheCreates        int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreateTokens   int64
	CachedInputTokens   int64
	ReasoningTokens     int64
	InputCost           float64
	OutputCost          float64
	CacheReadCost       float64
	CacheWriteCost      float64
	SumDurationMs       int64
	SumFirstTokenMs     int64
	CountTimed          int64
	MaxRequestCreatedAt int64
}

// bucketKey identifies one aggregate group.
type bucketKey struct {
	Bucket       int64
	Route        string
	Model        string
	Client       string
}

// RollupTick advances the rollup cursor: reads console_requests rows newer than
// the last processed created_at, aggregates them into 5-minute buckets, and
// upserts them into request_stats_5m. Idempotent — re-running with no new rows
// is a no-op.
func (r *Rollup) RollupTick(ctx context.Context) error {
	// Derive the cursor: the largest max_request_created_at already rolled up.
	var cursor int64
	if err := r.db.WithContext(ctx).Model(&schema.RequestStats5m{}).
		Select("COALESCE(MAX(max_request_created_at), 0)").Row().Scan(&cursor); err != nil {
		return err
	}

	// Pull new rows since the cursor. Cap at a batch so a huge backlog after a
	// restart degrades gracefully instead of one giant transaction.
	var rows []rollupRow
	if err := r.db.WithContext(ctx).Model(&schema.ConsoleRequest{}).
		Select("created_at", "route_prefix", "request_model", "response_model",
			"api_key_name", "response_status", "failover_from",
			"cache_read_input_tokens", "cache_creation_input_tokens",
			"input_tokens", "output_tokens", "cached_input_tokens",
			"reasoning_output_tokens", "completed_at", "first_token_at").
		Where("created_at > ?", cursor).
		Order("created_at ASC").
		Limit(5000).
		Scan(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	// Fold rows into in-memory aggregates.
	aggs := map[bucketKey]*aggregate{}
	for i := range rows {
		row := &rows[i]
		bk := bucketKey{
			Bucket: alignDown(row.CreatedAt, bucketMs),
			Route:  row.RoutePrefix,
			Model:  resolveModel(row),
			Client: strOr(row.APIKeyName, ""),
		}
		ag := aggs[bk]
		if ag == nil {
			ag = &aggregate{}
			aggs[bk] = ag
		}
		r.fold(ag, row)
	}

	// Persist: for each (bucket, dim) delete any prior rows then insert the
	// freshly computed aggregate. Delete+insert per key is idempotent if a tick
	// overlaps the cursor (the overlapping bucket is recomputed from scratch).
	if err := r.persist(ctx, aggs); err != nil {
		return err
	}

	log.Printf("[stats] rollup: processed %d row(s) into %d bucket(s)", len(rows), len(aggs))
	return nil
}

// fold adds one request row into an aggregate accumulator.
func (r *Rollup) fold(ag *aggregate, row *rollupRow) {
	ag.Requests++
	if row.ResponseStatus == nil || *row.ResponseStatus >= 400 {
		ag.Errors++
	}
	if row.FailoverFrom != nil && *row.FailoverFrom != "" {
		ag.Failovers++
	}
	if row.CacheReadTokens > 0 {
		ag.CacheHits++
	}
	if row.CacheCreateTokens > 0 {
		ag.CacheCreates++
	}
	ag.InputTokens += row.InputTokens
	ag.OutputTokens += row.OutputTokens
	ag.CacheReadTokens += row.CacheReadTokens
	ag.CacheCreateTokens += row.CacheCreateTokens
	ag.CachedInputTokens += row.CachedInputTokens
	ag.ReasoningTokens += row.ReasoningTokens

	// Pre-price with the catalog (may be nil → costs stay 0).
	if r.cat != nil {
		if p := r.cat.LookupPricing(resolveModel(row)); p != nil {
			if p.Input != nil {
				ag.InputCost += float64(row.InputTokens) / tokensPerMillion * *p.Input
			}
			if p.Output != nil {
				ag.OutputCost += float64(row.OutputTokens) / tokensPerMillion * *p.Output
			}
			if p.CacheRead != nil && row.CacheReadTokens > 0 {
				ag.CacheReadCost += float64(row.CacheReadTokens) / tokensPerMillion * *p.CacheRead
			}
			if p.CacheWrite != nil && row.CacheCreateTokens > 0 {
				ag.CacheWriteCost += float64(row.CacheCreateTokens) / tokensPerMillion * *p.CacheWrite
			}
		}
	}

	// Latency (only when the request completed — otherwise duration is unknown).
	if row.CompletedAt != nil {
		ag.SumDurationMs += *row.CompletedAt - row.CreatedAt
		ag.CountTimed++
	}
	if row.FirstTokenAt != nil {
		ag.SumFirstTokenMs += *row.FirstTokenAt - row.CreatedAt
	}

	if row.CreatedAt > ag.MaxRequestCreatedAt {
		ag.MaxRequestCreatedAt = row.CreatedAt
	}
}

// persist writes the aggregates into request_stats_5m, accumulating into any
// existing rows with the same (bucket_start, route, model, client) key. This
// uses an additive upsert (ON CONFLICT … DO UPDATE SET col = col + …) so that
// multiple ticks touching the same open 5m bucket sum correctly rather than
// overwriting each other. id is excluded from the conflict target.
func (r *Rollup) persist(ctx context.Context, aggs map[bucketKey]*aggregate) error {
	now := time.Now().UnixMilli()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for bk, ag := range aggs {
			row := schema.RequestStats5m{
				BucketStart:         bk.Bucket,
				RoutePrefix:         bk.Route,
				RequestModel:        bk.Model,
				APIKeyName:          bk.Client,
				Requests:            ag.Requests,
				Errors:              ag.Errors,
				Failovers:           ag.Failovers,
				CacheHits:           ag.CacheHits,
				CacheCreates:        ag.CacheCreates,
				InputTokens:         ag.InputTokens,
				OutputTokens:        ag.OutputTokens,
				CacheReadTokens:     ag.CacheReadTokens,
				CacheCreateTokens:   ag.CacheCreateTokens,
				CachedInputTokens:   ag.CachedInputTokens,
				ReasoningTokens:     ag.ReasoningTokens,
				InputCostUSD:        ag.InputCost,
				OutputCostUSD:       ag.OutputCost,
				CacheReadCostUSD:    ag.CacheReadCost,
				CacheWriteCostUSD:   ag.CacheWriteCost,
				SumDurationMs:       ag.SumDurationMs,
				SumFirstTokenMs:     ag.SumFirstTokenMs,
				CountTimed:          ag.CountTimed,
				MaxRequestCreatedAt: ag.MaxRequestCreatedAt,
				CreatedAt:           now,
			}
			if err := upsertAggregate(tx, &row); err != nil {
				return err
			}
		}
		return nil
	})
}

// upsertAggregate inserts a rollup row, or on a (bucket_start, route_prefix,
// request_model, api_key_name) conflict adds this tick's values into the
// existing row. The additive form keeps an open 5m bucket correct across
// multiple ticks. Cross-dialect: SQLite, Postgres, and MySQL 8+ all support the
// VALUES() function in ON DUPLICATE/ON CONFLICT DO UPDATE.
func upsertAggregate(tx *gorm.DB, row *schema.RequestStats5m) error {
	return tx.Exec(`INSERT INTO request_stats_5m (
		bucket_start, route_prefix, request_model, api_key_name,
		requests, errors, failovers, cache_hits, cache_creates,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		cached_input_tokens, reasoning_tokens,
		input_cost_usd, output_cost_usd, cache_read_cost_usd, cache_write_cost_usd,
		sum_duration_ms, sum_first_token_ms, count_timed,
		max_request_created_at, created_at
	) VALUES (?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?, ?,?,?, ?,?)
	ON CONFLICT(bucket_start, route_prefix, request_model, api_key_name) DO UPDATE SET
		requests = request_stats_5m.requests + excluded.requests,
		errors = request_stats_5m.errors + excluded.errors,
		failovers = request_stats_5m.failovers + excluded.failovers,
		cache_hits = request_stats_5m.cache_hits + excluded.cache_hits,
		cache_creates = request_stats_5m.cache_creates + excluded.cache_creates,
		input_tokens = request_stats_5m.input_tokens + excluded.input_tokens,
		output_tokens = request_stats_5m.output_tokens + excluded.output_tokens,
		cache_read_tokens = request_stats_5m.cache_read_tokens + excluded.cache_read_tokens,
		cache_creation_tokens = request_stats_5m.cache_creation_tokens + excluded.cache_creation_tokens,
		cached_input_tokens = request_stats_5m.cached_input_tokens + excluded.cached_input_tokens,
		reasoning_tokens = request_stats_5m.reasoning_tokens + excluded.reasoning_tokens,
		input_cost_usd = request_stats_5m.input_cost_usd + excluded.input_cost_usd,
		output_cost_usd = request_stats_5m.output_cost_usd + excluded.output_cost_usd,
		cache_read_cost_usd = request_stats_5m.cache_read_cost_usd + excluded.cache_read_cost_usd,
		cache_write_cost_usd = request_stats_5m.cache_write_cost_usd + excluded.cache_write_cost_usd,
		sum_duration_ms = request_stats_5m.sum_duration_ms + excluded.sum_duration_ms,
		sum_first_token_ms = request_stats_5m.sum_first_token_ms + excluded.sum_first_token_ms,
		count_timed = request_stats_5m.count_timed + excluded.count_timed,
		max_request_created_at = MAX(request_stats_5m.max_request_created_at, excluded.max_request_created_at),
		created_at = excluded.created_at`,
		row.BucketStart, row.RoutePrefix, row.RequestModel, row.APIKeyName,
		row.Requests, row.Errors, row.Failovers, row.CacheHits, row.CacheCreates,
		row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheCreateTokens,
		row.CachedInputTokens, row.ReasoningTokens,
		row.InputCostUSD, row.OutputCostUSD, row.CacheReadCostUSD, row.CacheWriteCostUSD,
		row.SumDurationMs, row.SumFirstTokenMs, row.CountTimed,
		row.MaxRequestCreatedAt, row.CreatedAt,
	).Error
}

// resolveModel returns the model id to roll up under: the upstream's actual
// response_model if present, else the requested model.
func resolveModel(row *rollupRow) string {
	if row.ResponseModel != nil && *row.ResponseModel != "" {
		return *row.ResponseModel
	}
	return row.RequestModel
}

// strOr dereferences a nullable string, defaulting to "".
func strOr(s *string, def string) string {
	if s != nil {
		return *s
	}
	return def
}

// alignDown rounds an epoch-ms timestamp down to the nearest bucket boundary.
func alignDown(ms int64, granularity int64) int64 {
	if granularity == 0 {
		return ms
	}
	return (ms / granularity) * granularity
}
