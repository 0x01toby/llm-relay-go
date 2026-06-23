package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSpike_EndToEnd_Streaming runs the full pipeline: a fake Chat Completions
// upstream that emits an SSE stream, proxied through the spike handler which
// converts Responses→Chat (request) and Chat→Responses (streaming response).
// This is the spike's acceptance test — it proves the converter is wired
// correctly into an HTTP server end to end, including incremental flushing.
func TestSpike_EndToEnd_Streaming(t *testing.T) {
	// Fake upstream: echoes a two-chunk "Hello" stream with a <think> block.
	upstreamSSE := strings.Join([]string{
		`data: {"id":"chatcmpl_e2e","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_e2e","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"<think>reason"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_e2e","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"ing</think>Hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_e2e","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var gotRequest string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotRequest = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		// Emit chunks with a small delay to exercise incremental flushing.
		for _, line := range strings.SplitAfter(upstreamSSE, "\n") {
			_, _ = w.Write([]byte(line))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	handler, err := New(SpikeConfig{
		UpstreamBaseURL:    upstream.URL,
		UpstreamAuthHeader: "Authorization",
		UpstreamAuthValue:  "Bearer test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Client request in Responses-API format.
	reqBody := `{"model":"gpt-test","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Errorf("content-type: got %q", resp.Header.Get("Content-Type"))
	}

	// Verify the request was converted to Chat Completions: should have
	// "messages" and "stream":true, and NOT have "input".
	if !strings.Contains(gotRequest, `"messages"`) {
		t.Errorf("forwarded request missing messages: %s", gotRequest)
	}
	if !strings.Contains(gotRequest, `"stream":true`) {
		t.Errorf("forwarded request missing stream:true: %s", gotRequest)
	}
	if strings.Contains(gotRequest, `"input"`) {
		t.Errorf("forwarded request should not contain 'input': %s", gotRequest)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	// The <think> tags must be split into reasoning/message events, never leaked.
	if !strings.Contains(text, "event: response.reasoning_text.delta") {
		t.Error("missing reasoning_text.delta")
	}
	if !strings.Contains(text, "event: response.output_text.delta") {
		t.Error("missing output_text.delta")
	}
	if strings.Contains(text, "<think>") || strings.Contains(text, "</think>") {
		t.Errorf("think tags leaked:\n%s", text)
	}
	if !strings.Contains(text, "event: response.completed") {
		t.Error("missing response.completed")
	}
	if !strings.Contains(text, `"output_text":"Hello"`) {
		t.Errorf("missing output_text Hello in:\n%s", text)
	}
	// Reasoning reassembly: the two chunks "reason"+"ing" must be joined. This
	// shows up in the reasoning_text.done event's "text":"reasoning" and in the
	// output_item.done event's reasoning_text part.
	if !strings.Contains(text, `"text":"reasoning"`) {
		t.Errorf("missing reassembled reasoning 'reasoning' in:\n%s", text)
	}
}

// TestSpike_EndToEnd_NonStreaming verifies the buffered-JSON path through the
// proxy.
func TestSpike_EndToEnd_NonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"chatcmpl_n","created":1,"model":"gpt-test","choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer upstream.Close()

	handler, _ := New(SpikeConfig{UpstreamBaseURL: upstream.URL})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content-type: %q", resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"object":"response"`) {
		t.Errorf("unexpected body: %s", body)
	}
	if !strings.Contains(string(body), `"output_text":"Done"`) {
		t.Errorf("missing output_text: %s", body)
	}
}

// Ensure the server doesn't hang on a slow client (regression guard for the
// streaming copy loop).
func TestSpike_DoesNotBlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
			flusher.Flush()
		}))
		defer upstream.Close()
		handler, _ := New(SpikeConfig{UpstreamBaseURL: upstream.URL})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"x","stream":true}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy blocked for >3s")
	}
}
