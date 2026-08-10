package pxpipe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func upstreamEcho(t *testing.T, gotBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = b
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
}

func newTestHandler(t *testing.T, upstream string, opts *TransformOptions) http.Handler {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(HandlerOptions{AnthropicUpstream: u, Transform: opts})
}

func TestHandlerTransformsMessagesRoute(t *testing.T) {
	var upstreamBody []byte
	up := upstreamEcho(t, &upstreamBody)
	defer up.Close()

	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var applied *TransformResult
	u, _ := url.Parse(up.URL)
	h := NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		OnResult:          func(_ *http.Request, res *TransformResult) { applied = res },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if applied == nil || !applied.Applied || applied.Reason != ReasonApplied {
		t.Fatalf("expected applied transform, got %+v", applied)
	}
	if !bytes.Contains(upstreamBody, []byte(`"type":"image"`)) {
		t.Error("upstream body has no image blocks")
	}
	if bytes.Equal(upstreamBody, input) {
		t.Error("upstream body was not transformed")
	}
}

func TestHandlerForwardsBillingLineAsHeader(t *testing.T) {
	const line = "x-anthropic-billing-header: cc_version=2.1.222; cc_prev_req=req_123"
	var gotHeader string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Anthropic-Billing-Header")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	srv := httptest.NewServer(newTestHandler(t, up.URL, nil))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"model":    "claude-fable-5",
		"system":   line + "\n" + strings.Repeat("stable system text ", 2_000),
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotHeader != "cc_version=2.1.222; cc_prev_req=req_123" {
		t.Fatalf("billing header = %q", gotHeader)
	}
	if bytes.Contains(gotBody, []byte(line)) {
		t.Fatal("billing line remained in the upstream body")
	}
}

func TestTransformAnthropicMessagesMarkerCount(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := TransformAnthropicMessages(TransformInput{Body: input, Model: "claude-fable-5"})
	if !result.Applied || !result.Info.cacheControlMarkersKnown {
		t.Fatalf("expected applied transform with parsed marker count: %+v", result)
	}
	if want := CountCacheControlMarkers(result.Body); result.Cache.MarkerCount != want {
		t.Fatalf("marker count = %d, want %d", result.Cache.MarkerCount, want)
	}
	if result.Cache.OwnsCacheControl != (result.Cache.MarkerCount > 0) {
		t.Fatalf("cache ownership = %v for %d markers", result.Cache.OwnsCacheControl, result.Cache.MarkerCount)
	}

	disabled := false
	result = TransformAnthropicMessages(TransformInput{
		Body: input, Model: "claude-fable-5", Options: &TransformOptions{Compress: &disabled},
	})
	if result.Info.cacheControlMarkersKnown {
		t.Fatal("compress=false result unexpectedly used parsed marker count")
	}
	if want := CountCacheControlMarkers(input); result.Cache.MarkerCount != want {
		t.Fatalf("fallback marker count = %d, want %d", result.Cache.MarkerCount, want)
	}
}

func TestHandlerPassesThroughOtherRoutes(t *testing.T) {
	var upstreamBody []byte
	up := upstreamEcho(t, &upstreamBody)
	defer up.Close()
	srv := httptest.NewServer(newTestHandler(t, up.URL, nil))
	defer srv.Close()

	payload := []byte(`{"model":"claude-fable-5","messages":[]}`)
	resp, err := http.Post(srv.URL+"/v1/messages/count_tokens", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !bytes.Equal(upstreamBody, payload) {
		t.Errorf("count_tokens body must pass through untouched: %s", upstreamBody)
	}
}

func TestHandlerUnsupportedModelPassesThrough(t *testing.T) {
	var upstreamBody []byte
	up := upstreamEcho(t, &upstreamBody)
	defer up.Close()
	srv := httptest.NewServer(newTestHandler(t, up.URL, nil))
	defer srv.Close()

	payload := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !bytes.Equal(upstreamBody, payload) {
		t.Errorf("unsupported model must pass through untouched: %s", upstreamBody)
	}
}

func TestExtractModelRejectsOversizedString(t *testing.T) {
	accepted := strings.Repeat("🙂", 100)
	if got := extractModel([]byte(`{"model":"` + accepted + `"}`)); got != accepted {
		t.Fatalf("200-code-unit model = %q", got)
	}
	rejected := strings.Repeat("🙂", 101)
	if got := extractModel([]byte(`{"model":"` + rejected + `"}`)); got != "" {
		t.Fatalf("202-code-unit model = %q, want empty", got)
	}
}

func TestHandlerNonClaudeMessagesPassThrough(t *testing.T) {
	SetAllowedModelBases([]string{"gpt-5.4"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })

	var upstreamBody []byte
	up := upstreamEcho(t, &upstreamBody)
	defer up.Close()
	u, _ := url.Parse(up.URL)
	var result *TransformResult
	srv := httptest.NewServer(NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		OnResult: func(_ *http.Request, got *TransformResult) {
			result = got
		},
	}))
	defer srv.Close()

	payload := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if result == nil || result.Reason != ReasonUnsupportedModel || result.Detail != "gpt-5.4" {
		t.Fatalf("non-Claude Messages result = %+v", result)
	}
	if !bytes.Equal(upstreamBody, payload) {
		t.Fatal("non-Claude Messages body was modified")
	}
}

