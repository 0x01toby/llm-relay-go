// Package observer captures response data for the observability console
// without blocking the client-facing stream. It is the Go port of the
// response-observation logic in src/response-observer.ts.
//
// The mechanism:
//
//   - A TeeReader splits the upstream response body: one copy streams to the
//     client in real time, the other feeds an observation buffer.
//   - A background goroutine reads the observation copy to completion, then
//     parses usage (tokens, model, stop reason) and timing (first chunk, first
//     token, total duration), and hands the result to the caller via a channel.
//   - The caller (gateway Handler) feeds that result into logtasks, which
//     serializes the console_requests UPDATE after the INSERT.
//
// This keeps the hot path unblocked: the client's read cadence controls how
// fast we drain the upstream, and observation is purely best-effort.
package observer

import (
	"io"
	"strings"
	"sync"
	"time"

	"github.com/taozhang/llmrelay/internal/configstore"
	"github.com/taozhang/llmrelay/internal/logging"
	"github.com/taozhang/llmrelay/internal/providers"
)

// MaxCaptureBytes caps how much of the response body we buffer for logging.
// Matches PAYLOAD_LOG_LIMIT_BYTES (5 MiB). Responses larger than this are
// truncated (the truncation flag is recorded).
const MaxCaptureBytes = logging.PayloadLogLimitBytes

// Result is the observed response data, delivered once the body is consumed.
type Result struct {
	Body             string
	BodyBytes        int
	Truncated        bool
	TruncationReason logging.TruncationReason
	FirstChunkAt     *int64 // epoch ms
	FirstTokenAt     *int64
	CompletedAt      *int64
	HasStreaming     bool
	Usage            providers.UsageData
}

// Capturer tee-splits a response body so it can be forwarded to the client
// while a background goroutine accumulates it for observation. Construct with
// NewCapturer, hand ObserveBody() to the client reader, and read ObserveDone()
// to get the Result.
type Capturer struct {
	source      io.Reader
	upstreamType configstore.UpstreamType
	contentType string
	createdAt   int64

	// The pipe: writes come from the TeeReader as the client reads; the
	// observer goroutine drains the read end.
	pipeReader *io.PipeReader
	pipeWriter *io.PipeWriter

	doneOnce sync.Once
	doneCh   chan Result
}

// NewCapturer builds a Capturer for source (the upstream response body). The
// createdAt timestamp anchors timing measurements.
func NewCapturer(source io.Reader, upstreamType configstore.UpstreamType, contentType string, createdAt int64) *Capturer {
	pr, pw := io.Pipe()
	c := &Capturer{
		source:      source,
		upstreamType: upstreamType,
		contentType: strings.ToLower(contentType),
		createdAt:   createdAt,
		pipeReader:  pr,
		pipeWriter:  pw,
		doneCh:      make(chan Result, 1),
	}
	// Start the observer goroutine immediately. It blocks on pipeReader until
	// the TeeReader (driven by the client) feeds it data or Close signals EOF.
	go c.observe()
	return c
}

// ClientBody returns an io.Reader the gateway should stream to the client.
// Reading it drives both the client response and the observation pipe.
func (c *Capturer) ClientBody() io.Reader {
	return io.TeeReader(c.source, c.pipeWriter)
}

// Close signals that the client stream is done. The observer goroutine
// finishes draining and delivers its Result. Always call this (usually via
// defer) after the client body is fully read or errored.
func (c *Capturer) Close() {
	_ = c.pipeWriter.Close()
}

// ObserveDone returns a channel that receives the Result once observation
// completes. It is buffered (size 1) so a non-receiving caller never blocks.
func (c *Capturer) ObserveDone() <-chan Result {
	return c.doneCh
}

// observe drains the pipe, accumulates the body (up to the cap), and parses
// usage/timing. Runs in its own goroutine.
func (c *Capturer) observe() {
	res := Result{}
	isEventStream := strings.Contains(c.contentType, "text/event-stream")
	res.HasStreaming = isEventStream

	var buf strings.Builder
	buf.Grow(8192)
	chunkBuf := make([]byte, 4096)
	firstChunk := true
	captureFull := true // false once we hit MaxCaptureBytes; skip buf writes thereafter

	for {
		n, err := c.pipeReader.Read(chunkBuf)
		if n > 0 {
			now := time.Now().UnixMilli()
			if firstChunk {
				fc := now
				res.FirstChunkAt = &fc
				firstChunk = false
			}
			res.BodyBytes += n
			if captureFull && buf.Len() < MaxCaptureBytes {
				remaining := MaxCaptureBytes - buf.Len()
				if n > remaining {
					buf.Write(chunkBuf[:remaining])
					res.Truncated = true
					res.TruncationReason = logging.TruncSizeLimit
					captureFull = false // stop buffering; just track byte count
				} else {
					buf.Write(chunkBuf[:n])
				}
			}
			// First-token detection for SSE.
			if res.FirstTokenAt == nil && isEventStream {
				text := string(chunkBuf[:n])
				if providers.HasTextualSignal(text, providers.UpstreamType(c.upstreamType)) {
					ft := now
					res.FirstTokenAt = &ft
				}
			}
		}
		if err != nil {
			break
		}
	}

	res.Body = buf.String()
	completed := time.Now().UnixMilli()
	res.CompletedAt = &completed

	// Parse usage from the captured body (works for both JSON and SSE).
	res.Usage = providers.ParseUsage(res.Body, providers.UpstreamType(c.upstreamType))

	c.doneOnce.Do(func() {
		c.doneCh <- res
	})
}

// awaitResult is a convenience that blocks until the result is ready or
// timeout elapses. Used in tests; production reads the channel asynchronously.
func awaitResult(c *Capturer, timeout time.Duration) (Result, bool) {
	select {
	case r := <-c.ObserveDone():
		return r, true
	case <-time.After(timeout):
		return Result{}, false
	}
}
