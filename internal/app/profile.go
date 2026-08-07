package app

import (
	"net/http"
	"net/url"

	pxpipe "github.com/evan-choi/pxpipe-go"
	"github.com/evan-choi/pxpipe-go/internal/mitm"
)

type profile struct {
	command string
	args    []string
}

func claudeProfile(command string, args []string) profile {
	return profile{command: command, args: append([]string(nil), args...)}
}

func (profile) routes(transformUpstream *url.URL) []mitm.Route {
	return []mitm.Route{{
		Host:       "api.anthropic.com",
		PathPrefix: "/v1/messages",
		Upstream:   transformUpstream,
	}}
}

func (profile) handlerOptions(transport *http.Transport) pxpipe.HandlerOptions {
	return pxpipe.HandlerOptions{
		AnthropicUpstream: &url.URL{Scheme: "https", Host: "api.anthropic.com"},
		Transport:         transport,
	}
}

func (profile) environment(proxyURL, certificatePath string) (map[string]string, []string) {
	return map[string]string{
		"HTTPS_PROXY":         proxyURL,
		"https_proxy":         proxyURL,
		"NODE_EXTRA_CA_CERTS": certificatePath,
	}, []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_UNIX_SOCKET", "NO_PROXY", "no_proxy"}
}
