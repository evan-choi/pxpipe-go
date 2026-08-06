package pxpipe

import (
	"bytes"
	"context"
	"crypto/sha256"
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

func reliabilityTestHandler(t *testing.T, upstream string, options HandlerOptions) http.Handler {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	options.AnthropicUpstream = u
	options.OpenAIUpstream = u
	return NewHandler(options)
}

func postJSON(t *testing.T, client *http.Client, target string, body []byte, authorization string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func responseJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := jsonUnmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return decoded
}

func TestReliabilityOptionDefaultsAndZero(t *testing.T) {
	defaults := resolveReliabilityConfig(HandlerOptions{})
	if defaults.headersTimeout != 5*time.Minute ||
		defaults.idleTimeout != 2*time.Minute ||
		defaults.duplicateHold != time.Minute {
		t.Fatalf("defaults = %+v", defaults)
	}

	zero := time.Duration(0)
	disabled := resolveReliabilityConfig(HandlerOptions{
		UpstreamHeadersTimeout: &zero,
		UpstreamIdleTimeout:    &zero,
		DuplicateHold:          &zero,
	})
	if disabled != (reliabilityConfig{}) {
		t.Fatalf("explicit zero must disable reliability timers: %+v", disabled)
	}
}

func TestHandlerRejectsCompressedBodyAboveProfileLimit(t *testing.T) {
	SetAllowedModelBases([]string{"claude-fable-5", "gpt-5.4"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })
	t.Setenv("PXPIPE_GPT_PROFILES", `{
        "claude-fable-5":{"maxSerializedRequestBytes":1},
        "gpt-5.4":{"maxSerializedRequestBytes":1}
    }`)

	tests := []struct {
		name        string
		fixture     string
		path        string
		wantTopType bool
	}{
		{
			name:        "anthropic messages",
			fixture:     filepath.Join("testdata", "transform", "big-claude-code", "input.json"),
			path:        "/v1/messages",
			wantTopType: true,
		},
		{
			name:    "openai chat",
			fixture: filepath.Join("testdata", "openai", "chat-big-slab", "input.json"),
			path:    "/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, err := os.ReadFile(tt.fixture)
			if err != nil {
				t.Fatal(err)
			}
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				fmt.Fprint(w, `{}`)
			}))
			defer upstream.Close()

			var result *TransformResult
			handler := reliabilityTestHandler(t, upstream.URL, HandlerOptions{
				OnResult: func(_ *http.Request, got *TransformResult) { result = got },
			})
			server := httptest.NewServer(handler)
			defer server.Close()

			resp := postJSON(t, server.Client(), server.URL+tt.path, input, "")
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			decoded := responseJSON(t, resp)
			if _, found := decoded["type"]; found != tt.wantTopType {
				t.Fatalf("top-level type presence = %v, body = %#v", found, decoded)
			}
			errorBody, ok := decoded["error"].(map[string]any)
			if !ok || errorBody["type"] != "request_too_large" {
				t.Fatalf("error body = %#v", decoded)
			}
			if upstreamCalls.Load() != 0 {
				t.Fatalf("upstream calls = %d", upstreamCalls.Load())
			}
			if result == nil || !result.Applied || result.Info == nil ||
				result.Info.SerializedRequestBytes <= 1 || result.Info.SizeLimitOutcome != "rejected" {
				t.Fatalf("transform result = %+v", result)
			}
		})
	}
}

