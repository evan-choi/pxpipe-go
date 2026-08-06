package pxpipe

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

type routeObservation struct {
	provider string
	path     string
	query    string
	header   http.Header
}

func routeUpstream(t *testing.T, provider string, seen chan<- routeObservation) (*httptest.Server, *url.URL) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		seen <- routeObservation{provider: provider, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return srv, u
}

func requestRoute(t *testing.T, client *http.Client, target string, headers http.Header, seen <-chan routeObservation) routeObservation {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return <-seen
}

func TestHandlerProviderUpstreamDefaults(t *testing.T) {
	defaultHandler := NewHandler(HandlerOptions{}).(*handler)
	if got := defaultHandler.opts.AnthropicUpstream.String(); got != "https://api.anthropic.com" {
		t.Fatalf("default AnthropicUpstream = %q", got)
	}
	if got := defaultHandler.opts.OpenAIUpstream.String(); got != "https://api.openai.com" {
		t.Fatalf("default OpenAIUpstream = %q", got)
	}
}

func TestHandlerRoutesProviderUpstreams(t *testing.T) {
	seen := make(chan routeObservation, 1)
	anthropic, anthropicURL := routeUpstream(t, "anthropic", seen)
	defer anthropic.Close()
	openAI, openAIURL := routeUpstream(t, "openai", seen)
	defer openAI.Close()
	proxy := httptest.NewServer(NewHandler(HandlerOptions{AnthropicUpstream: anthropicURL, OpenAIUpstream: openAIURL}))
	defer proxy.Close()

	tests := []struct {
		path string
		want string
	}{
		{"/v1/messages", "anthropic"},
		{"/v1/chat/completions", "openai"},
		{"/v1/responses", "openai"},
		{"/v1/responses/resp_123", "openai"},
		{"/openai/v1/responses", "anthropic"},
		{"/compat/chat/completions", "anthropic"},
		{"/anthropic/v1/messages", "anthropic"},
		{"/google-ai-studio/v1beta/models/gemini", "anthropic"},
		{"/grok/chat/completions", "anthropic"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := requestRoute(t, proxy.Client(), proxy.URL+tt.path+"?trace=1", nil, seen)
			if got.provider != tt.want || got.path != tt.path || got.query != "trace=1" {
				t.Fatalf("route = %s %s?%s, want %s %s?trace=1", got.provider, got.path, got.query, tt.want, tt.path)
			}
		})
	}
}

func TestHandlerRoutesModelsByAuthStyle(t *testing.T) {
	seen := make(chan routeObservation, 1)
	anthropic, anthropicURL := routeUpstream(t, "anthropic", seen)
	defer anthropic.Close()
	openAI, openAIURL := routeUpstream(t, "openai", seen)
	defer openAI.Close()

	tests := []struct {
		name      string
		headers   http.Header
		openAIKey string
		want      string
	}{
		{name: "no auth", want: "anthropic"},
		{name: "OpenAI bearer", headers: http.Header{"Authorization": {"Bearer sk-openai"}}, want: "openai"},
		{name: "Anthropic bearer", headers: http.Header{"Authorization": {"Bearer sk-ant-oat01-test"}}, want: "anthropic"},
		{name: "Anthropic key", headers: http.Header{"X-Api-Key": {"sk-ant-api"}}, want: "anthropic"},
		{name: "key wins over bearer", headers: http.Header{"Authorization": {"Bearer sk-openai"}, "X-Api-Key": {"sk-ant-api"}}, want: "anthropic"},
		{name: "configured OpenAI key", openAIKey: "configured", want: "openai"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := httptest.NewServer(NewHandler(HandlerOptions{
				AnthropicUpstream: anthropicURL, OpenAIUpstream: openAIURL, OpenAIAPIKey: tt.openAIKey,
			}))
			defer proxy.Close()
			got := requestRoute(t, proxy.Client(), proxy.URL+"/v1/models", tt.headers, seen)
			if got.provider != tt.want {
				t.Fatalf("provider = %q, want %q", got.provider, tt.want)
			}
		})
	}
}

