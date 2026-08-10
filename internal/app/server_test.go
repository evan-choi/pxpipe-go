package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	pxpipe "github.com/evan-choi/pxpipe-go"
	"github.com/evan-choi/pxpipe-go/internal/mitm"
)

const testCertificatePath = "/tmp/pxpipe ca.pem"

func TestRunServerPrintsGuideAndShutsDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	port := availableTCPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	var output lockedBuffer
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, nil, port, nil, &output) }()

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
		"HTTPS_PROXY=http://localhost:",
		"CODEX_CA_CERTIFICATE=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("startup output missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "model_provider") || strings.Contains(got, "openai_base_url") {
		t.Fatalf("Codex command overrides the configured provider: %s", got)
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

func TestServeProxyPreservesCustomCodexProviderAndAuthentication(t *testing.T) {
	t.Setenv("PXPIPE_MODELS", "gpt-5.6-sol")
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "openai", "responses-sol-mixed", "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	type receivedRequest struct {
		host, path, query, authorization, accountID, proxyAuthorization, originalScheme string
		body                                                                            []byte
	}
	received := make(chan receivedRequest, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- receivedRequest{
			host: r.Host, path: r.URL.Path, query: r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"), accountID: r.Header.Get("ChatGPT-Account-Id"),
			proxyAuthorization: r.Header.Get("Proxy-Authorization"), originalScheme: r.Header.Get(mitm.OriginalSchemeHeader),
			body: body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	authority, err := mitm.LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var output lockedBuffer
	proxy, err := newServeProxy(newRequestLog(&output, false), authority, transport, listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown serve proxy: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve proxy: %v", err)
		}
	})

	rootPEM, err := os.ReadFile(authority.CertificatePath())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatal("load serve proxy CA")
	}
	proxyURL, _ := url.Parse("http://" + listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: false,
	}, Timeout: 5 * time.Second}
	request, err := http.NewRequest(http.MethodPost, upstream.URL+"/tenant/v1/responses?trace=1", bytes.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer sk-clb-test")
	request.Header.Set("ChatGPT-Account-Id", "custom-account")
	request.Header.Set("Proxy-Authorization", "Basic must-not-leak")
	request.Header.Set(mitm.OriginalSchemeHeader, "http")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	got := <-received
	upstreamURL, _ := url.Parse(upstream.URL)
	if got.host != upstreamURL.Host || got.path != "/tenant/v1/responses" || got.query != "trace=1" {
		t.Fatalf("upstream destination = host %q path %q query %q", got.host, got.path, got.query)
	}
	if got.authorization != "Bearer sk-clb-test" || got.accountID != "custom-account" {
		t.Fatalf("upstream credentials = authorization %q account %q", got.authorization, got.accountID)
	}
	if got.proxyAuthorization != "" || got.originalScheme != "" {
		t.Fatalf("private headers leaked: proxy authorization %q scheme %q", got.proxyAuthorization, got.originalScheme)
	}
	if !bytes.Contains(got.body, []byte("data:image/")) {
		t.Fatal("custom-provider request reached upstream without transformation")
	}
	if logOutput := output.String(); !strings.Contains(logOutput, "/tenant/v1/responses") {
		t.Fatalf("request telemetry did not use the original endpoint: %s", logOutput)
	}
}

func TestServeProxyHandlesDirectReverseRequestsAndStripsPrivateHeader(t *testing.T) {
	authority, err := mitm.LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan *http.Request, 1)
	transport := appRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		requests <- r.Clone(r.Context())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    r,
		}, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := newServeProxy(newRequestLog(io.Discard, false), authority, transport, listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown serve proxy: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve proxy: %v", err)
		}
	})

	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/v1/messages", strings.NewReader(`{"model":"unsupported","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(mitm.OriginalSchemeHeader, "https")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	forwarded := <-requests
	if forwarded.URL.Host != "api.anthropic.com" || forwarded.URL.Path != "/v1/messages" {
		t.Fatalf("direct upstream = %q", forwarded.URL.String())
	}
	if got := forwarded.Header.Get(mitm.OriginalSchemeHeader); got != "" {
		t.Fatalf("forged private header reached direct upstream: %q", got)
	}
}

type appRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f appRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

func TestRunSummaryAggregatesAndMarksPartialMetrics(t *testing.T) {
	asText, sent := int64(200), int64(120)
	summary := newRunSummary()
	summary.add(requestLogRow{
		status: http.StatusOK, sentAs: "image",
		asText: &asText, sent: &sent,
	})
	summary.add(requestLogRow{status: http.StatusBadGateway, sentAs: "text"})
	var outputBuffer bytes.Buffer
	summary.write(&outputBuffer)
	got := outputBuffer.String()
	for _, want := range []string{
		"pxpipe summary",
		"estimated without pxpipe 200 tokens",
		"actual with pxpipe 120 tokens (-40.0%, 1/2 requests)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q: %s", want, got)
		}
	}
}

