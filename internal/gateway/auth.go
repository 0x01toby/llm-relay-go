package gateway

import (
	"net/http"
	"strings"

	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/repo"
)

// AuthenticatedAPIKey is the result of a successful managed-key auth.
type AuthenticatedAPIKey struct {
	ID            string
	Name          string
	AllowedModels []string
	CostQuota     *int64
	CostUsed      int64
	QuotaExhausted bool
}

// GatewayAuthResult is the outcome of authenticating a request.
type GatewayAuthResult struct {
	OK        bool
	APIKey    *AuthenticatedAPIKey // nil when authenticated as admin
	IsAdmin   bool
	ErrorResponse func(http.ResponseWriter)
}

// credentialCandidate is a credential extracted from a request header.
type credentialCandidate struct {
	header     string // "x-api-key" or "authorization"
	credential string
}

// readGatewayCredentials extracts x-api-key and Bearer token candidates,
// de-duplicating. Mirrors readGatewayCredentials.
func readGatewayCredentials(h http.Header) []credentialCandidate {
	var out []credentialCandidate
	seen := map[string]bool{}

	if key := strings.TrimSpace(h.Get("x-api-key")); key != "" {
		out = append(out, credentialCandidate{header: "x-api-key", credential: key})
		seen[key] = true
	}

	auth := h.Get("authorization")
	var bearer string
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		bearer = strings.TrimSpace(auth[7:])
	}
	if bearer != "" && !seen[bearer] {
		out = append(out, credentialCandidate{header: "authorization", credential: bearer})
	}
	return out
}

// AuthenticateGateway validates the request's credentials against the admin
// gateway key and managed API keys. It enforces quota exhaustion and the
// model allowlist. Mirrors authenticateGateway.
//
//   - No credentials → 401
//   - Admin key match → OK (apiKey nil, isAdmin true)
//   - Managed key match → OK (apiKey set); quota/model checks applied
//   - No match → 401 (or 503 if admin key unconfigured)
func AuthenticateGateway(h http.Header, upstreamType configstore.UpstreamType, requestedModel string, adminKey string, keyRepo *repo.APIKeyRepo) GatewayAuthResult {
	candidates := readGatewayCredentials(h)
	if len(candidates) == 0 {
		return GatewayAuthResult{
			ErrorResponse: func(w http.ResponseWriter) {
				writeGatewayError(w, upstreamType, 401, "缺少 x-api-key 或 Authorization: Bearer token")
			},
		}
	}

	// Admin key.
	if adminKey != "" {
		for _, c := range candidates {
			if c.credential == adminKey {
				return GatewayAuthResult{OK: true, IsAdmin: true}
			}
		}
	}

	// Managed keys (only if a key repo is configured).
	for _, c := range candidates {
		if keyRepo == nil {
			break
		}
		row, ok := keyRepo.Authenticate(requestContext(), c.credential)
		if !ok {
			continue
		}
		snap := repo.BuildQuotaSnapshot(row.CostQuotaMicrousd, row.CostUsedMicrousd)
		apiKey := &AuthenticatedAPIKey{
			ID:            row.ID,
			Name:          row.Name,
			AllowedModels: repo.ParseAllowedModels(row.AllowedModelsJSON),
			CostQuota:     row.CostQuotaMicrousd,
			CostUsed:      row.CostUsedMicrousd,
			QuotaExhausted: snap.QuotaExhausted,
		}
		// Quota check.
		if apiKey.QuotaExhausted {
			quota := int64(0)
			if apiKey.CostQuota != nil {
				quota = *apiKey.CostQuota
			}
			detail := "已用 $" + ftoa(snap.CostUsed, 6) + " / 额度 $" + ftoa(float64(quota)/1e6, 6)
			ak := apiKey
			return GatewayAuthResult{
				OK:     true,
				APIKey: ak,
				ErrorResponse: func(w http.ResponseWriter) {
					writeGatewayError(w, upstreamType, 429, "此 API key 的费用额度已用完", detail)
				},
			}
		}
		// Model allowlist (checked against the pre-resolution client model).
		if len(apiKey.AllowedModels) > 0 {
			if requestedModel == "" || requestedModel == "unknown" {
				return GatewayAuthResult{
					OK:     true,
					APIKey: apiKey,
					ErrorResponse: func(w http.ResponseWriter) {
						writeGatewayError(w, upstreamType, 403, "无法确定请求模型，此 API key 配置了模型限制")
					},
				}
			}
			if !repo.IsModelAllowed(requestedModel, apiKey.AllowedModels) {
				ak := apiKey
				model := requestedModel
				return GatewayAuthResult{
					OK:     true,
					APIKey: ak,
					ErrorResponse: func(w http.ResponseWriter) {
						writeGatewayError(w, upstreamType, 403, "模型 '"+model+"' 不在此 API key 的允许列表中")
					},
				}
			}
		}
		return GatewayAuthResult{OK: true, APIKey: apiKey}
	}

	// No match.
	if adminKey == "" {
		return GatewayAuthResult{
			ErrorResponse: func(w http.ResponseWriter) {
				writeGatewayError(w, upstreamType, 503, "网关未配置管理员 key，且提供的凭证无效")
			},
		}
	}
	return GatewayAuthResult{
		ErrorResponse: func(w http.ResponseWriter) {
			writeGatewayError(w, upstreamType, 401, "网关认证失败")
		},
	}
}

// writeGatewayError emits a provider-shaped error response. Mirrors
// buildGatewayErrorResponse: OpenAI uses the {error:{message,type,code,param}}
// envelope; Anthropic uses {error, details?}.
func writeGatewayError(w http.ResponseWriter, upstreamType configstore.UpstreamType, status int, message string, details ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if upstreamType == configstore.OpenAI {
		errType := "api_error"
		switch status {
		case 401:
			errType = "authentication_error"
		case 429:
			errType = "rate_limit_error"
		}
		code := "null"
		if status == 429 {
			code = `"insufficient_quota"`
		}
		msg := message
		if len(details) > 0 {
			msg = message + ": " + details[0]
		}
		// Hand-built JSON to match the exact envelope shape.
		_, _ = w.Write([]byte(`{"error":{"message":` + jsonString(msg) + `,"type":"` + errType + `","code":` + code + `,"param":null}}`))
		return
	}
	body := `{"error":` + jsonString(message)
	if len(details) > 0 {
		body += `,"details":` + jsonString(details[0])
	}
	body += "}"
	_, _ = w.Write([]byte(body))
}

// jsonString quotes a string for JSON output.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString("\\u00")
				const hex = "0123456789abcdef"
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ftoa formats a float with fixed precision.
func ftoa(f float64, prec int) string {
	negative := f < 0
	if negative {
		f = -f
	}
	// Round to prec decimals.
	mult := 1.0
	for i := 0; i < prec; i++ {
		mult *= 10
	}
	rounded := int64(f*mult + 0.5)
	intPart := rounded / int64(mult)
	frac := rounded % int64(mult)
	s := itoa(int(intPart))
	if prec > 0 {
		fracStr := itoa(int(frac))
		for len(fracStr) < prec {
			fracStr = "0" + fracStr
		}
		s = s + "." + fracStr
	}
	if negative {
		s = "-" + s
	}
	return s
}
