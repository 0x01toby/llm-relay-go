// Package catalog loads and caches model metadata (context windows and pricing)
// from models.dev.
//
// The catalog is held entirely in memory — there is no DB persistence layer.
// At boot the vendored models.dev catalog (models-dev.json, go:embed) is parsed
// into in-memory maps so pricing is available with zero external dependencies.
// A background refresh then pulls the live catalog from
// https://models.dev/api.json (10s timeout, 24h TTL) and updates the maps;
// concurrent refreshes are deduped via singleflight.
//
// All lookups (LookupContext / LookupPricing / PricingMap) are lock-free reads
// against the in-memory maps, so the cost column on every request list/detail
// is computed without a DB hit.
//
// models.dev's JSON is keyed by provider at the top level, with each provider
// exposing its models under a nested "models" object. The same model id (e.g.
// "claude-opus-4-5") is offered by several providers; we flatten to a single
// per-model-id entry, preferring the first-party provider (anthropic/openai)
// whose pricing is the most complete and authoritative.
package catalog

import (
	"context"
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/taozhang/llmrelay/internal/pricing"
)

const (
	modelsDevURL = "https://models.dev/api.json"
	fetchTimeout = 10 * time.Second
	cacheTTL     = 24 * time.Hour
)

//go:embed models-dev.json
var vendoredCatalogFS embed.FS

// vendoredCatalog is the lazily-decoded models-dev.json embedded in the binary.
var vendoredCatalog []byte

func init() {
	b, err := vendoredCatalogFS.ReadFile("models-dev.json")
	if err != nil {
		log.Printf("[catalog] could not read vendored models-dev.json: %v", err)
		return
	}
	vendoredCatalog = b
}

// Service owns the in-memory context/pricing caches and the models.dev fetcher.
type Service struct {
	httpClient *http.Client

	mu             sync.RWMutex
	contextCache   map[string]int
	pricingCache   map[string]*pricing.ModelPricing
	loadedAt       time.Time
	networkFetched time.Time
	group          singleflight.Group
}

// New builds an in-memory catalog Service. Pricing is populated by WarmFromEmbed
// (and optionally EnsureLoaded); lookups return nil/0 until then.
func New() *Service {
	return &Service{
		httpClient: &http.Client{Timeout: fetchTimeout},
	}
}

// WarmFromEmbed parses the vendored models-dev.json baked into the binary into
// the in-memory caches. This is the boot path: it gives the gateway pricing
// immediately, with no DB or network dependency.
func (s *Service) WarmFromEmbed() error {
	return s.loadFromBytes(vendoredCatalog, false)
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

// PricingMap returns a snapshot copy of the whole pricing cache keyed by model
// id (each value is the canonical pricing object the dashboard renders). Used
// by the Models page to enrich every configured model in one pass.
func (s *Service) PricingMap() map[string]map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]map[string]interface{}, len(s.pricingCache))
	for id, p := range s.pricingCache {
		out[id] = canonicalPricingMap(p)
	}
	return out
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

// refreshFromNetwork fetches models.dev and updates the in-memory caches.
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

	// Flatten the provider-keyed object into a model-keyed object (first-party
	// providers winning), then load it into memory.
	flat := flattenModelsDev(raw)
	b, _ := json.Marshal(flat)
	return s.loadFromBytes(b, true)
}

// loadFromBytes parses a catalog object (either provider-keyed raw or
// model-keyed flattened) into the in-memory caches. When fromNetwork is true
// the fetch timestamp is stamped.
func (s *Service) loadFromBytes(data []byte, fromNetwork bool) error {
	ctxCache, priceCache, err := parseModelsDev(data)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.contextCache = ctxCache
	s.pricingCache = priceCache
	s.loadedAt = time.Now()
	if fromNetwork {
		s.networkFetched = time.Now()
	}
	s.mu.Unlock()

	label := "vendored fallback"
	if fromNetwork {
		label = "network refresh"
	}
	log.Printf("[catalog] loaded (%s): %d context + %d pricing entries", label, len(ctxCache), len(priceCache))
	return nil
}

// onFetchFailure leaves the existing cache intact but stamps loadedAt so a
// transient network blip doesn't trigger a retry storm within the TTL.
func (s *Service) onFetchFailure() {
	s.mu.Lock()
	if s.contextCache == nil {
		s.contextCache = map[string]int{}
	}
	s.loadedAt = time.Now()
	s.mu.Unlock()
}

// canonicalPricingMap renders a ModelPricing as the canonical object the
// dashboard renders: {"input":..,"output":..,"cache_read":..,"cache_write":..}.
func canonicalPricingMap(p *pricing.ModelPricing) map[string]interface{} {
	out := map[string]interface{}{}
	if p.Input != nil {
		out["input"] = *p.Input
	}
	if p.Output != nil {
		out["output"] = *p.Output
	}
	if p.CacheRead != nil {
		out["cache_read"] = *p.CacheRead
	}
	if p.CacheWrite != nil {
		out["cache_write"] = *p.CacheWrite
	}
	return out
}