func TestRequestTUIViewIncludesGuideAndMetrics(t *testing.T) {
	cacheHits, asText, sent, saved := int64(1200), int64(4200), int64(2800), int64(1400)
	model, _ := newRequestTUIModel(47821, testCertificatePath).Update(requestLogRowsMsg{sequence: 1, rows: []requestLogRow{{
		status: 200, endpoint: "/v1/messages", model: "claude-fable-5", sentAs: "image",
		cacheHits: &cacheHits, asText: &asText, sent: &sent, saved: &saved,
	}}})
	view := model.View()
	lines := strings.Split(view, "\n")
	if len(lines) < 2 || lines[1] != "" {
		t.Fatalf("TUI title is not followed by a blank line: %q", view)
	}
	for _, want := range []string{
		"pxpipe serve",
		"ANTHROPIC_BASE_URL=http://localhost:47821 claude",
		"HTTPS_PROXY=http://localhost:47821",
		"CODEX_CA_CERTIFICATE='/tmp/pxpipe ca.pem'",
		"Codex",
		"[copy]",
		"Cache hits",
		"1,200",
		"4,200",
		"2,800",
		"1,400",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("TUI view missing %q: %s", want, view)
		}
	}
}

func TestRequestTUICommandRowsCopyToClipboard(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	tests := []struct {
		name    string
		row     int
		label   string
		gap     int
		command string
		notice  string
	}{
		{name: "Claude", row: claudeCommandRow, label: "Claude", gap: 2, command: "ANTHROPIC_BASE_URL=http://localhost:47821 claude", notice: "Copied Claude command"},
		{name: "Codex", row: codexCommandRow, label: "Codex", gap: 3, command: newRequestTUIModel(47821, testCertificatePath).codexCommand(), notice: "Copied Codex command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := newRequestTUIModel(47821, testCertificatePath)
			button := model.commandRow(test.label, test.gap, test.command)
			updated, command := model.Update(tea.MouseMsg(tea.MouseEvent{
				X: button.copyStart, Y: test.row, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			}))
			if command == nil {
				t.Fatal("click did not schedule clipboard sequence cleanup")
			}
			view := updated.View()
			encoded := base64.StdEncoding.EncodeToString([]byte(test.command))
			if !strings.Contains(view, "\x1b]52;c;"+encoded) || !strings.Contains(view, test.notice) {
				t.Fatalf("click did not render OSC 52 copy and notice: %q", view)
			}
		})
	}
}

func TestRequestTUIRejectsClicksOutsideCommandText(t *testing.T) {
	model := newRequestTUIModel(47821, testCertificatePath)
	button := model.commandRow("Claude", 2, model.claudeCommand())
	updated, command := model.Update(tea.MouseMsg(tea.MouseEvent{
		X: button.copyStart - 1,
		Y: claudeCommandRow, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	}))
	if command != nil || strings.Contains(updated.View(), "\x1b]52;") {
		t.Fatal("click outside [copy] copied to clipboard")
	}
}

func TestRequestTUINarrowWidthKeepsCommandRowsOnOneLine(t *testing.T) {
	const width = 32
	model := newRequestTUIModel(47821, testCertificatePath)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 12})
	model = updated.(requestTUIModel)
	lines := strings.Split(model.View(), "\n")
	for row := 0; row <= codexCommandRow; row++ {
		if got := lipgloss.Width(lines[row]); got > width {
			t.Fatalf("row %d width = %d, want <= %d: %q", row, got, width, lines[row])
		}
	}
	updated, command := model.Update(tea.MouseMsg(tea.MouseEvent{
		X: width - 1, Y: codexCommandRow, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	}))
	if command == nil || !strings.Contains(updated.View(), base64.StdEncoding.EncodeToString([]byte(model.codexCommand()))) {
		t.Fatal("narrow Codex row did not copy the full command")
	}
}

func TestRequestTUIClipboardCleanupDoesNotClearNewerCopy(t *testing.T) {
	updated, _ := newRequestTUIModel(47821, testCertificatePath).Update(tea.MouseMsg(tea.MouseEvent{
		X: newRequestTUIModel(47821, testCertificatePath).commandRow("Claude", 2, newRequestTUIModel(47821, testCertificatePath).claudeCommand()).copyStart,
		Y: claudeCommandRow, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	}))
	model := updated.(requestTUIModel)
	codexButton := model.commandRow("Codex", 3, model.codexCommand())
	updated, _ = model.Update(tea.MouseMsg(tea.MouseEvent{
		X: codexButton.copyStart, Y: codexCommandRow, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	}))
	model = updated.(requestTUIModel)
	updated, _ = model.Update(clearClipboardSequenceMsg(1))
	model = updated.(requestTUIModel)
	if model.clipboardSequence == "" || !strings.Contains(model.copyNotice, "Codex") {
		t.Fatal("older cleanup cleared the newer clipboard copy")
	}
	updated, _ = model.Update(clearClipboardSequenceMsg(2))
	if updated.(requestTUIModel).clipboardSequence != "" {
		t.Fatal("current clipboard cleanup did not clear its sequence")
	}
}

