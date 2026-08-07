package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/evan-choi/pxpipe-go/internal/mitm"
)

func TestProfileEnvironment(t *testing.T) {
	tests := []struct {
		name      string
		profile   profile
		caEnv     string
		hasHTTP   bool
		websocket string
		unset     []string
	}{
		{
			name: "claude", profile: claudeProfile("claude", nil), caEnv: "NODE_EXTRA_CA_CERTS",
			hasHTTP: true, unset: []string{"NO_PROXY", "no_proxy"},
		},
		{
			name: "opencode", profile: openCodeProfile("opencode", nil), caEnv: "NODE_EXTRA_CA_CERTS",
			hasHTTP: true, websocket: "false", unset: []string{"NO_PROXY", "no_proxy"},
		},
		{
			name: "codex", profile: codexProfile("codex", nil), caEnv: "CODEX_CA_CERTIFICATE",
			hasHTTP: true, unset: []string{"NO_PROXY", "no_proxy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, unset := tt.profile.environment("http://127.0.0.1:1234", "/tmp/pxpipe-ca.pem")
			if set["HTTPS_PROXY"] != "http://127.0.0.1:1234" || set["https_proxy"] != set["HTTPS_PROXY"] {
				t.Fatalf("HTTPS proxy environment = %#v", set)
			}
			if set[tt.caEnv] != "/tmp/pxpipe-ca.pem" {
				t.Fatalf("CA environment = %#v", set)
			}
			if tt.hasHTTP && (set["HTTP_PROXY"] != set["HTTPS_PROXY"] || set["http_proxy"] != set["HTTPS_PROXY"]) {
				t.Fatalf("HTTP proxy environment = %#v", set)
			}
			if set["OPENCODE_EXPERIMENTAL_WEBSOCKETS"] != tt.websocket {
				t.Fatalf("WebSocket setting = %q", set["OPENCODE_EXPERIMENTAL_WEBSOCKETS"])
			}
			for _, name := range tt.unset {
				if !slices.Contains(unset, name) {
					t.Errorf("unset does not contain %q: %#v", name, unset)
				}
				if slices.Contains(unset, "ANTHROPIC_BASE_URL") || slices.Contains(unset, "ANTHROPIC_UNIX_SOCKET") {
					t.Fatal("Claude upstream override was removed")
				}
			}
		})
	}
}

func TestProfileHandlerForwardsCustomOpenAIPathToOriginalHost(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := codexProfile("codex", nil)
	transformPath := "/private-transform"

	req := httptest.NewRequest(http.MethodPost, "http://transform.local/private-transform/backend-api/codex/responses", strings.NewReader(`{"model":"unsupported","input":[]}`))
	req.Host = upstreamURL.Host
	req.Header.Set(mitm.OriginalSchemeHeader, upstreamURL.Scheme)
	response := httptest.NewRecorder()
	p.handler(upstream.Client().Transport, transformPath).ServeHTTP(response, req)
	if response.Code != http.StatusOK || gotPath != "/backend-api/codex/responses" {
		t.Fatalf("response = %d, upstream path = %q", response.Code, gotPath)
	}

	unknown := httptest.NewRequest(http.MethodGet, "http://transform.local/health", nil)
	unknown.Host = upstreamURL.Host
	unknown.Header.Set(mitm.OriginalSchemeHeader, upstreamURL.Scheme)
	response = httptest.NewRecorder()
	p.handler(upstream.Client().Transport, transformPath).ServeHTTP(response, unknown)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unknown host status = %d", response.Code)
	}
}

func TestNamedProfilesExposeExpectedInferenceRoutes(t *testing.T) {
	tests := []struct {
		name    string
		profile profile
		want    []string
	}{
		{name: "claude", profile: claudeProfile("claude", nil), want: []string{"*/messages"}},
		{name: "opencode", profile: openCodeProfile("opencode", nil), want: []string{
			"*/messages", "*/chat/completions", "*/responses",
		}},
		{name: "codex", profile: codexProfile("codex", nil), want: []string{"*/responses"}},
	}
	transformURL, err := url.Parse("http://127.0.0.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, route := range tt.profile.routes(transformURL) {
				got = append(got, route.Host+route.PathPrefix+route.PathSuffix)
				if route.Upstream.String() != transformURL.String() {
					t.Fatalf("route upstream = %q", route.Upstream)
				}
			}
			for _, want := range tt.want {
				if !slices.Contains(got, want) {
					t.Errorf("routes do not contain %q: %#v", want, got)
				}
			}
		})
	}
}
