// Package sse provides primitives for parsing and emitting Server-Sent Events.
//
// The BlockReader splits an upstream byte stream into SSE event blocks on blank
// lines (\n\n or \r\n\r\n boundaries), carrying a partial block across read
// calls. This is the foundation for all streaming logic: usage parsing,
// <think>-tag splitting, and the Responses-API converter.
package sse

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// BlockReader yields complete SSE event blocks from an underlying reader.
// A block is the text between two blank-line boundaries. The final trailing
// bytes (with no closing blank line) are returned once the source is exhausted.
type BlockReader struct {
	r       *bufio.Reader
	buffer  strings.Builder
	done    bool
	eofSeen bool
}

// NewBlockReader wraps r into a BlockReader.
func NewBlockReader(r io.Reader) *BlockReader {
	return &BlockReader{r: bufio.NewReader(r)}
}

// Next returns the next complete SSE block and true, or "" / false at EOF.
// When the stream ends with non-empty trailing bytes (no closing blank line),
// those are returned as a final block followed by a true EOF on the next call.
func (b *BlockReader) Next() (string, bool, error) {
	if b.done {
		return "", false, nil
	}

	for {
		chunk, err := b.r.ReadSlice('\n')
		// Guard against an upstream sending a single SSE line longer than
		// the buffer (e.g. a huge JSON data payload with no newline). Without
		// this, ErrBufferFull would cause the buffer to grow without bound.
		if err == bufio.ErrBufferFull {
			b.done = true
			return "", false, fmt.Errorf("SSE line exceeds %d-byte buffer limit", b.r.Size())
		}
		if len(chunk) > 0 {
			b.buffer.Write(chunk)
		}

		// Detect a block boundary: a line that is empty after stripping a
		// trailing newline. ReadSlice includes the '\n'.
		if len(chunk) > 0 {
			line := strings.TrimRight(string(chunk), "\r\n")
			if line == "" {
				block := b.buffer.String()
				b.buffer.Reset()
				block = strings.TrimSuffix(block, "\n")
				block = strings.TrimSuffix(block, "\r")
				block = strings.TrimSuffix(block, "\n")
				return block, true, nil
			}
		}

		if err != nil {
			if err == io.EOF {
				b.eofSeen = true
				remainder := b.buffer.String()
				b.buffer.Reset()
				remainder = strings.TrimRight(remainder, "\r\n")
				if remainder != "" {
					return remainder, true, nil
				}
				b.done = true
				return "", false, nil
			}
			// For other errors, surface any buffered remainder then the error.
			if b.buffer.Len() > 0 {
				block := strings.TrimRight(b.buffer.String(), "\r\n")
				b.buffer.Reset()
				if block != "" {
					return block, true, nil
				}
			}
			b.done = true
			return "", false, err
		}
	}
}

// ExtractDataLines pulls the concatenated data payload out of an SSE block.
// Lines beginning with "data:" have that prefix stripped; one optional leading
// space after the colon is ignored per the SSE spec. Multi-line data values are
// joined with "\n". Returns "" if there are no data lines.
func ExtractDataLines(block string) string {
	var parts []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data := line[len("data:"):]
			if strings.HasPrefix(data, " ") {
				data = data[1:]
			}
			parts = append(parts, data)
		}
	}
	return strings.Join(parts, "\n")
}

// IsDone reports whether a data payload is the SSE terminal sentinel "[DONE]".
func IsDone(data string) bool {
	return strings.TrimSpace(data) == "[DONE]"
}
