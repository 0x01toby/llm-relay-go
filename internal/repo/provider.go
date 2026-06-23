// Package repo implements the database access layer using GORM. Each
// repository owns one table and is constructed with a shared *gorm.DB. This
// works across Postgres, MySQL, and SQLite (GORM translates placeholders and
// dialect-specific upsert syntax).
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/taozhang/llmrelay/internal/schema"
)

// ProviderRepo owns the console_providers table.
type ProviderRepo struct {
	db *gorm.DB
}

// NewProviderRepo builds a ProviderRepo against gdb.
func NewProviderRepo(gdb *gorm.DB) *ProviderRepo { return &ProviderRepo{db: gdb} }

// ProviderInput is the mutation payload (mirrors ConfigEntry).
type ProviderInput struct {
	Type              string
	TargetBaseURL     string
	SystemPrompt      *string
	Models            []map[string]interface{} // each may carry context etc.
	Priority          int
	Enabled           bool
	RoutingVisibility string
	AuthHeader        *string
	AuthValue         *string
	ExtraFields       map[string]interface{}
	ProviderUUID      string
}

// ErrNotFound is returned when a row targeted by an update/delete is absent.
var ErrNotFound = errors.New("not found")

// List returns all providers ordered by channel name, backfilling any rows
// missing a provider_uuid.
func (r *ProviderRepo) List(ctx context.Context) ([]schema.ConsoleProvider, error) {
	var rows []schema.ConsoleProvider
	if err := r.db.WithContext(ctx).Order("channel_name ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	// Backfill any rows missing a provider_uuid (mirrors the original).
	var needBackfill []string
	for i := range rows {
		if rows[i].ProviderUUID == "" {
			needBackfill = append(needBackfill, rows[i].ChannelName)
		}
	}
	for _, name := range needBackfill {
		if err := r.db.WithContext(ctx).Model(&schema.ConsoleProvider{}).
			Where("channel_name = ?", name).
			Update("provider_uuid", uuid.NewString()).Error; err != nil {
			return nil, fmt.Errorf("backfill uuid for %s: %w", name, err)
		}
	}
	if len(needBackfill) > 0 {
		return r.List(ctx)
	}
	return rows, nil
}

// Create inserts a new provider. The provider_uuid is generated if empty.
func (r *ProviderRepo) Create(ctx context.Context, channelName string, in ProviderInput) error {
	pUUID := in.ProviderUUID
	if pUUID == "" {
		pUUID = uuid.NewString()
	}
	row := buildProviderRow(channelName, in, pUUID)
	return r.db.WithContext(ctx).Create(&row).Error
}

// Upsert inserts or updates a provider on conflict, preserving the existing
// provider_uuid if the row already exists.
func (r *ProviderRepo) Upsert(ctx context.Context, channelName string, in ProviderInput) error {
	// Preserve existing UUID if present.
	var existingUUID string
	err := r.db.WithContext(ctx).Model(&schema.ConsoleProvider{}).
		Where("channel_name = ?", channelName).
		Select("provider_uuid").Row().Scan(&existingUUID)
	if err != nil && !isNoRows(err) {
		return err
	}
	pUUID := in.ProviderUUID
	if pUUID == "" {
		if existingUUID != "" {
			pUUID = existingUUID
		} else {
			pUUID = uuid.NewString()
		}
	}
	row := buildProviderRow(channelName, in, pUUID)
	// ON CONFLICT (channel_name) DO UPDATE — GORM translates per dialect.
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "channel_name"}},
		DoUpdates: clause.AssignmentColumns([]string{"provider_uuid", "type", "target_base_url", "system_prompt", "models_json", "priority", "auth_header", "auth_value", "extra_fields_json", "routing_visibility", "enabled", "updated_at"}),
	}).Create(&row).Error
}

