package pxpipe

import (
	"bufio"
	"bytes"
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

func TestHandlerOversizedBodyPassesThroughUnchanged(t *testing.T) {
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
			resp.Body.Close()
			if called {
				t.Fatal("oversized body must not be transformed")
			}
			if !bytes.Equal(upstreamBody, payload) {
				t.Fatalf("upstream body was truncated: got %d bytes, want %d", len(upstreamBody), len(payload))
			}
		})
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
