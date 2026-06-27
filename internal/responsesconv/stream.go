package responsesconv

import (
	"io"
	"strings"
)

// This file ports the streaming Chat Completions SSE → Responses API SSE
// converter: a state machine that processes SSE blocks incrementally and emits
// the Responses-API event lifecycle:
//
//	response.created
//	  → (per output item) output_item.added, content_part.added,
//	    one+ output_text.delta / reasoning_text.delta,
//	    output_text.done / reasoning_text.done, content_part.done, output_item.done
//	  → (per tool call) output_item.added, output_item.done
//	response.completed
//	[DONE]
//
// Tool-call argument/name fragments are buffered across chunks and emitted
// only at finalize time (no incremental function_call_arguments deltas).
// Text deltas are run through the incremental <think>-tag parser so reasoning
// blocks split across SSE chunks are handled correctly.

type streamToolCall struct {
	id        string
	name      strings.Builder
	arguments strings.Builder
}

type streamOutputItemKind int

const (
	streamMessage streamOutputItemKind = iota
	streamReasoning
)

type streamOutputItem struct {
	kind        streamOutputItemKind
	outputIndex int
	id          string
	text        strings.Builder
	finalized   bool
}

type streamState struct {
	responseID    string
	model         string
	createdAt     float64
	created       bool
	finalized     bool
	finishReason  interface{}
	usage         interface{}
	toolCalls     map[int]*streamToolCall
	toolOrder     []int // preserves insertion order for finalize emission
	items         []*streamOutputItem
	activeItemIdx int
	active        bool
	thinkParser   *thinkTagParser
}

func newStreamState() *streamState {
	return &streamState{
		createdAt:     float64(0), // set on first chunk or finalized
		toolCalls:     map[int]*streamToolCall{},
		activeItemIdx: -1,
		thinkParser:   newThinkTagParser(),
	}
}

// emitter consumes the encoded SSE frame bytes. In production this writes to
// the http.ResponseWriter (with flushing); in tests it appends to a buffer.
type emitter interface {
	emit(frame string)
}

// emitBuffer is a simple emitter collecting frames into a string.
type emitBuffer struct{ b strings.Builder }

func (e *emitBuffer) emit(frame string) { e.b.WriteString(frame) }

func sseFrame(event string, payload string) string {
	return "event: " + event + "\ndata: " + payload + "\n\n"
}

func sseDoneFrame() string { return "data: [DONE]\n\n" }

func streamResponseSkeleton(st *streamState, status string, output []Obj) Obj {
	fs := statusFromFinishReason(st.finishReason)
	var incomplete interface{}
	if status == "incomplete" {
		incomplete = fs.incompleteDetails
	}
	var usage interface{}
	if status == "completed" || status == "incomplete" {
		usage = convertChatUsageToResponsesUsage(st.usage)
	}
	id := st.responseID
	if id == "" {
		id = toResponseID(nil)
	}
	return Obj{
		"id":                   id,
		"object":               "response",
		"created_at":           st.createdAt,
		"status":               status,
		"error":                nil,
		"incomplete_details":   incomplete,
		"model":                st.model,
		"output":               output,
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"store":                false,
		"usage":                usage,
	}
}

func responseContentPartForStreamItem(item *streamOutputItem) Obj {
	if item.kind == streamReasoning {
		return Obj{"type": "reasoning_text", "text": item.text.String()}
	}
	return Obj{"type": "output_text", "text": item.text.String(), "annotations": Arr{}}
}

func responseOutputItemForStream(item *streamOutputItem) Obj {
	if item.kind == streamReasoning {
		return Obj{
			"id":      item.id,
			"type":    "reasoning",
			"content": Arr{responseContentPartForStreamItem(item)},
			"summary": Arr{},
		}
	}
	return Obj{
		"id":      item.id,
		"type":    "message",
		"status":  "completed",
		"role":    "assistant",
		"content": Arr{responseContentPartForStreamItem(item)},
	}
}

func responseOutputItemAddedForStream(item *streamOutputItem) Obj {
	if item.kind == streamReasoning {
		return Obj{
			"id":      item.id,
			"type":    "reasoning",
			"content": Arr{},
			"summary": Arr{},
		}
	}
	return Obj{
		"id":      item.id,
		"type":    "message",
		"status":  "in_progress",
		"role":    "assistant",
		"content": Arr{},
	}
}

func ensureStreamCreated(em emitter, st *streamState, chunk Obj) {
	if st.responseID == "" {
		st.responseID = toResponseID(chunk["id"])
	}
	if st.model == "" {
		if m, ok := strField(chunk, "model"); ok {
			st.model = m
		}
	}
	if n, ok := numberField(chunk, "created"); ok {
		st.createdAt = n
	}
	if st.createdAt == 0 {
		// Use a real timestamp only when none arrived (mirrors Math.floor(Date.now()/1000)).
		st.createdAt = float64(0) // finalized path sets a real value if still 0
	}
	if st.created {
		return
	}
	st.created = true
	resp := streamResponseSkeleton(st, "in_progress", []Obj{})
	em.emit(sseFrame("response.created", jsonMustEncode(Obj{"type": "response.created", "response": resp})))
}

