package pxpipe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHandlerAccountsAnthropicCacheUsage(t *testing.T) {
	var responseBody = `{"usage":{"cache_creation_input_tokens":7}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responseBody)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	h := NewHandler(HandlerOptions{AnthropicUpstream: upstreamURL}).(*handler)
	server := httptest.NewServer(h)
	defer server.Close()

	body := []byte(`{"model":"claude-fable-5","messages":[{"role":"user","content":"stable session"}]}`)
	send := func() {
		resp, err := http.Post(server.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	send()
	key := sha8("stable session")
	state := h.sessions.noteHistoryRequest(key, time.Now())
	if state.cold {
		t.Fatal("cache creation should mark the session alive")
	}

	responseBody = `{"usage":{"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	send()
	state = h.sessions.noteHistoryRequest(key, time.Now())
	if !state.cold {
		t.Fatal("zero accounting after a live cache should permit pack-fill")
	}
	h.sessions.noteCacheOutcome(key, 1, 0)
	if again := h.sessions.noteHistoryRequest(key, time.Now()); again.cold {
		t.Fatal("cache-read accounting should restore the warm path")
	}
}

func TestHandlerDoesNotAccountCompressedAnthropicResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		fmt.Fprint(w, `{"usage":{"cache_creation_input_tokens":7}}`)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	h := NewHandler(HandlerOptions{AnthropicUpstream: upstreamURL}).(*handler)
	server := httptest.NewServer(h)
	defer server.Close()
	body := []byte(`{"model":"claude-fable-5","messages":[{"role":"user","content":"compressed session"}]}`)
	resp, err := http.Post(server.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	key := sha8("compressed session")
	h.sessions.mu.Lock()
	rec := h.sessions.touchLocked(key)
	observed := rec.cacheObserved
	h.sessions.mu.Unlock()
	if observed {
		t.Fatal("compressed response unexpectedly updated cache accounting")
	}
}

func TestHandlerAccountsUpstream413BeforeReadingBody(t *testing.T) {
	h := NewHandler(HandlerOptions{}).(*handler)
	key := "session"
	h.sessions.noteHistoryRequest(key, time.Now())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx := context.WithValue(req.Context(), protocolContextKey{}, ProtocolAnthropicMessages)
	ctx = context.WithValue(ctx, sessionContextKey{}, key)
	res := &http.Response{
		StatusCode: http.StatusRequestEntityTooLarge,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString("not read")),
		Request:    req.WithContext(ctx),
	}
	if err := h.proxy.ModifyResponse(res); err != nil {
		t.Fatal(err)
	}
	if got := h.sessions.noteHistoryRequest(key, time.Now()); !got.cold {
		t.Fatal("upstream 413 did not immediately mark the cache dead")
	}
}

func TestHandlerModifyResponseAllowsMissingRequest(t *testing.T) {
	h := NewHandler(HandlerOptions{}).(*handler)
	res := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}
	if err := h.proxy.ModifyResponse(res); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerLocalSizeLimitDoesNotMarkCacheDead(t *testing.T) {
	SetAllowedModelBases([]string{"claude-fable-5"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })
	t.Setenv("PXPIPE_GPT_PROFILES", `{"claude-fable-5":{"maxSerializedRequestBytes":1}}`)
	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("locally rejected request reached upstream")
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	h := NewHandler(HandlerOptions{AnthropicUpstream: upstreamURL}).(*handler)
	server := httptest.NewServer(h)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/messages", "application/json", bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}

	key := sha8(firstUserText(mustParseOrderedJSON(t, input)))
	if got := h.sessions.noteHistoryRequest(key, time.Now()); got.cold {
		t.Fatal("proxy-local size rejection incorrectly marked the upstream cache dead")
	}
}

func mustParseOrderedJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	req, err := parseOrderedJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
