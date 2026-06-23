// Package logging centralizes the byte/duration limits used when capturing
// request and response payloads for the observability console. Ported from
// src/logging-constants.ts.
package logging

import "github.com/taozhang/llmrelay/internal/config"

// PayloadLogLimitBytes is the cap on a single logged payload (request body,
// forwarded body, or response body). Payloads larger than this are truncated.
const PayloadLogLimitBytes = config.PayloadLogLimitBytes

// ResponseStreamLogMaxBytes is the byte cap for an observed streaming response.
const ResponseStreamLogMaxBytes = config.DefaultResponseStreamLogMaxBytes

// ResponseStreamLogMaxDurationMs returns the runtime-configured cap on how
// long a streaming response is observed for logging purposes.
func ResponseStreamLogMaxDurationMs() int64 { return config.ResponseStreamLogMaxDurationMs() }

// MinResponseStreamLogMaxDurationMs is the floor for the streaming-log duration.
const MinResponseStreamLogMaxDurationMs = config.MinResponseStreamLogMaxDurMs

// TruncationReason describes why a captured payload was cut short. Mirrors the
// TS TruncationReason type ("size_limit" | "duration_limit" | null).
type TruncationReason string

const (
	TruncSizeLimit     TruncationReason = "size_limit"
	TruncDurationLimit TruncationReason = "duration_limit"
)
