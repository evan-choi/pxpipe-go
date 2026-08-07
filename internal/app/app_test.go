package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunProfileComposesProxyEnvironmentAndReturnsChildExitCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("PXPIPE_APP_HELPER", "1")
	t.Setenv("NO_PROXY", "api.anthropic.com")
	t.Setenv("no_proxy", "api.anthropic.com")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:2455")
	t.Setenv("ANTHROPIC_UNIX_SOCKET", "/tmp/anthropic.sock")
	extraCA := filepath.Join(home, "existing-ca.pem")
	if err := os.WriteFile(extraCA, []byte("EXISTING-CA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", extraCA)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	code, err := runProfile(context.Background(), claudeProfile(os.Args[0], []string{"-test.run=TestAppHelper"}), nil, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code != 17 {
		t.Fatalf("exit code = %d", code)
	}
	proxyURL, err := url.Parse(strings.TrimSpace(stdout.String()))
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL.Scheme != "http" || !strings.HasPrefix(proxyURL.Host, "127.0.0.1:") || strings.HasSuffix(proxyURL.Host, ":0") {
		t.Fatalf("proxy URL = %q", proxyURL.String())
	}
	certificatePath := filepath.Join(configDir, "pxpipe", "mitm-ca.pem")
	if _, err := os.Stat(certificatePath); err != nil {
		t.Fatalf("CA certificate: %v", err)
	}
}

func TestProfilesRouteLocalOverridesByPath(t *testing.T) {
	tests := []struct {
		name        string
		profile     func(string, []string) profile
		models      string
		fixturePath string
		requestPath string
		imageMarker string
	}{
		{
			name: "claude", profile: claudeProfile, models: "claude-fable-5",
			fixturePath: filepath.Join("..", "..", "testdata", "transform", "big-claude-code", "input.json"),
			requestPath: "/tenant/v1/messages",
			imageMarker: `"type":"base64"`,
		},
		{
			name: "codex", profile: codexProfile, models: "gpt-5.6-sol",
			fixturePath: filepath.Join("..", "..", "testdata", "openai", "responses-sol-mixed", "input.json"),
			requestPath: "/tenant/v1/responses",
			imageMarker: "data:image/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			t.Setenv("PXPIPE_MODELS", tt.models)
			upstreamBodies := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamBodies <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()
			t.Setenv("PXPIPE_PROXY_REQUEST_HELPER", "1")
			t.Setenv("PXPIPE_PROXY_REQUEST_TARGET", upstream.URL)
			t.Setenv("PXPIPE_PROXY_REQUEST_PATH", tt.requestPath)
			t.Setenv("PXPIPE_PROXY_REQUEST_FIXTURE", tt.fixturePath)

			code, err := runProfile(
				context.Background(),
				tt.profile(os.Args[0], []string{"-test.run=^TestProxyRequestHelper$"}),
				nil, io.Discard, io.Discard,
			)
			if err != nil {
				t.Fatal(err)
			}
			if code != 0 {
				t.Fatalf("helper exit code = %d", code)
			}
			select {
			case body := <-upstreamBodies:
				if !bytes.Contains(body, []byte(tt.imageMarker)) {
					t.Fatal("local override request reached upstream without transformation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("local override did not receive the proxied request")
			}
		})
	}
}

func TestClaudeProfileRoutesUnixSocketOverride(t *testing.T) {
	type observation struct {
		body []byte
		host string
		path string
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("PXPIPE_MODELS", "claude-fable-5")
	upstreamRequests := make(chan observation, 1)
	upstreamListener, _, removeUpstreamSocket, err := newUnixSocketListener()
	if err != nil {
		t.Fatal(err)
	}
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamRequests <- observation{body: body, host: r.Host, path: r.URL.Path}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})}
	upstreamDone := make(chan error, 1)
	go func() { upstreamDone <- serve(upstream, upstreamListener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := upstream.Shutdown(ctx); err != nil {
			t.Errorf("shutdown Unix upstream: %v", err)
		}
		if err := <-upstreamDone; err != nil {
			t.Errorf("serve Unix upstream: %v", err)
		}
		removeUpstreamSocket()
	})
	t.Setenv("ANTHROPIC_UNIX_SOCKET", upstreamListener.Addr().String())
	t.Setenv("PXPIPE_PROXY_REQUEST_HELPER", "1")
	t.Setenv("PXPIPE_PROXY_REQUEST_USE_UNIX", "1")
	t.Setenv("PXPIPE_PROXY_REQUEST_TARGET", "http://api.anthropic.com")
	t.Setenv("PXPIPE_PROXY_REQUEST_PATH", "/tenant/v1/messages")
	t.Setenv("PXPIPE_PROXY_REQUEST_FIXTURE", filepath.Join("..", "..", "testdata", "transform", "big-claude-code", "input.json"))

	code, err := runProfile(
		context.Background(),
		claudeProfile(os.Args[0], []string{"-test.run=^TestProxyRequestHelper$"}),
		nil, io.Discard, io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("helper exit code = %d", code)
	}
	select {
	case request := <-upstreamRequests:
		if request.host != "api.anthropic.com" || request.path != "/tenant/v1/messages" {
			t.Fatalf("Unix upstream request = host %q path %q", request.host, request.path)
		}
		if !bytes.Contains(request.body, []byte(`"type":"base64"`)) {
			t.Fatal("Unix socket request reached upstream without transformation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("original Unix socket did not receive the proxied request")
	}
}

func TestProxyRequestHelper(t *testing.T) {
	if os.Getenv("PXPIPE_PROXY_REQUEST_HELPER") != "1" {
		return
	}
	payload, err := os.ReadFile(os.Getenv("PXPIPE_PROXY_REQUEST_FIXTURE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(82)
	}
	target := strings.TrimRight(os.Getenv("PXPIPE_PROXY_REQUEST_TARGET"), "/") + os.Getenv("PXPIPE_PROXY_REQUEST_PATH")
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(83)
	}
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{}
	if os.Getenv("PXPIPE_PROXY_REQUEST_USE_UNIX") == "1" {
		socketPath := os.Getenv("ANTHROPIC_UNIX_SOCKET")
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		}
	} else {
		proxyURL, parseErr := url.Parse(os.Getenv("HTTP_PROXY"))
		if parseErr != nil {
			fmt.Fprintln(os.Stderr, parseErr)
			os.Exit(81)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(84)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, response.Status)
		os.Exit(85)
	}
	os.Exit(0)
}

func TestAppHelper(t *testing.T) {
	if os.Getenv("PXPIPE_APP_HELPER") != "1" {
		return
	}
	proxyURL := os.Getenv("HTTPS_PROXY")
	if proxyURL == "" || os.Getenv("https_proxy") != proxyURL {
		fmt.Fprintln(os.Stderr, "proxy environment mismatch")
		os.Exit(91)
	}
	if os.Getenv("NODE_EXTRA_CA_CERTS") == "" {
		fmt.Fprintln(os.Stderr, "missing CA environment")
		os.Exit(92)
	}
	certificateBundle, err := os.ReadFile(os.Getenv("NODE_EXTRA_CA_CERTS"))
	if err != nil || !strings.Contains(string(certificateBundle), "EXISTING-CA") ||
		!strings.Contains(string(certificateBundle), "BEGIN CERTIFICATE") {
		fmt.Fprintln(os.Stderr, "existing CA was not preserved")
		os.Exit(95)
	}
	unixSocket := os.Getenv("ANTHROPIC_UNIX_SOCKET")
	if os.Getenv("ANTHROPIC_BASE_URL") != "http://127.0.0.1:2455" || unixSocket == "" || unixSocket == "/tmp/anthropic.sock" {
		fmt.Fprintln(os.Stderr, "Claude override environment mismatch")
		os.Exit(93)
	}
	if _, err := os.Stat(unixSocket); err != nil {
		fmt.Fprintln(os.Stderr, "Claude Unix proxy is not listening")
		os.Exit(96)
	}
	if os.Getenv("NO_PROXY") != "" || os.Getenv("no_proxy") != "" {
		fmt.Fprintln(os.Stderr, "proxy bypass environment was not removed")
		os.Exit(94)
	}
	fmt.Println(proxyURL)
	os.Exit(17)
}
