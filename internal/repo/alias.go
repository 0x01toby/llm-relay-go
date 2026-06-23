package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/taozhang/llmrelay/internal/schema"
)

// AliasRepo owns the model_aliases table.
type AliasRepo struct {
	db *gorm.DB
}

// NewAliasRepo builds an AliasRepo against gdb.
func NewAliasRepo(gdb *gorm.DB) *AliasRepo { return &AliasRepo{db: gdb} }

// AliasTarget is a single alias routing target.
type AliasTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// AliasInput is the mutation payload (mirrors ModelAliasMutationInput).
type AliasInput struct {
	Alias       string
	Provider    string
	Model       string
	Targets     []AliasTarget
	Description *string
	Visible     *bool
	Enabled     *bool
}

// List returns all aliases ordered by created_at.
func (r *AliasRepo) List(ctx context.Context) ([]schema.ModelAlias, error) {
	var rows []schema.ModelAlias
	if err := r.db.WithContext(ctx).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Get returns one alias by id, or ErrNotFound.
func (r *AliasRepo) Get(ctx context.Context, id int) (schema.ModelAlias, error) {
	var row schema.ModelAlias
	err := r.db.WithContext(ctx).First(&row, id).Error
	if err != nil {
		if isNoRows(err) {
			return schema.ModelAlias{}, ErrNotFound
		}
		return schema.ModelAlias{}, err
	}
	return row, nil
}

// Create inserts a new alias and returns the created row.
func (r *AliasRepo) Create(ctx context.Context, in AliasInput) (schema.ModelAlias, error) {
	row := buildAliasRow(in, 0)
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return schema.ModelAlias{}, err
	}
	return row, nil
}

// Update modifies an alias. Returns ErrNotFound if absent.
func (r *AliasRepo) Update(ctx context.Context, id int, in AliasInput) (schema.ModelAlias, error) {
	row := buildAliasRow(in, id)
	res := r.db.WithContext(ctx).Model(&schema.ModelAlias{}).Where("id = ?", id).Updates(map[string]interface{}{
		"alias":        row.Alias,
		"provider":     row.Provider,
		"model":        row.Model,
		"targets_json": row.TargetsJSON,
		"description":  row.Description,
		"visible":      row.Visible,
		"enabled":      row.Enabled,
		"updated_at":   row.UpdatedAt,
	})
	if res.Error != nil {
		return schema.ModelAlias{}, res.Error
	}
	if res.RowsAffected == 0 {
		return schema.ModelAlias{}, fmt.Errorf("alias %d %w", id, ErrNotFound)
	}
	// Reload to get the full row (created_at etc).
	return r.Get(ctx, id)
}

// SetEnabled toggles an alias's enabled flag.
func (r *AliasRepo) SetEnabled(ctx context.Context, id int, enabled bool) (schema.ModelAlias, error) {
	res := r.db.WithContext(ctx).Model(&schema.ModelAlias{}).Where("id = ?", id).
		Updates(map[string]interface{}{"enabled": boolToInt(enabled), "updated_at": nowMs()})
	if res.Error != nil {
		return schema.ModelAlias{}, res.Error
	}
	if res.RowsAffected == 0 {
		return schema.ModelAlias{}, fmt.Errorf("alias %d %w", id, ErrNotFound)
	}
	return r.Get(ctx, id)
}

// Delete removes an alias. Returns ErrNotFound if absent.
func (r *AliasRepo) Delete(ctx context.Context, id int) error {
	res := r.db.WithContext(ctx).Delete(&schema.ModelAlias{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("alias %d %w", id, ErrNotFound)
	}
	return nil
}

// buildAliasRow assembles a ModelAlias from input. When id is 0, Create will
// auto-generate it.
func buildAliasRow(in AliasInput, id int) schema.ModelAlias {
	visible := 1
	if in.Visible != nil && !*in.Visible {
		visible = 0
	}
	enabled := 1
	if in.Enabled != nil && !*in.Enabled {
		enabled = 0
	}
	return schema.ModelAlias{
		ID:          id,
		Alias:       in.Alias,
		Provider:    in.Provider,
		Model:       in.Model,
		TargetsJSON: targetsToJSON(in.Targets),
		Description: in.Description,
		Visible:     visible,
		Enabled:     enabled,
		CreatedAt:   nowMs(),
		UpdatedAt:   nowMs(),
	}
}

// targetsToJSON serializes targets; empty slice → "" (the column may be empty,
// callers fall back to the single {provider, model} pair via ParseTargets).
func targetsToJSON(targets []AliasTarget) string {
	if len(targets) == 0 {
		return ""
	}
	b, err := json.Marshal(targets)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseTargets decodes an alias's targets_json, falling back to the single
// {provider, model} pair when the column is empty.
func ParseTargets(a schema.ModelAlias) ([]AliasTarget, error) {
	if a.TargetsJSON != "" {
		var t []AliasTarget
		if err := json.Unmarshal([]byte(a.TargetsJSON), &t); err != nil {
			return nil, fmt.Errorf("invalid targets_json for alias %q: %w", a.Alias, err)
		}
		return t, nil
	}
	return []AliasTarget{{Provider: a.Provider, Model: a.Model}}, nil
}

// ErrEmptyTargets signals a targets array with zero entries.
var ErrEmptyTargets = errors.New("targets must have at least one entry")
