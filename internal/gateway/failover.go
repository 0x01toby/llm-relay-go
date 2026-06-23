package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/taozhang/llmrelay/internal/repo"
)

// ModelFallbackMode controls how failover picks secondary candidates.
type ModelFallbackMode string

const (
	FallbackDisabled ModelFallbackMode = "disabled"
	FallbackSameModel ModelFallbackMode = "same_model"
	FallbackAnyModel  ModelFallbackMode = "any_model"
)

// CustomModelFallbackRule maps a requested model to fallback model specifiers.
type CustomModelFallbackRule struct {
	Model     string   `json:"model"`
	Fallbacks []string `json:"fallbacks"`
}

// FailoverPolicy is the resolved failover configuration. Mirrors
// GatewayFailoverPolicy.
type FailoverPolicy struct {
	Enabled              bool                      `json:"enabled"`
	RetryAttempts        int                       `json:"retryAttempts"`
	ModelFallbackMode    ModelFallbackMode         `json:"modelFallbackMode"`
	MaxFallbackAttempts  int                       `json:"maxFallbackAttempts"`
	CustomModelFallbacks []CustomModelFallbackRule `json:"customModelFallbacks"`
	RetryOnTimeout       bool                      `json:"retryOnTimeout"`
	RetryOnNetworkError  bool                      `json:"retryOnNetworkError"`
	RetryOnStatusCodes   []int                     `json:"retryOnStatusCodes"`
	RetryOnStatusRanges  []string                  `json:"retryOnStatusRanges"`
}

// CodeDefaultFailoverPolicy mirrors CODE_DEFAULT_GATEWAY_FAILOVER_POLICY.
func CodeDefaultFailoverPolicy() FailoverPolicy {
	return FailoverPolicy{
		Enabled:             true,
		RetryAttempts:       1,
		ModelFallbackMode:   FallbackSameModel,
		MaxFallbackAttempts: 2,
		RetryOnTimeout:      true,
		RetryOnNetworkError: true,
		RetryOnStatusCodes:  []int{408, 429},
		RetryOnStatusRanges: []string{"5xx"},
	}
}

// FailoverTriggerKind identifies why a failover is being considered.
type FailoverTriggerKind int

const (
	TriggerTimeout FailoverTriggerKind = iota
	TriggerNetworkError
	TriggerStatus
)

// FailoverTrigger describes a failure that may warrant retry/failover.
type FailoverTrigger struct {
	Kind   FailoverTriggerKind
	Status int // valid when Kind == TriggerStatus
}

// DescribeTrigger returns a human-readable reason. Mirrors describeFailoverTrigger.
func DescribeTrigger(t FailoverTrigger) string {
	switch t.Kind {
	case TriggerTimeout:
		return "timeout"
	case TriggerNetworkError:
		return "network_error"
	}
	return "http_" + itoa(t.Status)
}

// ShouldTriggerFailover reports whether the policy says to retry/failover on
// this trigger. Mirrors shouldTriggerFailover.
func ShouldTriggerFailover(p FailoverPolicy, t FailoverTrigger) bool {
	if !p.Enabled {
		return false
	}
	switch t.Kind {
	case TriggerTimeout:
		return p.RetryOnTimeout
	case TriggerNetworkError:
		return p.RetryOnNetworkError
	case TriggerStatus:
		for _, code := range p.RetryOnStatusCodes {
			if code == t.Status {
				return true
			}
		}
		for _, r := range p.RetryOnStatusRanges {
			if r == "5xx" && t.Status >= 500 && t.Status <= 599 {
				return true
			}
		}
	}
	return false
}

// CustomFallbackModels returns the fallback specifiers for requestedModel, if a
// custom rule matches. Mirrors getCustomModelFallbackModels.
func CustomFallbackModels(p FailoverPolicy, requestedModel string) []string {
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		return nil
	}
	for _, rule := range p.CustomModelFallbacks {
		if rule.Model == model {
			return rule.Fallbacks
		}
	}
	return nil
}