func activeStreamItem(st *streamState) *streamOutputItem {
	if !st.active || st.activeItemIdx < 0 || st.activeItemIdx >= len(st.items) {
		return nil
	}
	return st.items[st.activeItemIdx]
}

func finalizeActiveStreamItem(em emitter, st *streamState) {
	item := activeStreamItem(st)
	if item == nil || item.finalized {
		return
	}

	part := responseContentPartForStreamItem(item)
	doneEvent := "response.output_text.done"
	if item.kind == streamReasoning {
		doneEvent = "response.reasoning_text.done"
	}
	donePayload := Obj{
		"type":          doneEvent,
		"item_id":       item.id,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"text":          item.text.String(),
	}
	em.emit(sseFrame(doneEvent, jsonMustEncode(donePayload)))
	em.emit(sseFrame("response.content_part.done", jsonMustEncode(Obj{
		"type":          "response.content_part.done",
		"item_id":       item.id,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"part":          part,
	})))
	em.emit(sseFrame("response.output_item.done", jsonMustEncode(Obj{
		"type":         "response.output_item.done",
		"output_index": item.outputIndex,
		"item":         responseOutputItemForStream(item),
	})))

	item.finalized = true
	st.active = false
}

func ensureStreamItemStarted(em emitter, st *streamState, kind streamOutputItemKind) *streamOutputItem {
	ensureStreamCreated(em, st, Obj{})
	item := activeStreamItem(st)
	if item != nil && !item.finalized {
		if item.kind == kind {
			return item
		}
		finalizeActiveStreamItem(em, st)
	}

	outputIndex := len(st.items)
	newItem := &streamOutputItem{
		kind:        kind,
		outputIndex: outputIndex,
		id:          generatedItemID(prefixForKind(kind), st.responseID, outputIndex),
	}
	st.items = append(st.items, newItem)
	st.activeItemIdx = outputIndex
	st.active = true

	em.emit(sseFrame("response.output_item.added", jsonMustEncode(Obj{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item":         responseOutputItemAddedForStream(newItem),
	})))
	em.emit(sseFrame("response.content_part.added", jsonMustEncode(Obj{
		"type":          "response.content_part.added",
		"item_id":       newItem.id,
		"output_index":  outputIndex,
		"content_index": 0,
		"part":          responseContentPartForStreamItem(newItem),
	})))

	return newItem
}

func prefixForKind(kind streamOutputItemKind) string {
	if kind == streamReasoning {
		return "rs"
	}
	return "msg"
}

func appendStreamTextDelta(em emitter, st *streamState, seg thinkSegment) {
	if seg.text == "" {
		return
	}
	kind := streamMessage
	if seg.kind == thinkReasoning {
		kind = streamReasoning
	}
	item := ensureStreamItemStarted(em, st, kind)
	item.text.WriteString(seg.text)

	event := "response.output_text.delta"
	if kind == streamReasoning {
		event = "response.reasoning_text.delta"
	}
	em.emit(sseFrame(event, jsonMustEncode(Obj{
		"type":          event,
		"item_id":       item.id,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"delta":         seg.text,
	})))
}

func appendToolCallDelta(st *streamState, toolCallDelta interface{}) {
	arr := toArr(toolCallDelta)
	if arr == nil {
		return
	}
	for _, item := range arr {
		rec := toObj(item)
		if rec == nil {
			continue
		}
		var idx int
		if n, ok := numberField(rec, "index"); ok {
			idx = int(n)
		} else {
			idx = len(st.toolCalls)
		}
		existing, ok := st.toolCalls[idx]
		if !ok {
			existing = &streamToolCall{}
			st.toolCalls[idx] = existing
			st.toolOrder = append(st.toolOrder, idx)
		}
		if id, ok := strField(rec, "id"); ok {
			existing.id = id
		}
		fn := toObj(rec["function"])
		if fn == nil {
			fn = Obj{}
		}
		if name, ok := strField(fn, "name"); ok {
			existing.name.WriteString(name)
		}
		if args, ok := strField(fn, "arguments"); ok {
			existing.arguments.WriteString(args)
		}
	}
}