func TestHandlerDoesNotLimitUncompressedBody(t *testing.T) {
	SetAllowedModelBases([]string{"gemini-3.6-flash"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })
	t.Setenv("PXPIPE_GPT_PROFILES", `{"gemini-3.6-flash":{"maxSerializedRequestBytes":1}}`)

	payload := []byte(`{"model":"gemini-3.6-flash","input":"hello"}`)
	var forwarded []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	disabled := false
	var result *TransformResult
	handler := reliabilityTestHandler(t, upstream.URL, HandlerOptions{
		Transform: &TransformOptions{Compress: &disabled},
		OnResult:  func(_ *http.Request, got *TransformResult) { result = got },
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	resp := postJSON(t, server.Client(), server.URL+"/v1/responses", payload, "Bearer test")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !bytes.Equal(forwarded, payload) {
		t.Fatalf("forwarded body = %q", forwarded)
	}
	if result == nil || result.Applied || result.Info == nil ||
		result.Info.SerializedRequestBytes != len(payload) || result.Info.SizeLimitOutcome != "" {
		t.Fatalf("transform result = %+v", result)
	}
}

func TestHandlerUpstreamHeadersTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	disabled := false
	zero := time.Duration(0)
	headersTimeout := 40 * time.Millisecond
	handler := reliabilityTestHandler(t, upstream.URL, HandlerOptions{
		Transform:              &TransformOptions{Compress: &disabled},
		UpstreamHeadersTimeout: &headersTimeout,
		UpstreamIdleTimeout:    &zero,
		DuplicateHold:          &zero,
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.6-flash","input":"hello"}`)
	started := time.Now()
	resp := postJSON(t, server.Client(), server.URL+"/v1/responses", payload, "Bearer test")
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	decoded := responseJSON(t, resp)
	if decoded["error"] != "pxpipe upstream timeout (no response headers within 40ms)" {
		t.Fatalf("body = %#v", decoded)
	}
	if time.Since(started) > time.Second {
		t.Fatal("headers timeout took too long")
	}
}

func TestHeadersTimeoutCancelsRoundTripper(t *testing.T) {
	cancelled := make(chan error, 1)
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		cancelled <- contextCause(r)
		return nil, r.Context().Err()
	})
	transport := newReliabilityTransport(base, reliabilityConfig{headersTimeout: 20 * time.Millisecond})
	request, _ := http.NewRequest(http.MethodGet, "http://upstream", nil)

	_, err := transport.RoundTrip(request)
	var timeout *upstreamHeadersTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("RoundTrip error = %v", err)
	}
	select {
	case cause := <-cancelled:
		if !errors.As(cause, &timeout) {
			t.Fatalf("cancel cause = %v", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("RoundTripper context was not cancelled")
	}
}

func contextCause(r *http.Request) error {
	return context.Cause(r.Context())
}

func TestHandlerUpstreamIdleTimeoutAllowsSlowFirstChunk(t *testing.T) {
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		select {
		case <-time.After(80 * time.Millisecond):
		case <-r.Context().Done():
			close(cancelled)
			return
		}
		fmt.Fprint(w, "data: first\n\n")
		flusher.Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()

	disabled := false
	headersTimeout := 300 * time.Millisecond
	idleTimeout := 40 * time.Millisecond
	zero := time.Duration(0)
	handler := reliabilityTestHandler(t, upstream.URL, HandlerOptions{
		Transform:              &TransformOptions{Compress: &disabled},
		UpstreamHeadersTimeout: &headersTimeout,
		UpstreamIdleTimeout:    &idleTimeout,
		DuplicateHold:          &zero,
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.6-flash","input":"hello"}`)
	resp := postJSON(t, server.Client(), server.URL+"/v1/responses", payload, "Bearer test")
	type readResult struct {
		body []byte
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		body, err := io.ReadAll(resp.Body)
		done <- readResult{body: body, err: err}
	}()

	select {
	case result := <-done:
		_ = resp.Body.Close()
		if !bytes.Contains(result.body, []byte("data: first")) {
			t.Fatalf("slow first chunk was not delivered: %q (%v)", result.body, result.err)
		}
	case <-time.After(time.Second):
		_ = resp.Body.Close()
		t.Fatal("idle upstream stream hung")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("idle timeout did not cancel upstream")
	}
}

func TestHandlerUpstreamIdleTimeoutResetsOnBytes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			time.Sleep(30 * time.Millisecond)
			fmt.Fprintf(w, "data: %d\n\n", i)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	disabled := false
	headersTimeout := 300 * time.Millisecond
	idleTimeout := 70 * time.Millisecond
	zero := time.Duration(0)
	handler := reliabilityTestHandler(t, upstream.URL, HandlerOptions{
		Transform:              &TransformOptions{Compress: &disabled},
		UpstreamHeadersTimeout: &headersTimeout,
		UpstreamIdleTimeout:    &idleTimeout,
		DuplicateHold:          &zero,
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.6-flash","input":"hello"}`)
	resp := postJSON(t, server.Client(), server.URL+"/v1/responses", payload, "Bearer test")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if !bytes.Contains(body, []byte(fmt.Sprintf("data: %d", i))) {
			t.Fatalf("missing chunk %d: %q", i, body)
		}
	}
}

func TestReliabilityBodyUsesIdleTimeoutForFirstChunkWhenHeadersDisabled(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	released := make(chan struct{}, 1)
	body := newReliabilityBody(reader, cancel, func() { released <- struct{}{} }, 0, 20*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		var timeout *upstreamIdleTimeoutError
		if !errors.As(err, &timeout) {
			t.Fatalf("read error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first chunk wait was not bounded by idle timeout")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("idle timeout did not release lease")
	}
	if cause := context.Cause(ctx); cause == nil {
		t.Fatal("idle timeout did not cancel request context")
	}
}

func TestHandlerDuplicateInFlightLifecycle(t *testing.T) {
	releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(upstreamCalls.Add(1)) - 1
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: call-%d\n\n", call)
		w.(http.Flusher).Flush()
		if call < len(releases) {
			select {
			case <-releases[call]:
			case <-r.Context().Done():
			}
		}
		fmt.Fprint(w, "data: done\n\n")
	}))
	defer upstream.Close()

	disabled := false
	hold := 100 * time.Millisecond
	headersTimeout := time.Second
	zero := time.Duration(0)
	handler := reliabilityTestHandler(t, upstream.URL, HandlerOptions{
		Transform:              &TransformOptions{Compress: &disabled},
		UpstreamHeadersTimeout: &headersTimeout,
		UpstreamIdleTimeout:    &zero,
		DuplicateHold:          &hold,
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()
	payload := []byte(`{"model":"gemini-3.6-flash","input":"hello"}`)
	request := func() *http.Response {
		return postJSON(t, client, server.URL+"/v1/responses", payload, "Bearer same")
	}

	first := request()
	immediate := request()
	if immediate.StatusCode != http.StatusConflict {
		t.Fatalf("immediate duplicate status = %d", immediate.StatusCode)
	}
	duplicateBody := responseJSON(t, immediate)
	duplicateError, _ := duplicateBody["error"].(map[string]any)
	if duplicateError["type"] != "duplicate_request_in_flight" || upstreamCalls.Load() != 1 {
		t.Fatalf("duplicate body/calls = %#v / %d", duplicateBody, upstreamCalls.Load())
	}

	time.Sleep(150 * time.Millisecond)
	late := request()
	if late.StatusCode != http.StatusOK || upstreamCalls.Load() != 2 {
		t.Fatalf("late retry status/calls = %d / %d", late.StatusCode, upstreamCalls.Load())
	}

	close(releases[0])
	_, _ = io.ReadAll(first.Body)
	_ = first.Body.Close()
	stillDuplicate := request()
	if stillDuplicate.StatusCode != http.StatusConflict {
		t.Fatalf("old lease release deleted replacement: status = %d", stillDuplicate.StatusCode)
	}
	_ = stillDuplicate.Body.Close()

	close(releases[1])
	_, _ = io.ReadAll(late.Body)
	_ = late.Body.Close()
	final := request()
	defer final.Body.Close()
	if final.StatusCode != http.StatusOK || upstreamCalls.Load() != 3 {
		t.Fatalf("post-release status/calls = %d / %d", final.StatusCode, upstreamCalls.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeTrackingBody struct{ closed atomic.Bool }

func (*closeTrackingBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type readWriteCloseBuffer struct {
	bytes.Buffer
	closed atomic.Bool
}

func (b *readWriteCloseBuffer) Close() error {
	b.closed.Store(true)
	return nil
}

func TestReliabilityBodyCloseReleasesLease(t *testing.T) {
	var calls atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})
	transport := newReliabilityTransport(base, reliabilityConfig{duplicateHold: time.Minute})
	request, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/responses", strings.NewReader("body"))
	request = withBodyDigest(request, []byte("body"))

	first, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/responses", nil)
	duplicateBody := &closeTrackingBody{}
	duplicate.Body = duplicateBody
	duplicate = withBodyDigest(duplicate, []byte("body"))
	if _, err := transport.RoundTrip(duplicate); !errors.Is(err, errDuplicateRequestInFlight) {
		t.Fatalf("second RoundTrip error = %v", err)
	}
	if !duplicateBody.closed.Load() {
		t.Fatal("duplicate request body was not closed")
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := transport.RoundTrip(request.Clone(request.Context()))
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Body.Close()
	if calls.Load() != 2 {
		t.Fatalf("base calls = %d", calls.Load())
	}
}

func TestReliabilityTransportPreservesUpgradeBody(t *testing.T) {
	upgraded := &readWriteCloseBuffer{}
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusSwitchingProtocols,
			Header:     make(http.Header),
			Body:       upgraded,
		}, nil
	})
	transport := newReliabilityTransport(base, reliabilityConfig{})
	request, _ := http.NewRequest(http.MethodGet, "http://upstream/socket", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	readWriter, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("upgrade body type = %T", response.Body)
	}
	if _, err := readWriter.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if got := upgraded.String(); got != "ping" {
		t.Fatalf("upgrade write = %q", got)
	}
	if err := readWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if !upgraded.closed.Load() {
		t.Fatal("upgrade body was not closed")
	}
}

func TestDuplicateKeyIncludesFinalHeaders(t *testing.T) {
	digest := sha256.Sum256([]byte("body"))
	first, _ := http.NewRequest(http.MethodPost, "https://upstream/v1/responses", nil)
	first.Header.Set("Authorization", "Bearer a")
	first.Header.Set("X-Test", "value")
	second, _ := http.NewRequest(http.MethodPost, "https://upstream/v1/responses", nil)
	second.Header.Set("X-Test", "value")
	second.Header.Set("Authorization", "Bearer a")
	if duplicateKey(first, digest) != duplicateKey(second, digest) {
		t.Fatal("header insertion order changed duplicate key")
	}
	second.Header.Set("Authorization", "Bearer b")
	if duplicateKey(first, digest) == duplicateKey(second, digest) {
		t.Fatal("authorization must be part of duplicate key")
	}
}

func TestProxyTransportFailureIsJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	proxyErrorHandler(recorder, request, errors.New("dial failed"))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content-type = %q", contentType)
	}
	var body map[string]any
	if err := jsonUnmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "pxpipe upstream unreachable" {
		t.Fatalf("body = %#v", body)
	}
}