// Update renames and/or updates a provider. Returns ErrNotFound if the row is
// absent.
func (r *ProviderRepo) Update(ctx context.Context, currentName, nextName string, in ProviderInput) error {
	var existing schema.ConsoleProvider
	err := r.db.WithContext(ctx).Where("channel_name = ?", currentName).First(&existing).Error
	if err != nil {
		if isNoRows(err) {
			return fmt.Errorf("Provider %q %w", currentName, ErrNotFound)
		}
		return err
	}
	existingUUID := existing.ProviderUUID
	if existingUUID == "" {
		existingUUID = uuid.NewString()
	}
	row := buildProviderRow(nextName, in, existingUUID)
	enabled := boolToInt(in.Enabled)
	row.Enabled = enabled
	res := r.db.WithContext(ctx).Model(&schema.ConsoleProvider{}).
		Where("channel_name = ?", currentName).
		Updates(map[string]interface{}{
			"channel_name":       nextName,
			"type":               row.Type,
			"target_base_url":    row.TargetBaseURL,
			"system_prompt":      row.SystemPrompt,
			"models_json":        row.ModelsJSON,
			"priority":           row.Priority,
			"auth_header":        row.AuthHeader,
			"auth_value":         row.AuthValue,
			"extra_fields_json":  row.ExtraFieldsJSON,
			"routing_visibility": row.RoutingVisibility,
			"provider_uuid":      existingUUID,
			"enabled":            enabled,
			"updated_at":         row.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("Provider %q %w", currentName, ErrNotFound)
	}
	return nil
}

// SetEnabled toggles a provider's enabled flag.
func (r *ProviderRepo) SetEnabled(ctx context.Context, channelName string, enabled bool) error {
	res := r.db.WithContext(ctx).Model(&schema.ConsoleProvider{}).
		Where("channel_name = ?", channelName).
		Updates(map[string]interface{}{"enabled": boolToInt(enabled), "updated_at": nowMs()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("Provider %q %w", channelName, ErrNotFound)
	}
	return nil
}

// Delete removes a provider.
func (r *ProviderRepo) Delete(ctx context.Context, channelName string) error {
	res := r.db.WithContext(ctx).Where("channel_name = ?", channelName).Delete(&schema.ConsoleProvider{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("Provider %q %w", channelName, ErrNotFound)
	}
	return nil
}

// Clear removes all providers.
func (r *ProviderRepo) Clear(ctx context.Context) error {
	return r.db.WithContext(ctx).Where("1 = 1").Delete(&schema.ConsoleProvider{}).Error
}

// GetByChannel returns the row for channelName, or ErrNotFound if absent.
func (r *ProviderRepo) GetByChannel(ctx context.Context, channelName string) (schema.ConsoleProvider, error) {
	var row schema.ConsoleProvider
	err := r.db.WithContext(ctx).Where("channel_name = ?", channelName).First(&row).Error
	if err != nil {
		if isNoRows(err) {
			return schema.ConsoleProvider{}, ErrNotFound
		}
		return schema.ConsoleProvider{}, err
	}
	return row, nil
}

// buildProviderRow assembles a ConsoleProvider from the input + channel/uuid.
func buildProviderRow(channelName string, in ProviderInput, pUUID string) schema.ConsoleProvider {
	return schema.ConsoleProvider{
		ChannelName:       channelName,
		ProviderUUID:      pUUID,
		Type:              typeOrDefault(in.Type),
		TargetBaseURL:     in.TargetBaseURL,
		SystemPrompt:      in.SystemPrompt,
		ModelsJSON:        modelsJSON(in.Models),
		Priority:          in.Priority,
		AuthHeader:        in.AuthHeader,
		AuthValue:         in.AuthValue,
		ExtraFieldsJSON:   extraFieldsJSON(in.ExtraFields),
		RoutingVisibility: visibilityOrDefault(in.RoutingVisibility),
		Enabled:           boolToInt(in.Enabled),
		CreatedAt:         nowMs(),
		UpdatedAt:         nowMs(),
	}
}

func modelsJSON(models []map[string]interface{}) string {
	if len(models) == 0 {
		return "[]"
	}
	b, err := jsonMarshal(models)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func extraFieldsJSON(fields map[string]interface{}) string {
	if len(fields) == 0 {
		return ""
	}
	b, err := jsonMarshal(fields)
	if err != nil {
		return ""
	}
	return string(b)
}

func visibilityOrDefault(v string) string {
	if v == "" {
		return "direct"
	}
	return v
}

func typeOrDefault(t string) string {
	if t == "" {
		return "openai"
	}
	return t
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
