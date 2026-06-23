package responsesconv

import (
	"io"

	"github.com/taozhang/llmrelay/internal/sse"
)

// sseBlockReader adapts the shared sse.BlockReader to the blockReader
// interface used by the streaming converter. Keeping this in a separate file
// (and the interface in stream.go) means responsesconv only depends on sse
// through this thin shim, avoiding any chance of an import cycle.
type sseBlockReader struct {
	inner *sse.BlockReader
}

func newSSEBlockReader(r io.Reader) *sseBlockReader {
	return &sseBlockReader{inner: sse.NewBlockReader(r)}
}

func (b *sseBlockReader) Next() (string, bool, error) {
	return b.inner.Next()
}
