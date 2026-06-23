package responsesconv

import "strings"

// This file is a direct port of the <think>-tag splitting logic from
// openai-responses-chat-compat.ts. It splits assistant text into alternating
// "message" (visible) and "reasoning" (<think>...</think>) segments.
//
// There are two forms:
//   - splitThinkTaggedText: one-shot, for non-streaming responses.
//   - thinkTagParser (stateful): incremental, for streaming responses. It
//     holds back a partial tag prefix at the end of each chunk so that a tag
//     split across chunks (e.g. "<thi" then "nk>") is not emitted as text.

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

type thinkSegmentKind int

const (
	thinkMessage thinkSegmentKind = iota
	thinkReasoning
)

type thinkSegment struct {
	kind thinkSegmentKind
	text string
}

// pushThinkSegment appends text to the last segment if it has the same kind,
// otherwise starts a new segment. Empty text is ignored.
func pushThinkSegment(segments *[]thinkSegment, kind thinkSegmentKind, text string) {
	if text == "" {
		return
	}
	n := len(*segments)
	if n > 0 && (*segments)[n-1].kind == kind {
		(*segments)[n-1].text += text
		return
	}
	*segments = append(*segments, thinkSegment{kind: kind, text: text})
}

func kindForThink(inThink bool) thinkSegmentKind {
	if inThink {
		return thinkReasoning
	}
	return thinkMessage
}

// splitThinkTaggedText performs a one-shot split of text into segments. Used
// for non-streaming responses where the full content is available at once.
func splitThinkTaggedText(text string) []thinkSegment {
	var segments []thinkSegment
	cursor := 0
	inThink := false

	for cursor < len(text) {
		tag := thinkOpenTag
		if inThink {
			tag = thinkCloseTag
		}
		idx := strings.Index(text[cursor:], tag)
		if idx == -1 {
			pushThinkSegment(&segments, kindForThink(inThink), text[cursor:])
			break
		}
		pushThinkSegment(&segments, kindForThink(inThink), text[cursor:cursor+idx])
		cursor = cursor + idx + len(tag)
		inThink = !inThink
	}

	return segments
}

// longestTagSuffixPrefix returns the largest L in (0, len(tag)) such that value
// ends with the first L characters of tag. This determines how much of the
// buffer could be the start of a (possibly incomplete) tag and must be held
// back rather than emitted as safe text.
//
// Ported verbatim from the TS longestTagSuffixPrefix.
func longestTagSuffixPrefix(value, tag string) int {
	maxLength := len(value)
	if len(tag)-1 < maxLength {
		maxLength = len(tag) - 1
	}
	for length := maxLength; length > 0; length-- {
		if strings.HasSuffix(value, tag[:length]) {
			return length
		}
	}
	return 0
}

// thinkTagParser is the incremental <think>-tag state machine for streams.
type thinkTagParser struct {
	buffer  string
	inThink bool
}

func newThinkTagParser() *thinkTagParser {
	return &thinkTagParser{}
}

// consume appends a chunk to the buffer and emits any complete segments,
// holding back a partial tag prefix at the tail.
func (p *thinkTagParser) consume(chunk string) []thinkSegment {
	var segments []thinkSegment
	p.buffer += chunk

	for len(p.buffer) > 0 {
		tag := thinkOpenTag
		if p.inThink {
			tag = thinkCloseTag
		}
		idx := strings.Index(p.buffer, tag)
		if idx != -1 {
			pushThinkSegment(&segments, kindForThink(p.inThink), p.buffer[:idx])
			p.buffer = p.buffer[idx+len(tag):]
			p.inThink = !p.inThink
			continue
		}

		held := longestTagSuffixPrefix(p.buffer, tag)
		safe := p.buffer[:len(p.buffer)-held]
		pushThinkSegment(&segments, kindForThink(p.inThink), safe)
		p.buffer = p.buffer[len(p.buffer)-held:]
		break
	}

	return segments
}

// flush emits whatever remains in the buffer under the current state. Called
// once when the stream ends.
func (p *thinkTagParser) flush() []thinkSegment {
	var segments []thinkSegment
	pushThinkSegment(&segments, kindForThink(p.inThink), p.buffer)
	p.buffer = ""
	return segments
}
