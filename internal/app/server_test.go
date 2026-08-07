package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pxpipe "github.com/evan-choi/pxpipe-go"
)

func TestRunServerPrintsGuideAndShutsDown(t *testing.T) {
	port := availableTCPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	var output lockedBuffer
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, port, &output) }()

	address := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond); err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
	got := output.String()
	for _, want := range []string{
		"Listening on 127.0.0.1:",
		"ANTHROPIC_BASE_URL=http://localhost:",
		"OPENAI_BASE_URL=http://localhost:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("startup output missing %q: %s", want, got)
		}
	}
}

func TestServeHandlerLogsAnthropicUsage(t *testing.T) {
	const usageBody = `{"usage":{"input_tokens":100,"output_tokens":5,"cache_creation_input_tokens":20,"cache_read_input_tokens":30}}`
	acceptedEncoding := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptedEncoding <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Accept-Encoding") != "identity" {
			w.Header().Set("Content-Encoding", "gzip")
			compressed := gzip.NewWriter(w)
			_, _ = io.WriteString(compressed, usageBody)
			_ = compressed.Close()
			return
		}
		_, _ = io.WriteString(w, usageBody)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	var output lockedBuffer
	logger := newRequestLog(&output, false)
	handler := newServeHandler(logger, pxpipe.HandlerOptions{AnthropicUpstream: upstreamURL})
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", "gzip, br")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if got := <-acceptedEncoding; got != "identity" {
		t.Fatalf("upstream Accept-Encoding = %q, want identity", got)
	}

	var got string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got = output.String()
		if strings.Contains(got, "claude-fable-5") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for _, want := range []string{"Result", "/v1/messages", "claude-fable-5", "text", "30", "128"} {
		if !strings.Contains(got, want) {
			t.Fatalf("request log missing %q: %s", want, got)
		}
	}
}

func TestStatusResponseWriterForwardsInformationalThenFinalStatus(t *testing.T) {
	recorder := &statusListResponseWriter{header: make(http.Header)}
	wrapper := &statusResponseWriter{ResponseWriter: recorder}
	wrapper.WriteHeader(http.StatusEarlyHints)
	wrapper.WriteHeader(http.StatusNotFound)
	_, _ = wrapper.Write([]byte("missing"))
	if wrapper.status != http.StatusNotFound || !slices.Equal(recorder.statuses, []int{http.StatusEarlyHints, http.StatusNotFound}) {
		t.Fatalf("status = wrapper %d response %v", wrapper.status, recorder.statuses)
	}
}

func TestRequestLogSanitizesCells(t *testing.T) {
	row := formatRequestLogRow(requestLogRow{
		status: 200, endpoint: "/v1/messages\n\x1b[2J", model: "mødel-世界", sentAs: "text",
	})
	if strings.ContainsAny(row, "\n\r\x1b") || !strings.Contains(row, "mødel-世界") {
		t.Fatalf("unsafe or invalid log row: %q", row)
	}
}

func TestServeHandlerPreservesUpgradeAndLogs101(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream response does not support hijacking")
			return
		}
		conn, stream, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = stream.Flush()
		line, err := stream.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = stream.WriteString("echo: " + line)
		_ = stream.Flush()
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	var output lockedBuffer
	proxy := httptest.NewServer(newServeHandler(newRequestLog(&output, false), pxpipe.HandlerOptions{
		AnthropicUpstream: upstreamURL,
	}))
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(conn, "GET /upgrade HTTP/1.1\r\nHost: proxy\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("upgrade status = %q, err = %v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	_, _ = io.WriteString(conn, "ping\n")
	echo, err := reader.ReadString('\n')
	if err != nil || echo != "echo: ping\n" {
		t.Fatalf("upgrade echo = %q, err = %v", echo, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !strings.Contains(output.String(), "101") {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(output.String(), "101") {
		t.Fatalf("upgrade telemetry missing status 101: %s", output.String())
	}
}

func TestRequestLogRetainsTwentyRows(t *testing.T) {
	logger := newRequestLog(io.Discard, true)
	for status := 200; status < 225; status++ {
		logger.add(requestLogRow{status: status, endpoint: "/v1/messages", sentAs: "-"})
	}
	if len(logger.rows) != 20 || logger.rows[0].status != 205 || logger.rows[19].status != 224 {
		t.Fatalf("recent rows = %d, first=%d last=%d", len(logger.rows), logger.rows[0].status, logger.rows[19].status)
	}
}

func availableTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type statusListResponseWriter struct {
	header   http.Header
	statuses []int
}

func (w *statusListResponseWriter) Header() http.Header { return w.header }
func (w *statusListResponseWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}
func (*statusListResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
