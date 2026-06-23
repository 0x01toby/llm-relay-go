package responsesconv

import (
	"net/url"
	"strings"
)

// IsOpenAIResponsesEndpointPath mirrors isOpenAiResponsesEndpointPath: strict
// equality with /v1/responses.
func IsOpenAIResponsesEndpointPath(pathname string) bool {
	return pathname == "/v1/responses"
}

// RewriteResponsesTargetURLToChatCompletions mirrors the TS helper:
//   - path ending in /responses → replace suffix with /chat/completions
//   - path not ending in /chat/completions → append /chat/completions
//   - otherwise leave unchanged
func RewriteResponsesTargetURLToChatCompletions(targetURL string) string {
	u, err := url.Parse(targetURL)
	if err != nil {
		return targetURL
	}
	switch {
	case strings.HasSuffix(u.Path, "/responses"):
		u.Path = u.Path[:len(u.Path)-len("/responses")] + "/chat/completions"
	case strings.HasSuffix(u.Path, "/chat/completions"):
		// leave as-is
	default:
		u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	}
	return u.String()
}
