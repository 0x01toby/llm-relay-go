package responsesconv

import (
	"strings"
	"testing"
)

// These tests verify the <think>-tag parsers in isolation, focusing on the
// partial-tag holdback (longestTagSuffixPrefix) which is the subtlest logic
// in the whole converter.

func TestSplitThinkTaggedText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []thinkSegment
	}{
		{"plain", "hello", []thinkSegment{{thinkMessage, "hello"}}},
		{"single think", "<think>reasoning</think>answer", []thinkSegment{
			{thinkReasoning, "reasoning"},
			{thinkMessage, "answer"},
		}},
		{"leading think no trailing", "<think>only reasoning", []thinkSegment{
			{thinkReasoning, "only reasoning"},
		}},
		{"adjacent same-kind merged", "a<think></think>b", []thinkSegment{
			{thinkMessage, "ab"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitThinkTaggedText(tc.text)
			if !segmentsEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLongestTagSuffixPrefix(t *testing.T) {
	cases := []struct {
		value string
		tag   string
		want  int
	}{
		{"<thi", "<think>", 4},    // "<thi" is a prefix of "<think>"
		{"<think", "<think>", 6},  // full prefix minus the final '>'
		{"xy<th", "<think>", 3},   // "<th" at the tail
		{"hello", "<think>", 0},   // no overlap
		{"</th", "</think>", 4},   // close-tag prefix
		{"", "<think>", 0},
	}
	for _, tc := range cases {
		got := longestTagSuffixPrefix(tc.value, tc.tag)
		if got != tc.want {
			t.Errorf("longestTagSuffixPrefix(%q,%q) = %d, want %d", tc.value, tc.tag, got, tc.want)
		}
	}
}

// TestThinkParser_SplitAcrossChunks simulates "<think>plan</think>Hi" arriving
// in tiny pieces. The parser merges same-kind segments *within* a consume()
// call but not across calls (cross-call merging is the stream layer's job, via
// the active output item). So we assert on the concatenated text per kind and
// verify no partial tag is ever emitted as message text.
func TestThinkParser_SplitAcrossChunks(t *testing.T) {
	p := newThinkTagParser()
	chunks := []string{"<thi", "nk>pla", "n</thi", "nk>Hi"}
	var reasoningText, messageText string
	var leakedTag bool
	for _, chunk := range chunks {
		for _, seg := range p.consume(chunk) {
			switch seg.kind {
			case thinkReasoning:
				reasoningText += seg.text
			case thinkMessage:
				messageText += seg.text
				if strings.Contains(seg.text, "<") {
					leakedTag = true
				}
			}
		}
	}
	for _, seg := range p.flush() {
		switch seg.kind {
		case thinkReasoning:
			reasoningText += seg.text
		case thinkMessage:
			messageText += seg.text
		}
	}

	if leakedTag {
		t.Errorf("a partial '<' tag leaked into message text: %q", messageText)
	}
	if reasoningText != "plan" {
		t.Errorf("reasoning text: got %q, want %q", reasoningText, "plan")
	}
	if messageText != "Hi" {
		t.Errorf("message text: got %q, want %q", messageText, "Hi")
	}
}

// TestThinkParser_NoFalsePartial verifies that ordinary text not resembling a
// tag is emitted immediately (the holdback only retains genuine tag prefixes).
func TestThinkParser_NoFalsePartial(t *testing.T) {
	p := newThinkTagParser()
	got := p.consume("regular text with no tags")
	got = append(got, p.flush()...)
	want := []thinkSegment{{thinkMessage, "regular text with no tags"}}
	if !segmentsEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func segmentsEqual(a, b []thinkSegment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].kind != b[i].kind || a[i].text != b[i].text {
			return false
		}
	}
	return true
}