// failoverCache is a TTL cache over the gateway.failover settings row.
type failoverCache struct {
	repo      *repo.SettingsRepo
	defaults  FailoverPolicy
	mu        sync.Mutex
	cached    *FailoverPolicy
	loadedAt  time.Time
	lastWarn  time.Time
}

func newFailoverCache(r *repo.SettingsRepo) *failoverCache {
	return &failoverCache{repo: r, defaults: CodeDefaultFailoverPolicy()}
}

func (c *failoverCache) Get(ctx context.Context) FailoverPolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && time.Since(c.loadedAt) < settingsCacheTTL {
		return *c.cached
	}
	raw, _, err := c.repo.Get(ctx, "gateway.failover")
	if err != nil {
		c.warnFallback(err)
		if c.cached != nil {
			return *c.cached
		}
		return c.defaults
	}
	p := parseFailoverPolicy(raw, c.defaults)
	c.cached = &p
	c.loadedAt = time.Now()
	return p
}

func (c *failoverCache) warnFallback(err error) {
	if time.Since(c.lastWarn) < warnInterval {
		return
	}
	c.lastWarn = time.Now()
	log.Printf("[GATEWAY_FAILOVER_POLICY_FALLBACK] %v", err)
}

func parseFailoverPolicy(raw string, defaults FailoverPolicy) FailoverPolicy {
	p := defaults
	if raw == "" || raw == "{}" {
		return p
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return p
	}
	if v, ok := m["enabled"]; ok {
		if b, ok := v.(bool); ok {
			p.Enabled = b
		}
	}
	if v, ok := numField(m, "retryAttempts"); ok {
		p.RetryAttempts = int(v)
	}
	if v, ok := m["modelFallbackMode"]; ok {
		if s, ok := v.(string); ok {
			p.ModelFallbackMode = ModelFallbackMode(s)
		}
	}
	if v, ok := numField(m, "maxFallbackAttempts"); ok {
		p.MaxFallbackAttempts = int(v)
	}
	if v, ok := m["retryOnTimeout"]; ok {
		if b, ok := v.(bool); ok {
			p.RetryOnTimeout = b
		}
	}
	if v, ok := m["retryOnNetworkError"]; ok {
		if b, ok := v.(bool); ok {
			p.RetryOnNetworkError = b
		}
	}
	if v, ok := m["retryOnStatusCodes"]; ok {
		if arr, ok := v.([]interface{}); ok {
			codes := make([]int, 0, len(arr))
			for _, c := range arr {
				if f, ok := c.(float64); ok {
					codes = append(codes, int(f))
				}
			}
			p.RetryOnStatusCodes = codes
		}
	}
	if v, ok := m["retryOnStatusRanges"]; ok {
		if arr, ok := v.([]interface{}); ok {
			ranges := make([]string, 0, len(arr))
			for _, r := range arr {
				if s, ok := r.(string); ok {
					ranges = append(ranges, s)
				}
			}
			p.RetryOnStatusRanges = ranges
		}
	}
	if v, ok := m["customModelFallbacks"]; ok {
		if arr, ok := v.([]interface{}); ok {
			rules := make([]CustomModelFallbackRule, 0, len(arr))
			for _, r := range arr {
				if obj, ok := r.(map[string]interface{}); ok {
					rule := CustomModelFallbackRule{}
					if m, ok := obj["model"].(string); ok {
						rule.Model = m
					}
					if fbs, ok := obj["fallbacks"].([]interface{}); ok {
						for _, fb := range fbs {
							if s, ok := fb.(string); ok {
								rule.Fallbacks = append(rule.Fallbacks, s)
							}
						}
					}
					rules = append(rules, rule)
				}
			}
			p.CustomModelFallbacks = rules
		}
	}
	return p
}

// --- small helpers shared by settings + failover ---

func numField(m map[string]interface{}, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

func parseURL(s string) (*url.URL, error) { return url.Parse(s) }
func toLower(s string) string             { return strings.ToLower(s) }
func contains(s, sub string) bool         { return strings.Contains(s, sub) }

// itoa is a tiny strconv.Itoa alias kept local to avoid pulling strconv into
// every file that references failover triggers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
