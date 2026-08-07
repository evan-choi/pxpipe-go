package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunProfileComposesProxyEnvironmentAndReturnsChildExitCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("PXPIPE_APP_HELPER", "1")
	t.Setenv("NO_PROXY", "api.anthropic.com")
	t.Setenv("no_proxy", "api.anthropic.com")
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
	if os.Getenv("ANTHROPIC_BASE_URL") != "" || os.Getenv("ANTHROPIC_UNIX_SOCKET") != "" {
		fmt.Fprintln(os.Stderr, "base URL environment was not removed")
		os.Exit(93)
	}
	if os.Getenv("NO_PROXY") != "" || os.Getenv("no_proxy") != "" {
		fmt.Fprintln(os.Stderr, "proxy bypass environment was not removed")
		os.Exit(94)
	}
	fmt.Println(proxyURL)
	os.Exit(17)
}