func TestHandlerTransformFuncRunsPerRequestAndOverridesStatic(t *testing.T) {
	var upstreamBody []byte
	up := upstreamEcho(t, &upstreamBody)
	defer up.Close()
	u, _ := url.Parse(up.URL)
	enabled, disabled := true, false
	var calls atomic.Int32
	results := make(chan *TransformResult, 2)
	srv := httptest.NewServer(NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		Transform:         &TransformOptions{Compress: &enabled},
		TransformFunc: func() *TransformOptions {
			calls.Add(1)
			return &TransformOptions{Compress: &disabled}
		},
		OnResult: func(_ *http.Request, result *TransformResult) { results <- result },
	}))
	defer srv.Close()

	payload := []byte(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}]}`)
	for range 2 {
		resp, err := http.Post(srv.URL+"/v1/messages", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if result := <-results; result.Reason != ReasonCompressDisabled {
			t.Fatalf("dynamic transform result = %+v", result)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("TransformFunc calls = %d, want 2", calls.Load())
	}
	if !bytes.Equal(upstreamBody, payload) {
		t.Fatal("dynamic compress=false body was modified")
	}
}

func TestHandlerReturnsPinCommandsLocally(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		body        string
		contentType string
		contains    string
	}{
		{
			name:        "Anthropic Messages",
			path:        "/v1/messages",
			body:        `{"model":"claude-test","messages":[{"role":"user","content":"@pxpipe pin concise"}]}`,
			contentType: "application/json",
			contains:    `"type":"message"`,
		},
		{
			name:        "OpenAI Chat",
			path:        "/v1/chat/completions",
			body:        `{"model":"gpt-test","messages":[{"role":"user","content":"@pxpipe pin concise"}]}`,
			contentType: "application/json",
			contains:    `"object":"chat.completion"`,
		},
		{
			name:        "OpenAI Responses stream",
			path:        "/v1/responses",
			body:        `{"model":"gpt-test","stream":true,"input":[{"role":"user","content":"@pxpipe pin concise"}]}`,
			contentType: "text/event-stream",
			contains:    "event: response.completed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				fmt.Fprint(w, `{"upstream":true}`)
			}))
			defer up.Close()
			u, _ := url.Parse(up.URL)
			var onResultCalls atomic.Int32
			srv := httptest.NewServer(NewHandler(HandlerOptions{
				AnthropicUpstream: u,
				OpenAIUpstream:    u,
				OnResult: func(_ *http.Request, _ *TransformResult) {
					onResultCalls.Add(1)
				},
			}))
			defer srv.Close()

			resp, err := http.Post(srv.URL+tt.path, "application/json", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != tt.contentType ||
				resp.Header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("response = %d content-type=%q cache-control=%q", resp.StatusCode,
					resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
			}
			if !bytes.Contains(body, []byte(tt.contains)) {
				t.Fatalf("local reply missing %q: %s", tt.contains, body)
			}
			if upstreamCalls.Load() != 0 || onResultCalls.Load() != 0 {
				t.Fatalf("local reply calls: upstream=%d OnResult=%d", upstreamCalls.Load(), onResultCalls.Load())
			}
		})
	}
}

func TestHandlerStreamsSSE(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "event: chunk\ndata: {\"i\":%d}\n\n", i)
			fl.Flush()
		}
	}))
	defer up.Close()
	srv := httptest.NewServer(newTestHandler(t, up.URL, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	events := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: chunk") {
			events++
		}
	}
	if events != 3 {
		t.Errorf("got %d events, want 3", events)
	}
}

func TestHandlerReportsResponseUsageAtEOF(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"input_tokens":12,"output_tokens":3,"cache_creation_input_tokens":4,"cache_read_input_tokens":5}}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	completed := make(chan ResponseResult, 1)
	srv := httptest.NewServer(NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		OnResponseComplete: func(_ *http.Request, result ResponseResult) {
			completed <- result
		},
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case result := <-completed:
		want := ResponseUsage{InputTokens: 12, OutputTokens: 3, CacheCreationInputTokens: 4, CacheReadInputTokens: 5}
		if result.StatusCode != http.StatusOK || result.Usage == nil || *result.Usage != want {
			t.Fatalf("response result = %+v, want status 200 usage %+v", result, want)
		}
	case <-time.After(time.Second):
		t.Fatal("response completion callback did not run")
	}
}

func TestHandlerReportsAnthropicSSEUsageExactlyOnce(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":5}}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"usage\":{\"output_tokens\":3}}\n\n")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	completed := make(chan ResponseResult, 2)
	srv := httptest.NewServer(NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		OnResponseComplete: func(_ *http.Request, result ResponseResult) {
			completed <- result
		},
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	result := <-completed
	if result.Usage == nil || result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 3 || result.Usage.CacheReadInputTokens != 5 {
		t.Fatalf("SSE response result = %+v", result)
	}
	select {
	case duplicate := <-completed:
		t.Fatalf("duplicate completion callback: %+v", duplicate)
	default:
	}
}

func TestHandlerSuppressesResponseCompletionAfterReadFailure(t *testing.T) {
	zero := time.Duration(0)
	var calls atomic.Int32
	h := NewHandler(HandlerOptions{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: &fragmentReadCloser{
					fragments: [][]byte{[]byte("partial")},
					err:       errors.New("upstream stream failed"),
				},
				Request: req,
			}, nil
		}),
		OnResponseComplete:     func(*http.Request, ResponseResult) { calls.Add(1) },
		UpstreamHeadersTimeout: &zero,
		UpstreamIdleTimeout:    &zero,
		DuplicateHold:          &zero,
	})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if calls.Load() != 0 {
		t.Fatalf("completion calls after read failure = %d", calls.Load())
	}
}

func TestHandlerReportsOpenAIResponseWithoutUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	completed := make(chan ResponseResult, 1)
	srv := httptest.NewServer(NewHandler(HandlerOptions{
		OpenAIUpstream: u,
		OnResponseComplete: func(_ *http.Request, result ResponseResult) {
			completed <- result
		},
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"unsupported","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case result := <-completed:
		if result.StatusCode != http.StatusOK || result.Usage != nil {
			t.Fatalf("response result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("response completion callback did not run")
	}
}

func TestResponseCompletionWrapperPreservesReadWriteBodies(t *testing.T) {
	body := &readWriteCloseBuffer{}
	_, _ = body.WriteString("response")
	completed := 0
	wrapper := wrapResponseCompletion(body, func() { completed++ })
	readWriter, ok := wrapper.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("response wrapper dropped io.ReadWriteCloser")
	}
	if _, err := io.ReadAll(readWriter); err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("completion calls = %d, want 1", completed)
	}
	if _, err := readWriter.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("completion calls after write = %d, want 1", completed)
	}
}

func TestHandlerTransformsOpenAIRoutes(t *testing.T) {
	SetAllowedModelBases([]string{"gpt-5.4", "gpt-5"})
	defer SetAllowedModelBases(nil)

	cases := []struct {
		fixture string
		path    string
	}{
		{"chat-big-slab", "/v1/chat/completions"},
		{"responses-codex-pairs", "/v1/responses"},
		{"responses-codex-pairs", "/openai/v1/responses"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			var upstreamBody []byte
			up := upstreamEcho(t, &upstreamBody)
			defer up.Close()
			input, err := os.ReadFile(filepath.Join("testdata", "openai", c.fixture, "input.json"))
			if err != nil {
				t.Fatal(err)
			}
			var applied *TransformResult
			u, _ := url.Parse(up.URL)
			h := NewHandler(HandlerOptions{
				AnthropicUpstream: u,
				OpenAIUpstream:    u,
				OnResult:          func(_ *http.Request, res *TransformResult) { applied = res },
			})
			srv := httptest.NewServer(h)
			defer srv.Close()
			resp, err := http.Post(srv.URL+c.path, "application/json", bytes.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if applied == nil || !applied.Applied {
				t.Fatalf("transform not applied: %+v", applied)
			}
			if !bytes.Contains(upstreamBody, []byte("data:image/png;base64,")) {
				t.Error("upstream body has no rendered image part")
			}
		})
	}
}

func TestHandlerOpenAIUnsupportedModelPassesThrough(t *testing.T) {
	var upstreamBody []byte
	up := upstreamEcho(t, &upstreamBody)
	defer up.Close()
	input, err := os.ReadFile(filepath.Join("testdata", "openai", "chat-big-slab", "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var applied *TransformResult
	u, _ := url.Parse(up.URL)
	h := NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		OpenAIUpstream:    u,
		OnResult:          func(_ *http.Request, res *TransformResult) { applied = res },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if applied == nil || applied.Applied || applied.Reason != ReasonUnsupportedModel {
		t.Fatalf("expected unsupported-model passthrough, got %+v", applied)
	}
	if !bytes.Equal(upstreamBody, input) {
		t.Error("body must pass through untouched")
	}
}

func TestHandlerCustomProtocolOf(t *testing.T) {
	var upstreamBody []byte
	up := upstreamEcho(t, &upstreamBody)
	defer up.Close()

	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var applied *TransformResult
	u, _ := url.Parse(up.URL)
	h := NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		OnResult:          func(_ *http.Request, res *TransformResult) { applied = res },
		ProtocolOf: func(path string) Protocol {
			if path == "/api/llm/claude" {
				return ProtocolAnthropicMessages
			}
			return ProtocolNone
		},
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/llm/claude", "application/json", bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if applied == nil || !applied.Applied {
		t.Fatalf("custom path not transformed: %+v", applied)
	}

	applied = nil
	resp, err = http.Post(srv.URL+"/v1/messages", "application/json", bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if applied != nil {
		t.Fatalf("built-in path should be overridden to pass through, got %+v", applied)
	}
	if !bytes.Equal(upstreamBody, input) {
		t.Error("pass-through body was modified")
	}
}

func TestDefaultProtocolOf(t *testing.T) {
	cases := map[string]Protocol{
		"/v1/messages":                ProtocolAnthropicMessages,
		"/anthropic/v1/messages":      ProtocolAnthropicMessages,
		"/v1/chat/completions":        ProtocolOpenAIChat,
		"/openai/v1/chat/completions": ProtocolOpenAIChat,
		"/v1/responses":               ProtocolOpenAIResponses,
		"/v1/messages/count_tokens":   ProtocolNone,
		"/healthz":                    ProtocolNone,
	}
	for path, want := range cases {
		if got := DefaultProtocolOf(path); got != want {
			t.Errorf("DefaultProtocolOf(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestHandlerRejectsOversizedTransformableBody(t *testing.T) {
	payload := []byte(`{"model":"claude-fable-5","messages":[],"padding":"` + strings.Repeat("x", 128) + `"}`)

	for _, chunked := range []bool{false, true} {
		name := "known-length"
		if chunked {
			name = "chunked"
		}
		t.Run(name, func(t *testing.T) {
			var upstreamBody []byte
			up := upstreamEcho(t, &upstreamBody)
			defer up.Close()
			u, _ := url.Parse(up.URL)
			called := false
			srv := httptest.NewServer(NewHandler(HandlerOptions{
				AnthropicUpstream: u,
				MaxBodyBytes:      32,
				OnResult: func(_ *http.Request, _ *TransformResult) {
					called = true
				},
			}))
			defer srv.Close()

			var body io.Reader = bytes.NewReader(payload)
			if chunked {
				pr, pw := io.Pipe()
				body = pr
				go func() {
					_, _ = pw.Write(payload)
					_ = pw.Close()
				}()
			}
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", body)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			responseBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusRequestEntityTooLarge || !bytes.Contains(responseBody, []byte(`"type":"request_too_large"`)) {
				t.Fatalf("response = %d %s", resp.StatusCode, responseBody)
			}
			if called {
				t.Fatal("oversized body must not be transformed")
			}
			if upstreamBody != nil {
				t.Fatalf("oversized body reached upstream: %d bytes", len(upstreamBody))
			}
		})
	}
}

func TestHandlerBodyLengthHintsPreservePayload(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-4-6","messages":[],"padding":"` + strings.Repeat("x", 40) + `"}`)
	for _, tc := range []struct {
		name          string
		contentLength int64
		maxBodyBytes  int64
		wantStatus    int
		wantResult    bool
	}{
		{name: "short_hint_oversized", contentLength: 32, maxBodyBytes: 64, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "exact_boundary", contentLength: int64(len(payload)), maxBodyBytes: int64(len(payload)), wantStatus: http.StatusNoContent, wantResult: true},
		{name: "long_hint", contentLength: 120, maxBodyBytes: 128, wantStatus: http.StatusNoContent, wantResult: true},
		{name: "false_large_hint", contentLength: 8 << 20, maxBodyBytes: defaultMaxBodyBytes, wantStatus: http.StatusNoContent, wantResult: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var forwarded []byte
			var called bool
			zero := time.Duration(0)
			h := NewHandler(HandlerOptions{
				MaxBodyBytes: tc.maxBodyBytes,
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					forwarded, _ = io.ReadAll(req.Body)
					return &http.Response{
						StatusCode: http.StatusNoContent,
						Header:     make(http.Header),
						Body:       http.NoBody,
						Request:    req,
					}, nil
				}),
				OnResult:               func(_ *http.Request, _ *TransformResult) { called = true },
				UpstreamHeadersTimeout: &zero,
				UpstreamIdleTimeout:    &zero,
				DuplicateHold:          &zero,
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
			req.ContentLength = tc.contentLength
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d", recorder.Code)
			}
			if called != tc.wantResult {
				t.Fatalf("OnResult called = %v, want %v", called, tc.wantResult)
			}
			if tc.wantStatus == http.StatusNoContent && !bytes.Equal(forwarded, payload) {
				t.Fatalf("forwarded %d bytes, want %d", len(forwarded), len(payload))
			}
			if tc.wantStatus != http.StatusNoContent && forwarded != nil {
				t.Fatalf("rejected request forwarded %d bytes", len(forwarded))
			}
		})
	}
}

