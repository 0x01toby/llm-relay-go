// Package repo implements the database access layer as hand-written pgx
// repositories. Each repository owns one table and is constructed with a
// shared *db.Pool. This replaces Drizzle ORM with direct SQL, keeping the
// query semantics identical to the original src/*-store.ts modules.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taozhang/llmrelay/internal/schema"
)

// ProviderRepo owns the console_providers table.
type ProviderRepo struct {
	pool *pgxpool.Pool
}

// NewProviderRepo builds a ProviderRepo against pool.
func NewProviderRepo(pool *pgxpool.Pool) *ProviderRepo { return &ProviderRepo{pool: pool} }

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
// missing a provider_uuid (mirrors listConsoleProviderEntries).
func (r *ProviderRepo) List(ctx context.Context) ([]schema.ConsoleProvider, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT channel_name, provider_uuid, type, target_base_url, system_prompt,
		       models_json, priority, auth_header, auth_value, extra_fields_json,
		       routing_visibility, enabled, created_at, updated_at
		FROM console_providers
		ORDER BY channel_name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []schema.ConsoleProvider
	var needBackfill []string
	for rows.Next() {
		var p schema.ConsoleProvider
		if err := scanProvider(rows, &p); err != nil {
			return nil, err
		}
		if p.ProviderUUID == "" {
			needBackfill = append(needBackfill, p.ChannelName)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, name := range needBackfill {
		if _, err := r.pool.Exec(ctx, `UPDATE console_providers SET provider_uuid = $1 WHERE channel_name = $2`, uuid.NewString(), name); err != nil {
			return nil, fmt.Errorf("backfill uuid for %s: %w", name, err)
		}
	}
	if len(needBackfill) > 0 {
		return r.List(ctx)
	}
	return out, nil
}

// Create inserts a new provider. The provider_uuid is generated if empty.
func (r *ProviderRepo) Create(ctx context.Context, channelName string, in ProviderInput) error {
	now := nowMs()
	pUUID := in.ProviderUUID
	if pUUID == "" {
		pUUID = uuid.NewString()
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO console_providers
		  (channel_name, provider_uuid, type, target_base_url, system_prompt,
		   models_json, priority, auth_header, auth_value, extra_fields_json,
		   routing_visibility, enabled, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`,
		channelName, pUUID, in.Type, in.TargetBaseURL, in.SystemPrompt,
		modelsJSON(in.Models), in.Priority, in.AuthHeader, in.AuthValue, extraFieldsJSON(in.ExtraFields),
		visibilityOrDefault(in.RoutingVisibility), boolToInt(in.Enabled), now, now,
	)
	return err
}

// Upsert inserts or updates a provider on conflict, preserving the existing
// provider_uuid if the row already exists (mirrors upsertConsoleProviderEntry).
func (r *ProviderRepo) Upsert(ctx context.Context, channelName string, in ProviderInput) error {
	now := nowMs()
	// Preserve existing UUID if present.
	var existingUUID string
	err := r.pool.QueryRow(ctx, `SELECT provider_uuid FROM console_providers WHERE channel_name = $1`, channelName).Scan(&existingUUID)
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
	_, err = r.pool.Exec(ctx, `
		INSERT INTO console_providers
		  (channel_name, provider_uuid, type, target_base_url, system_prompt,
		   models_json, priority, auth_header, auth_value, extra_fields_json,
		   routing_visibility, enabled, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (channel_name) DO UPDATE SET
		  provider_uuid = EXCLUDED.provider_uuid,
		  type = EXCLUDED.type,
		  target_base_url = EXCLUDED.target_base_url,
		  system_prompt = EXCLUDED.system_prompt,
		  models_json = EXCLUDED.models_json,
		  priority = EXCLUDED.priority,
		  auth_header = EXCLUDED.auth_header,
		  auth_value = EXCLUDED.auth_value,
		  extra_fields_json = EXCLUDED.extra_fields_json,
		  routing_visibility = EXCLUDED.routing_visibility,
		  enabled = EXCLUDED.enabled,
		  updated_at = EXCLUDED.updated_at
	`,
		channelName, pUUID, in.Type, in.TargetBaseURL, in.SystemPrompt,
		modelsJSON(in.Models), in.Priority, in.AuthHeader, in.AuthValue, extraFieldsJSON(in.ExtraFields),
		visibilityOrDefault(in.RoutingVisibility), boolToInt(in.Enabled), now, now,
	)
	return err
}

// Update renames and/or updates a provider. Returns ErrNotFound if the row is
// absent (mirrors updateConsoleProviderEntry).
func (r *ProviderRepo) Update(ctx context.Context, currentName, nextName string, in ProviderInput) error {
	now := nowMs()
	var existingUUID string
	err := r.pool.QueryRow(ctx, `SELECT provider_uuid FROM console_providers WHERE channel_name = $1`, currentName).Scan(&existingUUID)
	if err != nil {
		if isNoRows(err) {
			return fmt.Errorf("Provider %q %w", currentName, ErrNotFound)
		}
		return err
	}
	if existingUUID == "" {
		existingUUID = uuid.NewString()
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE console_providers SET
		  channel_name = $1, type = $2, target_base_url = $3, system_prompt = $4,
		  models_json = $5, priority = $6, auth_header = $7, auth_value = $8,
		  extra_fields_json = $9, routing_visibility = $10, provider_uuid = $11,
		  enabled = $12, updated_at = $13
		WHERE channel_name = $14
	`,
		nextName, in.Type, in.TargetBaseURL, in.SystemPrompt,
		modelsJSON(in.Models), in.Priority, in.AuthHeader, in.AuthValue,
		extraFieldsJSON(in.ExtraFields), visibilityOrDefault(in.RoutingVisibility),
		existingUUID, boolToInt(in.Enabled), now, currentName,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("Provider %q %w", currentName, ErrNotFound)
	}
	return nil
}

// SetEnabled toggles a provider's enabled flag (mirrors toggleConsoleProviderEntry).
func (r *ProviderRepo) SetEnabled(ctx context.Context, channelName string, enabled bool) error {
	tag, err := r.pool.Exec(ctx, `UPDATE console_providers SET enabled = $1, updated_at = $2 WHERE channel_name = $3`, boolToInt(enabled), nowMs(), channelName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("Provider %q %w", channelName, ErrNotFound)
	}
	return nil
}

// Delete removes a provider (mirrors deleteConsoleProviderEntry).
func (r *ProviderRepo) Delete(ctx context.Context, channelName string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM console_providers WHERE channel_name = $1`, channelName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("Provider %q %w", channelName, ErrNotFound)
	}
	return nil
}

// Clear removes all providers.
func (r *ProviderRepo) Clear(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM console_providers`)
	return err
}

// --- helpers ---

func scanProvider(rows interface{ Scan(...interface{}) error }, p *schema.ConsoleProvider) error {
	return rows.Scan(
		&p.ChannelName, &p.ProviderUUID, &p.Type, &p.TargetBaseURL, &p.SystemPrompt,
		&p.ModelsJSON, &p.Priority, &p.AuthHeader, &p.AuthValue, &p.ExtraFieldsJSON,
		&p.RoutingVisibility, &p.Enabled, &p.CreatedAt, &p.UpdatedAt,
	)
}

func modelsJSON(models []map[string]interface{}) string {
	if len(models) == 0 {
		return "[]"
	}
	b, err := json.Marshal(models)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func extraFieldsJSON(fields map[string]interface{}) string {
	if len(fields) == 0 {
		return ""
	}
	b, err := json.Marshal(fields)
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetByChannel returns the row for channelName, or ErrNotFound if absent.
func (r *ProviderRepo) GetByChannel(ctx context.Context, channelName string) (schema.ConsoleProvider, error) {
	var p schema.ConsoleProvider
	err := r.pool.QueryRow(ctx, `
		SELECT channel_name, provider_uuid, type, target_base_url, system_prompt,
		       models_json, priority, auth_header, auth_value, extra_fields_json,
		       routing_visibility, enabled, created_at, updated_at
		FROM console_providers
		WHERE channel_name = $1
	`, channelName).Scan(
		&p.ChannelName, &p.ProviderUUID, &p.Type, &p.TargetBaseURL, &p.SystemPrompt,
		&p.ModelsJSON, &p.Priority, &p.AuthHeader, &p.AuthValue, &p.ExtraFieldsJSON,
		&p.RoutingVisibility, &p.Enabled, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return schema.ConsoleProvider{}, ErrNotFound
		}
		return schema.ConsoleProvider{}, err
	}
	return p, nil
}
