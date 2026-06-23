// Package gateway implements the LLM relay proxy engine: authentication,
// routing, upstream forwarding with failover/retry, timeouts, and response
// observation. It composes the configstore (routing), repo (auth/settings),
// responsesconv (protocol conversion), and providers (usage parsing) packages
// into the request pipeline that powers the proxy catch-all.
package gateway

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/taozhang/llmrelay/internal/repo"
	"github.com/taozhang/llmrelay/internal/schema"
)

// settingsCacheTTL is how long a cached settings view is considered fresh.
// Mirrors SETTINGS_CACHE_TTL_MS (5s) in the original.
const settingsCacheTTL = 5 * time.Second

// warnInterval rate-limits the "DB read failed, using fallback" warning to
// once per minute (mirrors SETTINGS_WARNING_INTERVAL_MS).
const warnInterval = 60 * time.Second

// TimeoutSettings is the resolved upstream-timeout configuration.
type TimeoutSettings struct {
	DefaultFirstByteMs int64
	StreamFirstByteMs  int64
	ImageFirstByteMs   int64
	ResponseIdleMs     int64
}

// timeoutCache is a TTL cache over the gateway.timeouts settings row. It
// mirrors the original's getGatewayTimeoutSettings: serve from cache if fresh,
// else read the DB; on DB error, serve stale cache if present else code
// defaults, warning at most once per minute.
type timeoutCache struct {
	repo      *repo.SettingsRepo
	defaults  TimeoutSettings
	mu        sync.Mutex
	cached    *TimeoutSettings
	updatedAt int64
	loadedAt  time.Time
	lastWarn  time.Time
}

func newTimeoutCache(r *repo.SettingsRepo, defaults TimeoutSettings) *timeoutCache {
	return &timeoutCache{repo: r, defaults: defaults}
}

func (c *timeoutCache) Get(ctx context.Context) TimeoutSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && time.Since(c.loadedAt) < settingsCacheTTL {
		return *c.cached
	}
	raw, updated, err := c.repo.Get(ctx, schema.SettingsKeyTimeouts)
	if err != nil {
		c.warnFallback(err)
		if c.cached != nil {
			return *c.cached
		}
		return c.defaults
	}
	ts := parseTimeouts(raw, c.defaults)
	c.cached = &ts
	c.updatedAt = updated
	c.loadedAt = time.Now()
	return ts
}

func (c *timeoutCache) warnFallback(err error) {
	if time.Since(c.lastWarn) < warnInterval {
		return
	}
	c.lastWarn = time.Now()
	log.Printf("[GATEWAY_TIMEOUT_SETTINGS_FALLBACK] %v", err)
}

func parseTimeouts(raw string, defaults TimeoutSettings) TimeoutSettings {
	ts := defaults
	if raw == "" || raw == "{}" {
		return ts
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ts
	}
	if v, ok := numField(m, "defaultFirstByteTimeoutMs"); ok {
		ts.DefaultFirstByteMs = v
	}
	if v, ok := numField(m, "streamFirstByteTimeoutMs"); ok {
		ts.StreamFirstByteMs = v
	}
	if v, ok := numField(m, "imageFirstByteTimeoutMs"); ok {
		ts.ImageFirstByteMs = v
	}
	if v, ok := numField(m, "responseIdleTimeoutMs"); ok {
		ts.ResponseIdleMs = v
	}
	return ts
}

// SelectFirstByteTimeout picks the first-byte timeout for a request based on
// whether it's an image endpoint or a streaming request. Mirrors
// selectUpstreamFirstByteTimeoutMs.
func SelectFirstByteTimeout(pathname, targetURL string, ts TimeoutSettings, isStreaming bool) int64 {
	if isImageRequestPath(pathname, targetURL) {
		return ts.ImageFirstByteMs
	}
	if isStreaming {
		return ts.StreamFirstByteMs
	}
	return ts.DefaultFirstByteMs
}

// isImageRequestPath reports whether the request targets an image-generation
// endpoint (OpenAI images API). Mirrors isImageRequestPath.
func isImageRequestPath(pathname, targetURL string) bool {
	candidates := []string{pathname}
	if targetURL != "" {
		if u, err := parseURL(targetURL); err == nil {
			candidates = append(candidates, u.Path)
		} else {
			candidates = append(candidates, targetURL)
		}
	}
	for _, c := range candidates {
		low := toLower(c)
		if contains(low, "/images/generations") || contains(low, "/images/edits") || contains(low, "/images/variations") {
			return true
		}
	}
	return false
}