func TestHandlerOversizedBypassPassesThrough(t *testing.T) {
	payload := []byte(`{"model":"claude-fable-5","messages":[],"padding":"` + strings.Repeat("x", 128) + `"}`)
	var upstreamBody []byte
	up := upstreamEcho(t, &upstreamBody)
	defer up.Close()
	u, _ := url.Parse(up.URL)
	srv := httptest.NewServer(NewHandler(HandlerOptions{AnthropicUpstream: u, MaxBodyBytes: 32}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(payload))
	req.Header.Set("X-Pxpipe-Bypass", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(upstreamBody, payload) {
		t.Fatalf("bypass response=%d body=%d/%d", resp.StatusCode, len(upstreamBody), len(payload))
	}
}

func TestHandlerBypassSkipsTransformAndStripsHeader(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var upstreamBody []byte
	var upstreamBypass string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamBypass = r.Header.Get("X-Pxpipe-Bypass")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	called := false
	srv := httptest.NewServer(NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		OnResult: func(_ *http.Request, _ *TransformResult) {
			called = true
		},
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pxpipe-Bypass", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if called {
		t.Fatal("bypassed request was transformed")
	}
	if !bytes.Equal(upstreamBody, input) {
		t.Fatal("bypassed body was modified")
	}
	if upstreamBypass != "" {
		t.Fatalf("bypass header leaked upstream: %q", upstreamBypass)
	}

	pin := []byte(`{"model":"claude-fable-5","messages":[{"role":"user","content":"@pxpipe pin concise"}]}`)
	req, err = http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(pin))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pxpipe-Bypass", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !bytes.Equal(upstreamBody, pin) {
		t.Fatal("bypassed pin command was handled locally")
	}
}

func TestHandlerFalseyBypassStillTransforms(t *testing.T) {
	var upstreamBody []byte
	var upstreamBypass string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamBypass = r.Header.Get("X-Pxpipe-Bypass")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	var result *TransformResult
	srv := httptest.NewServer(NewHandler(HandlerOptions{
		AnthropicUpstream: u,
		OnResult: func(_ *http.Request, got *TransformResult) {
			result = got
		},
	}))
	defer srv.Close()

	payload := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pxpipe-Bypass", "false")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if result == nil || result.Reason != ReasonUnsupportedModel {
		t.Fatalf("falsey bypass skipped classification: %+v", result)
	}
	if !bytes.Equal(upstreamBody, payload) {
		t.Fatal("unsupported body was modified")
	}
	if upstreamBypass != "" {
		t.Fatalf("bypass header leaked upstream: %q", upstreamBypass)
	}
}
