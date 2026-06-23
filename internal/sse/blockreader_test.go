package sse

import (
	"io"
	"strings"
	"testing"
)

func collectBlocks(t *testing.T, input string) []string {
	t.Helper()
	br := NewBlockReader(strings.NewReader(input))
	var blocks []string
	for {
		block, ok, err := br.Next()
		if err != nil && err != io.EOF {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			break
		}
		blocks = append(blocks, block)
	}
	return blocks
}

func TestBlockReader_SplitsOnDoubleNewline(t *testing.T) {
	input := "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n"
	got := collectBlocks(t, input)
	want := []string{"event: a\ndata: 1", "event: b\ndata: 2"}
	if len(got) != len(want) {
		t.Fatalf("got %d blocks %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBlockReader_HandlesCRLF(t *testing.T) {
	input := "event: a\r\ndata: 1\r\n\r\nevent: b\r\ndata: 2\r\n\r\n"
	got := collectBlocks(t, input)
	want := []string{"event: a\r\ndata: 1", "event: b\r\ndata: 2"}
	if len(got) != len(want) {
		t.Fatalf("got %d blocks %v, want %d", len(got), got, len(want))
	}
}

func TestBlockReader_TrailingBlockWithoutBlankLine(t *testing.T) {
	// The real upstream sometimes omits a final blank line.
	input := "data: hello"
	got := collectBlocks(t, input)
	if len(got) != 1 || got[0] != "data: hello" {
		t.Fatalf("got %v", got)
	}
}

func TestBlockReader_EmptyInput(t *testing.T) {
	got := collectBlocks(t, "")
	if len(got) != 0 {
		t.Fatalf("expected no blocks, got %v", got)
	}
}

func TestBlockReader_CarriesPartialAcrossReads(t *testing.T) {
	// Simulate chunk boundaries by feeding the stream in small pieces.
	full := "data: part1\n\ndata: part2\n\n"
	br := NewBlockReader(&pieceReader{data: full, size: 3})
	var blocks []string
	for {
		block, ok, err := br.Next()
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		blocks = append(blocks, block)
	}
	want := []string{"data: part1", "data: part2"}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks %v, want %d", len(blocks), blocks, len(want))
	}
}

func TestExtractDataLines(t *testing.T) {
	block := "event: message\ndata: {\"a\":1}\ndata: cont"
	got := ExtractDataLines(block)
	want := "{\"a\":1}\ncont"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIsDone(t *testing.T) {
	if !IsDone("[DONE]") {
		t.Error("[DONE] should be terminal")
	}
	if !IsDone(" [DONE] ") {
		t.Error("[DONE] with whitespace should be terminal")
	}
	if IsDone("[DON]") {
		t.Error("[DON] should not be terminal")
	}
}

// pieceReader emits data in fixed-size chunks to simulate network framing.
type pieceReader struct {
	data string
	pos  int
	size int
}

func (p *pieceReader) Read(buf []byte) (int, error) {
	if p.pos >= len(p.data) {
		return 0, io.EOF
	}
	end := p.pos + p.size
	if end > len(p.data) {
		end = len(p.data)
	}
	n := copy(buf, p.data[p.pos:end])
	p.pos += n
	return n, nil
}
