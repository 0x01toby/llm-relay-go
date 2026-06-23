// Package catalog fetches and caches model metadata (context windows and
// pricing) from models.dev. It is a port of src/model-catalog.ts +
// src/catalog-db.ts + the fetch half of src/pricing.ts.
//
// Three-layer cache with singleflight (mirrors the original):
//   - In-memory maps (contextWindow, pricing) under a RWMutex, 24h TTL.
//   - DB cache (model_catalog_cache) warmed at boot, persisted on network fetch.
//   - Network fetch from https://models.dev/api.json (10s timeout), deduped
//     via singleflight so concurrent callers share one fetch.
package catalog

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/taozhang/llmrelay/internal/pricing"
	"github.com/taozhang/llmrelay/internal/schema"
)

const (
	modelsDevURL  = "https://models.dev/api.json"
	fetchTimeout  = 10 * time.Second
	cacheTTL      = 24 * time.Hour
	batchSize     = 500
)

// Service owns the context/pricing caches and the models.dev fetcher.
type Service struct {
	db         *gorm.DB
	httpClient *http.Client

	mu             sync.RWMutex
	contextCache   map[string]int
	pricingCache   map[string]*pricing.ModelPricing
	loadedAt       time.Time
	networkFetched time.Time
	group          singleflight.Group
}

// New builds a catalog Service.
func New(gdb *gorm.DB) *Service {
	return &Service{
		db:         gdb,
		httpClient: &http.Client{Timeout: fetchTimeout},
	}
}

// WarmFromDB loads the catalog from the model_catalog_cache table. Returns true
// if the DB data is fresh (< 24h), so the caller can skip the network fetch.
func (s *Service) WarmFromDB(ctx context.Context) (bool, error) {
	var rows []schema.ModelCatalogCache
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return false, err
	}

	ctxCache := map[string]int{}
	priceCache := map[string]*pricing.ModelPricing{}
	var maxFetched int64
	for _, row := range rows {
		if row.ContextWindow != nil {
			ctxCache[row.ModelID] = *row.ContextWindow
		}
		if row.PricingJSON != nil && *row.PricingJSON != "" {
			if p := parsePricing(*row.PricingJSON); p != nil {
				priceCache[row.ModelID] = p
			}
		}
		if row.FetchedAt > maxFetched {
			maxFetched = row.FetchedAt
		}
	}

	s.mu.Lock()
	s.contextCache = ctxCache
	s.pricingCache = priceCache
	s.loadedAt = time.Now()
	s.mu.Unlock()

	fresh := maxFetched > 0 && time.Since(time.UnixMilli(maxFetched)) < cacheTTL
	return fresh, nil
}

// LookupContext returns the context window for a model, or 0 if unknown.
func (s *Service) LookupContext(modelID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contextCache[modelID]
}

// LookupPricing returns the pricing for a model, or nil if unknown.
func (s *Service) LookupPricing(modelID string) *pricing.ModelPricing {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pricingCache[modelID]
}

// IsFresh reports whether the in-memory cache is within its TTL.
func (s *Service) IsFresh() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contextCache != nil && time.Since(s.loadedAt) < cacheTTL
}

// EnsureLoaded refreshes from the network if the cache is stale or empty.
// Concurrent callers share one fetch via singleflight.
func (s *Service) EnsureLoaded(ctx context.Context) error {
	if s.IsFresh() {
		return nil
	}
	_, err, _ := s.group.Do("fetch", func() (interface{}, error) {
		if s.IsFresh() {
			return nil, nil
		}
		return nil, s.refreshFromNetwork(ctx)
	})
	return err
}

// refreshFromNetwork fetches models.dev and updates both caches + DB.
func (s *Service) refreshFromNetwork(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.onFetchFailure()
		return err
	}
	defer resp.Body.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		s.onFetchFailure()
		return err
	}

	ctxCache := map[string]int{}
	priceCache := map[string]*pricing.ModelPricing{}
	for modelID, data := range raw {
		var m struct {
			Context *int `json:"limit"`
			Pricing struct {
				Input      *float64 `json:"prompt"`
				Output     *float64 `json:"completion"`
				CacheRead  *float64 `json:"cachedPrompt"`
				CacheWrite *float64 `json:"writeCache"`
			} `json:"pricing"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Context != nil {
			ctxCache[modelID] = *m.Context
		}
		p := &pricing.ModelPricing{
			Input: m.Pricing.Input, Output: m.Pricing.Output,
			CacheRead: m.Pricing.CacheRead, CacheWrite: m.Pricing.CacheWrite,
		}
		if p.Input != nil || p.Output != nil {
			priceCache[modelID] = p
		}
	}

	s.mu.Lock()
	s.contextCache = ctxCache
	s.pricingCache = priceCache
	s.loadedAt = time.Now()
	s.networkFetched = time.Now()
	s.mu.Unlock()

	// Persist to DB (best-effort, non-blocking).
	go s.persistToDB(ctxCache, priceCache)
	log.Printf("[catalog] refreshed from models.dev: %d context + %d pricing entries", len(ctxCache), len(priceCache))
	return nil
}

// onFetchFailure sets an empty cache so subsequent calls don't immediately
// retry within the TTL (mirrors refreshFromNetwork's failure handling).
func (s *Service) onFetchFailure() {
	s.mu.Lock()
	if s.contextCache == nil {
		s.contextCache = map[string]int{}
	}
	s.loadedAt = time.Now()
	s.mu.Unlock()
}

// persistToDB upserts the cache into model_catalog_cache in batches.
func (s *Service) persistToDB(ctxCache map[string]int, priceCache map[string]*pricing.ModelPricing) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Union of model IDs from both maps.
	ids := map[string]bool{}
	for id := range ctxCache {
		ids[id] = true
	}
	for id := range priceCache {
		ids[id] = true
	}

	now := time.Now().UnixMilli()
	for id := range ids {
		ctxW := ctxCache[id]
		var pricingStr *string
		if p := priceCache[id]; p != nil {
			b, _ := json.Marshal(p)
			str := string(b)
			pricingStr = &str
		}
		var ctxPtr *int
		if ctxW > 0 {
			ctxPtr = &ctxW
		}
		row := schema.ModelCatalogCache{
			ModelID:       id,
			ContextWindow: ctxPtr,
			PricingJSON:   pricingStr,
			FetchedAt:     now,
		}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "model_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"context_window", "pricing_json", "fetched_at"}),
		}).Create(&row).Error; err != nil {
			log.Printf("[catalog] persist error for %s: %v", id, err)
		}
	}
}

// parsePricing decodes a pricing_json value into a ModelPricing.
func parsePricing(s string) *pricing.ModelPricing {
	// model_catalog_cache stores the models.dev pricing sub-object; tolerate
	// either the flat {prompt, completion, ...} shape or a wrapped object.
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	p := &pricing.ModelPricing{}
	p.Input = numField(m, "prompt", "input")
	p.Output = numField(m, "completion", "output")
	p.CacheRead = numField(m, "cachedPrompt", "cache_read")
	p.CacheWrite = numField(m, "writeCache", "cache_write")
	if p.Input == nil && p.Output == nil {
		return nil
	}
	return p
}

func numField(m map[string]json.RawMessage, keys ...string) *float64 {
	for _, k := range keys {
		if raw, ok := m[k]; ok {
			var f float64
			if err := json.Unmarshal(raw, &f); err == nil {
				return &f
			}
		}
	}
	return nil
}
