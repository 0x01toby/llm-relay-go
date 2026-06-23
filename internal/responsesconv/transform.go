package responsesconv

import (
	"io"
	"net/http"
	"strings"
)

// This file ports transformChatCompletionsResponseToResponses: given an
// upstream Chat Completions *http.Response, produce a new *http.Response whose
// body has been converted to the Responses API format (streaming SSE or
// buffered JSON). Headers are adjusted (content-type set, content-length and
// content-encoding stripped).

func isEventStream(headers http.Header) bool {
	ct := headers.Get("Content-Type")
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// transformedHeaders copies source headers, drops content-length and
// content-encoding, and overrides content-type.
func transformedHeaders(source http.Header, contentType string) http.Header {
	out := source.Clone()
	out.Del("Content-Length")
	out.Del("Content-Encoding")
	out.Set("Content-Type", contentType)
	return out
}

// TransformResponse takes an upstream Chat Completions response and returns a
// response whose body is converted to Responses-API format. The caller is
// responsible for writing the returned response to the client and closing the
// original body when done.
//
// If the upstream response is not OK or has no body, it is returned unchanged.
func TransformResponse(resp *http.Response) *http.Response {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Body == nil {
		return resp
	}

	if isEventStream(resp.Header) {
		pr, pw := io.Pipe()
		go func() {
			ConvertStream(pipeEmitter{pw}, resp.Body)
			_ = resp.Body.Close()
			_ = pw.Close()
		}()
		return &http.Response{
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
			Proto:      resp.Proto,
			ProtoMajor: resp.ProtoMajor,
			ProtoMinor: resp.ProtoMinor,
			Header:     transformedHeaders(resp.Header, "text/event-stream; charset=utf-8"),
			Body:       pr,
		}
	}

	// Non-streaming: buffer fully, convert, re-wrap. We read everything now so
	// the caller can close the original body immediately.
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	body := string(data)
	out := body
	if err == nil {
		if converted, convErr := ConvertChatCompletionToResponsePayload(decodeLoose(body)); convErr == nil {
			out = converted
		}
	}

	return &http.Response{
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		Proto:      resp.Proto,
		ProtoMajor: resp.ProtoMajor,
		ProtoMinor: resp.ProtoMinor,
		Header:     transformedHeaders(resp.Header, "application/json"),
		Body:       io.NopCloser(strings.NewReader(out)),
		ContentLength: int64(len(out)),
	}
}

// decodeLoose parses JSON tolerantly; returns the raw value (which may not be
// an object) so ConvertChatCompletionToResponsePayload can reject non-objects.
func decodeLoose(body string) interface{} {
	v, err := jsonDecode([]byte(body))
	if err != nil {
		return body
	}
	return v
}

// pipeEmitter writes SSE frames to an io.Writer (the pipe's write end).
type pipeEmitter struct{ w io.WriteCloser }

func (e pipeEmitter) emit(frame string) {
	_, _ = io.WriteString(e.w, frame)
}