func processChatCompletionChunk(em emitter, st *streamState, chunk Obj) {
	ensureStreamCreated(em, st, chunk)
	if isRecord(chunk["usage"]) {
		st.usage = chunk["usage"]
	}

	choices := toArr(chunk["choices"])
	if choices == nil {
		return
	}
	for _, c := range choices {
		choice := toObj(c)
		if choice == nil {
			continue
		}
		if choice["finish_reason"] != nil {
			st.finishReason = choice["finish_reason"]
		}
		delta := toObj(choice["delta"])
		if delta == nil {
			delta = Obj{}
		}
		if tc := toArr(delta["tool_calls"]); tc != nil {
			appendToolCallDelta(st, tc)
		}
		contentDelta, ok := strField(delta, "content")
		if !ok || contentDelta == "" {
			// Kimi Coding emits text in reasoning_content and keeps content
			// null/empty. Surface it as output text for Responses clients.
			contentDelta, ok = strField(delta, "reasoning_content")
		}
		if !ok || contentDelta == "" {
			continue
		}
		for _, seg := range st.thinkParser.consume(contentDelta) {
			appendStreamTextDelta(em, st, seg)
		}
	}
}

func finalizeStream(em emitter, st *streamState) {
	if st.finalized {
		return
	}
	st.finalized = true
	ensureStreamCreated(em, st, Obj{})

	for _, seg := range st.thinkParser.flush() {
		appendStreamTextDelta(em, st, seg)
	}
	finalizeActiveStreamItem(em, st)

	output := make([]Obj, 0, len(st.items))
	for _, item := range st.items {
		output = append(output, responseOutputItemForStream(item))
	}

	// Append buffered tool calls as completed function_call items.
	funcCalls := functionCallItemsForStream(st, len(output))
	for offset, fc := range funcCalls {
		outputIndex := len(output) + offset
		em.emit(sseFrame("response.output_item.added", jsonMustEncode(Obj{
			"type":         "response.output_item.added",
			"output_index": outputIndex,
			"item":         fc,
		})))
		em.emit(sseFrame("response.output_item.done", jsonMustEncode(Obj{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item":         fc,
		})))
	}
	output = append(output, funcCalls...)

	fs := statusFromFinishReason(st.finishReason)
	resp := streamResponseSkeleton(st, fs.status, output)
	if text := collectOutputText(output); text != "" {
		resp["output_text"] = text
	}
	em.emit(sseFrame("response.completed", jsonMustEncode(Obj{"type": "response.completed", "response": resp})))
	em.emit(sseDoneFrame())
}

func functionCallItemsForStream(st *streamState, startIndex int) []Obj {
	var out []Obj
	for offset, idx := range st.toolOrder {
		call := st.toolCalls[idx]
		callID := call.id
		if callID == "" {
			callID = generatedItemID("call", st.responseID, idx)
		}
		out = append(out, Obj{
			"id":        generatedItemID("fc", st.responseID, startIndex+offset),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   callID,
			"name":      call.name.String(),
			"arguments": call.arguments.String(),
		})
	}
	return out
}

func processSseBlock(em emitter, st *streamState, block string) {
	data := extractData(block)
	if data == "" {
		return
	}
	if strings.TrimSpace(data) == "[DONE]" {
		finalizeStream(em, st)
		return
	}
	parsed, err := jsonDecode([]byte(data))
	if err != nil {
		// Resilient pass-through of malformed blocks.
		em.emit(block + "\n\n")
		return
	}
	if rec := toObj(parsed); rec != nil {
		processChatCompletionChunk(em, st, rec)
	}
}

// extractData is the SSE data-line extractor (delegates to the shared helper
// but trims the result the same way the TS code does).
func extractData(block string) string {
	return strings.TrimSpace(sseDataLines(block))
}

// sseDataLines mirrors sse.ExtractDataLines without an import cycle: joins
// multi-line "data:" payloads with "\n".
func sseDataLines(block string) string {
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

// ConvertStream transforms a Chat Completions SSE stream (r) into a Responses
// API SSE stream, writing frames to em. It processes blocks incrementally and
// always finalizes (even if the upstream never sent [DONE]).
func ConvertStream(em emitter, r io.Reader) {
	br := newBlockReader(r)
	st := newStreamState()

	for {
		block, ok, err := br.Next()
		if err != nil && err != io.EOF {
			// Non-recoverable read error: best-effort finalize then stop.
			finalizeStream(em, st)
			return
		}
		if ok {
			processSseBlock(em, st, block)
			continue
		}
		// ok == false → EOF. Flush any trailing buffered text then finalize.
		finalizeStream(em, st)
		return
	}
}

// newBlockReader is a tiny indirection so this package doesn't import sse
// (which would create a cycle if sse ever needed responsesconv). The concrete
// reader lives in stream_reader.go.
func newBlockReader(r io.Reader) blockReader { return newSSEBlockReader(r) }

// blockReader abstracts the minimal "next SSE block" surface we need.
type blockReader interface {
	Next() (block string, ok bool, err error)
}
