//go:build integration

// Integration tests for the repository layer, run against each configured
// database dialect (postgres / mysql / sqlite). Enabled dialects come from env:
//
//	TEST_DATABASE_URL=postgresql://...  (postgres)
//	TEST_MYSQL_URL=mysql://...          (mysql)
//	TEST_SQLITE_URL=sqlite:///path.db   (sqlite; defaults to in-memory)
//
// Run with:
//
//	go test ./internal/repo/ -tags integration -v
//	go test -p 1 ./internal/repo/ ./internal/consoleapi/ -tags integration
//
// (Use -p 1 when multiple packages share the same test DB so resets don't race.)
package repo

import (
	"context"
	"testing"

	"github.com/taozhang/llmrelay/internal/migrate"
	"github.com/taozhang/llmrelay/internal/schema"
	"github.com/taozhang/llmrelay/internal/testutil"
)

// runPerDialect runs fn against each configured dialect. Each fn gets a fresh DB.
func runPerDialect(t *testing.T, fn func(t *testing.T, url string)) {
	t.Helper()
	for _, c := range testutil.DialectURLs() {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			fn(t, c.URL)
		})
	}
}

func TestIntegration_Migrations_CreateAllTables(t *testing.T) {
	runPerDialect(t, func(t *testing.T, url string) {
		gdb := testutil.FreshDB(t, url)
		var tableCount int64
		err := gdb.Raw(`SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('information_schema','pg_catalog','mysql','sys','INFORMATION_SCHEMA')`).Scan(&tableCount).Error
		if err != nil {
			// SQLite has no information_schema — count via sqlite_master.
			err = gdb.Raw(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '%_seq'`).Scan(&tableCount).Error
		}
		if err != nil {
			t.Fatalf("count tables: %v", err)
		}
		// 6 application tables (console_requests, console_api_keys,
		// console_providers, model_aliases, model_catalog_cache,
		// model_metadata_overrides, gateway_settings = 7).
		if tableCount < 6 {
			t.Errorf("expected >= 6 tables, got %d", tableCount)
		}
	})
}

func TestIntegration_ProviderCRUD(t *testing.T) {
	runPerDialect(t, func(t *testing.T, url string) {
		gdb := testutil.FreshDB(t, url)
		r := NewProviderRepo(gdb)
		ctx := context.Background()

		sp := func(s string) *string { return &s }
		if err := r.Create(ctx, "ch1", ProviderInput{
			Type: "openai", TargetBaseURL: "https://api.openai.com/v1",
			Models: []map[string]interface{}{{"model": "gpt-4o"}},
		}); err != nil {
			t.Fatal(err)
		}

		list, err := r.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].ChannelName != "ch1" {
			t.Fatalf("list: %+v", list)
		}
		if list[0].ProviderUUID == "" {
			t.Error("provider_uuid should be backfilled")
		}
		if list[0].ModelsJSON != `[{"model":"gpt-4o"}]` {
			t.Errorf("models json: %q", list[0].ModelsJSON)
		}

		// Update (rename + change).
		if err := r.Update(ctx, "ch1", "ch2", ProviderInput{
			Type: "anthropic", TargetBaseURL: "https://api.anthropic.com",
			SystemPrompt: sp("be nice"),
		}); err != nil {
			t.Fatal(err)
		}
		list, _ = r.List(ctx)
		if len(list) != 1 || list[0].ChannelName != "ch2" || list[0].Type != "anthropic" {
			t.Fatalf("after update: %+v", list)
		}

		// Toggle off.
		if err := r.SetEnabled(ctx, "ch2", false); err != nil {
			t.Fatal(err)
		}
		list, _ = r.List(ctx)
		if list[0].Enabled != 0 {
			t.Error("should be disabled")
		}

		// Delete.
		if err := r.Delete(ctx, "ch2"); err != nil {
			t.Fatal(err)
		}
		if err := r.Delete(ctx, "ch2"); err == nil {
			t.Error("expected ErrNotFound on second delete")
		}
	})
}

func TestIntegration_AliasCRUD(t *testing.T) {
	runPerDialect(t, func(t *testing.T, url string) {
		gdb := testutil.FreshDB(t, url)
		r := NewAliasRepo(gdb)
		ctx := context.Background()

		a, err := r.Create(ctx, AliasInput{
			Alias: "fast", Provider: "ch", Model: "gpt-4o",
			Targets: []AliasTarget{{Provider: "ch", Model: "gpt-4o"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if a.Alias != "fast" {
			t.Errorf("alias: %q", a.Alias)
		}

		list, _ := r.List(ctx)
		if len(list) != 1 {
			t.Fatalf("list len: %d", len(list))
		}
		targets, err := ParseTargets(list[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) != 1 || targets[0].Model != "gpt-4o" {
			t.Errorf("targets: %+v", targets)
		}

		if _, err := r.SetEnabled(ctx, a.ID, false); err != nil {
			t.Fatal(err)
		}
		got, _ := r.Get(ctx, a.ID)
		if got.Enabled != 0 {
			t.Error("should be disabled")
		}

		if err := r.Delete(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIntegration_APIKeyCRUD(t *testing.T) {
	runPerDialect(t, func(t *testing.T, url string) {
		gdb := testutil.FreshDB(t, url)
		r := NewAPIKeyRepo(gdb)
		ctx := context.Background()

		created, err := r.Create(ctx, "my-key", nil)
		if err != nil {
			t.Fatal(err)
		}
		if created.RawKey == "" {
			t.Error("raw key should be returned")
		}

		// Authenticate by the raw key.
		auth, ok := r.Authenticate(ctx, created.RawKey)
		if !ok {
			t.Fatal("authenticate failed")
		}
		if auth.ID != created.Row.ID {
			t.Errorf("auth id mismatch: %q vs %q", auth.ID, created.Row.ID)
		}
		// Second auth observes the last_used_at written by the first.
		auth2, _ := r.Authenticate(ctx, created.RawKey)
		if auth2.LastUsedAt == nil {
			t.Error("last_used_at should be set after a prior auth")
		}

		if _, ok := r.Authenticate(ctx, "wrong"); ok {
			t.Error("wrong key should not authenticate")
		}

		updated, err := r.SetAllowedModels(ctx, created.Row.ID, []string{"gpt-4o", "claude", "gpt-4o"})
		if err != nil {
			t.Fatal(err)
		}
		models := ParseAllowedModels(updated.AllowedModelsJSON)
		if len(models) != 2 || models[0] != "gpt-4o" {
			t.Errorf("allowed models: %+v", models)
		}

		quota := int64(1_000_000)
		if _, err := r.SetCostQuota(ctx, created.Row.ID, &quota); err != nil {
			t.Fatal(err)
		}

		if _, err := r.Rename(ctx, created.Row.ID, "renamed"); err != nil {
			t.Fatal(err)
		}
		if err := r.Delete(ctx, created.Row.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIntegration_SettingsKV(t *testing.T) {
	runPerDialect(t, func(t *testing.T, url string) {
		gdb := testutil.FreshDB(t, url)
		r := NewSettingsRepo(gdb)
		ctx := context.Background()

		if _, _, err := r.Get(ctx, "gateway.timeouts"); err == nil {
			t.Error("absent key should error")
		}

		ts, err := r.Upsert(ctx, "gateway.timeouts", `{"default":300000}`)
		if err != nil {
			t.Fatal(err)
		}
		if ts == 0 {
			t.Error("updated_at should be non-zero")
		}
		val, _, err := r.Get(ctx, "gateway.timeouts")
		if err != nil {
			t.Fatal(err)
		}
		if val != `{"default":300000}` {
			t.Errorf("value: %q", val)
		}

		r.Upsert(ctx, "gateway.timeouts", `{"default":120000}`)
		val, _, _ = r.Get(ctx, "gateway.timeouts")
		if val != `{"default":120000}` {
			t.Errorf("after replace: %q", val)
		}
	})
}

func TestIntegration_ResetDB(t *testing.T) {
	runPerDialect(t, func(t *testing.T, url string) {
		ctx := context.Background()
		gdb := testutil.FreshDB(t, url)
		r := NewProviderRepo(gdb)
		r.Create(ctx, "temp", ProviderInput{Type: "openai", TargetBaseURL: "https://x"})
		if sqlDB, e := gdb.DB(); e == nil {
			_ = sqlDB.Close()
		}

		// Reset wipes everything.
		if err := migrate.ResetDB(ctx, url); err != nil {
			t.Fatal(err)
		}
		gdb2 := testutil.FreshDB(t, url)
		var n int64
		gdb2.Model(&schema.ConsoleProvider{}).Count(&n)
		if n != 0 {
			t.Errorf("after reset, providers should be empty, got %d", n)
		}
	})
}
