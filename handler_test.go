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
	return NewHandler(HandlerOptions{Upstream: u, Transform: opts})
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
		Upstream: u,
		OnResult: func(_ *http.Request, res *TransformResult) { applied = res },
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
				Upstream: u,
				OnResult: func(_ *http.Request, res *TransformResult) { applied = res },
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
		Upstream: u,
		OnResult: func(_ *http.Request, res *TransformResult) { applied = res },
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