func TestHandlerAppliesProviderCredentials(t *testing.T) {
	seen := make(chan routeObservation, 1)
	anthropic, anthropicURL := routeUpstream(t, "anthropic", seen)
	defer anthropic.Close()
	openAI, openAIURL := routeUpstream(t, "openai", seen)
	defer openAI.Close()
	authCalls := 0
	proxy := httptest.NewServer(NewHandler(HandlerOptions{
		AnthropicUpstream: anthropicURL,
		OpenAIUpstream:    openAIURL,
		APIKey:            "anthropic-key",
		AuthTokenFunc: func() string {
			authCalls++
			return "anthropic-token-" + strconv.Itoa(authCalls)
		},
		OpenAIAPIKey: "openai-key",
	}))
	defer proxy.Close()

	clientHeaders := http.Header{
		"Authorization":     {"Bearer client-token"},
		"X-Api-Key":         {"client-key"},
		"Anthropic-Version": {"2023-06-01"},
		"Anthropic-Beta":    {"test"},
	}
	got := requestRoute(t, proxy.Client(), proxy.URL+"/v1/responses", clientHeaders, seen)
	if got.provider != "openai" || got.header.Get("Authorization") != "Bearer openai-key" {
		t.Fatalf("OpenAI credentials = provider %q auth %q", got.provider, got.header.Get("Authorization"))
	}
	if got.header.Get("X-Api-Key") != "" || got.header.Get("Anthropic-Version") != "" || got.header.Get("Anthropic-Beta") != "" {
		t.Fatalf("Anthropic credentials leaked to OpenAI: %#v", got.header)
	}

	got = requestRoute(t, proxy.Client(), proxy.URL+"/v1/messages", clientHeaders, seen)
	if got.provider != "anthropic" || got.header.Get("X-Api-Key") != "anthropic-key" ||
		got.header.Get("Authorization") != "Bearer anthropic-token-1" {
		t.Fatalf("Anthropic credentials = provider %q key %q auth %q", got.provider, got.header.Get("X-Api-Key"), got.header.Get("Authorization"))
	}

	got = requestRoute(t, proxy.Client(), proxy.URL+"/openai/v1/responses", clientHeaders, seen)
	if got.provider != "anthropic" || got.header.Get("X-Api-Key") != "client-key" ||
		got.header.Get("Authorization") != "Bearer client-token" {
		t.Fatalf("prefixed passthrough credentials changed: provider %q key %q auth %q", got.provider, got.header.Get("X-Api-Key"), got.header.Get("Authorization"))
	}

	got = requestRoute(t, proxy.Client(), proxy.URL+"/anthropic/v1/messages", clientHeaders, seen)
	if got.header.Get("X-Api-Key") != "anthropic-key" || got.header.Get("Authorization") != "Bearer anthropic-token-2" || authCalls != 2 {
		t.Fatalf("prefixed Anthropic credentials = key %q auth %q", got.header.Get("X-Api-Key"), got.header.Get("Authorization"))
	}
}

func TestHandlerRewritesCustomProtocolPath(t *testing.T) {
	seen := make(chan routeObservation, 1)
	anthropic, anthropicURL := routeUpstream(t, "anthropic", seen)
	defer anthropic.Close()
	openAI, openAIURL := routeUpstream(t, "openai", seen)
	defer openAI.Close()
	proxy := httptest.NewServer(NewHandler(HandlerOptions{
		AnthropicUpstream: anthropicURL, OpenAIUpstream: openAIURL,
		ProtocolOf: func(path string) Protocol {
			if path == "/custom/chat" {
				return ProtocolOpenAIChat
			}
			return ProtocolNone
		},
		RewritePath: func(path string, protocol Protocol) string {
			if path == "/custom/chat" && protocol == ProtocolOpenAIChat {
				return "/v1/chat/completions"
			}
			return path
		},
	}))
	defer proxy.Close()

	got := requestRoute(t, proxy.Client(), proxy.URL+"/custom/chat?trace=1", nil, seen)
	if got.provider != "openai" || got.path != "/v1/chat/completions" || got.query != "trace=1" {
		t.Fatalf("custom route = %s %s?%s", got.provider, got.path, got.query)
	}
}