func TestTerminalClipboardSequenceUsesTmuxNativeOSC52(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	t.Setenv("STY", "")
	sequence := terminalClipboardSequence("copy me")
	if strings.Contains(sequence, "tmux;") || !strings.HasPrefix(sequence, "\x1b]52;") {
		t.Fatalf("tmux clipboard sequence is unnecessarily wrapped: %q", sequence)
	}
}

func TestRequestTUIHandlesTerminalControlKeys(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyType
		want tea.Msg
	}{
		{name: "quit", key: tea.KeyCtrlC, want: tea.QuitMsg{}},
		{name: "suspend", key: tea.KeyCtrlZ, want: tea.SuspendMsg{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, command := newRequestTUIModel(47821, testCertificatePath).Update(tea.KeyMsg{Type: test.key})
			if command == nil {
				t.Fatal("control key did not return a command")
			}
			if got := command(); got != test.want {
				t.Fatalf("command message = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRequestTUIIgnoresOlderRowsAndCompactsAtNarrowWidth(t *testing.T) {
	cacheHits, asText, sent, saved := int64(1200), int64(4200), int64(2800), int64(1400)
	model := newRequestTUIModel(47821, testCertificatePath)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	model = updated.(requestTUIModel)
	updated, _ = model.Update(requestLogRowsMsg{sequence: 2, rows: []requestLogRow{{
		status: 200, endpoint: "/v1/messages", model: "new-model", sentAs: "image",
		cacheHits: &cacheHits, asText: &asText, sent: &sent, saved: &saved,
	}}})
	model = updated.(requestTUIModel)
	updated, _ = model.Update(requestLogRowsMsg{sequence: 1, rows: []requestLogRow{{
		status: 500, endpoint: "/stale", model: "old-model", sentAs: "text",
	}}})
	view := updated.View()
	for _, want := range []string{"Recent requests", "new-model", "cache hits 1,200", "as text 4,200", "sent 2,800", "saved/lost 1,400"} {
		if !strings.Contains(view, want) {
			t.Fatalf("compact TUI missing %q: %s", want, view)
		}
	}
	if strings.Contains(view, "old-model") || strings.Contains(view, "/stale") {
		t.Fatalf("TUI accepted stale rows: %s", view)
	}
}

func TestRequestTUIShortHeightPreservesCommandRows(t *testing.T) {
	for height := 1; height < 10; height++ {
		model := newRequestTUIModel(47821, testCertificatePath)
		updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: height})
		lines := strings.Split(updated.View(), "\n")
		if len(lines) > height {
			t.Fatalf("height %d rendered %d lines", height, len(lines))
		}
		if height > claudeCommandRow && !strings.Contains(lines[claudeCommandRow], "Claude") {
			t.Fatalf("height %d shifted Claude row: %q", height, updated.View())
		}
		if height > codexCommandRow && !strings.Contains(lines[codexCommandRow], "Codex") {
			t.Fatalf("height %d shifted Codex row: %q", height, updated.View())
		}
		if height > codexCommandRow {
			model := updated.(requestTUIModel)
			button := model.commandRow("Codex", 3, model.codexCommand())
			_, command := model.Update(tea.MouseMsg(tea.MouseEvent{
				X: button.copyStart, Y: codexCommandRow, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			}))
			if command == nil {
				t.Fatalf("height %d Codex [copy] was not clickable", height)
			}
		}
	}
}

func TestOfferRequestLogRowsKeepsNewestSnapshot(t *testing.T) {
	updates := make(chan requestLogRowsMsg, 1)
	var mu sync.Mutex
	offerRequestLogRows(&mu, updates, requestLogRowsMsg{sequence: 2})
	offerRequestLogRows(&mu, updates, requestLogRowsMsg{sequence: 1})
	if got := <-updates; got.sequence != 2 {
		t.Fatalf("queued sequence = %d, want 2", got.sequence)
	}
}

func TestOfferRequestLogRowsKeepsNewestConcurrentSnapshot(t *testing.T) {
	updates := make(chan requestLogRowsMsg, 1)
	var mu sync.Mutex
	var producers sync.WaitGroup
	for sequence := uint64(1); sequence <= 100; sequence++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			offerRequestLogRows(&mu, updates, requestLogRowsMsg{sequence: sequence})
		}()
	}
	producers.Wait()
	if got := <-updates; got.sequence != 100 {
		t.Fatalf("queued concurrent sequence = %d, want 100", got.sequence)
	}
}

func TestRequestLogTUILifecycle(t *testing.T) {
	var output bytes.Buffer
	logger := newRequestLog(&output, true)
	done := logger.startTUI(nil, 47821, testCertificatePath)
	logger.add(requestLogRow{status: http.StatusOK, endpoint: "/v1/messages", sentAs: "text"})
	if err := logger.stopTUI(done); err != nil {
		t.Fatal(err)
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
