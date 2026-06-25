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

// This file holds console DB helpers that aren't stats-rollup queries (those
// live in queries_rollup.go): filter-value extraction, model-metadata overrides,
// and the shared alignDown helper.

// alignDown rounds an epoch-ms timestamp down to the nearest bucket boundary.
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

// Ensure fmt stays referenced (used by distinctColumn's Where building in some
// dialect paths and by queries_rollup.go fmt.Sprintf).
var _ = fmt.Sprintf
