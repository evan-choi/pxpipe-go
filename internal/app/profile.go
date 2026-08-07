package app

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	pxpipe "github.com/evan-choi/pxpipe-go"
	"github.com/evan-choi/pxpipe-go/internal/mitm"
)

type profileKind uint8

const (
	profileClaude profileKind = iota
	profileOpenCode
	profileCodex
	profileGeneric
)

type profile struct {
	kind    profileKind
	command string
	args    []string
}

func claudeProfile(command string, args []string) profile {
	return newProfile(profileClaude, command, args)
}

func openCodeProfile(command string, args []string) profile {
	return newProfile(profileOpenCode, command, args)
}

func codexProfile(command string, args []string) profile {
	return newProfile(profileCodex, command, args)
}

func genericProfile(command string, args []string) profile {
	return newProfile(profileGeneric, command, args)
}

func newProfile(kind profileKind, command string, args []string) profile {
	return profile{
		kind:    kind,
		command: command,
		args:    append([]string(nil), args...),
	}
}

func (p profile) routes(transformUpstream *url.URL) []mitm.Route {
	if p.kind == profileClaude {
		return []mitm.Route{{Host: "*", PathSuffix: "/messages", Upstream: transformUpstream}}
	}
	routes := []mitm.Route{{Host: "*", PathSuffix: "/responses", Upstream: transformUpstream}}
	if p.kind != profileCodex {
		routes = append(routes,
			mitm.Route{Host: "*", PathSuffix: "/messages", Upstream: transformUpstream},
			mitm.Route{Host: "*", PathSuffix: "/chat/completions", Upstream: transformUpstream},
		)
	}
	return routes
}

type originalUpstreamKey struct{}

func (p profile) handler(transport http.RoundTripper, transformPath string) http.Handler {
	transformer := pxpipe.NewHandler(pxpipe.HandlerOptions{
		Transport:  transport,
		ProtocolOf: p.protocolOf,
		UpstreamFor: func(r *http.Request, _ pxpipe.Protocol) *url.URL {
			upstream, _ := r.Context().Value(originalUpstreamKey{}).(*url.URL)
			return upstream
		},
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ok := stripTransformPath(r.URL.Path, transformPath)
		if !ok {
			http.Error(w, "pxpipe: unmatched transform host", http.StatusMisdirectedRequest)
			return
		}
		scheme := r.Header.Get(mitm.OriginalSchemeHeader)
		r.Header.Del(mitm.OriginalSchemeHeader)
		if (scheme != "http" && scheme != "https") || r.Host == "" {
			http.Error(w, "pxpipe: invalid original upstream", http.StatusBadRequest)
			return
		}
		r.URL.Path = path
		r.URL.RawPath = ""
		upstream := &url.URL{Scheme: scheme, Host: r.Host}
		r = r.WithContext(context.WithValue(r.Context(), originalUpstreamKey{}, upstream))
		transformer.ServeHTTP(w, r)
	})
}

func (p profile) unixHandler(transport http.RoundTripper) http.Handler {
	return pxpipe.NewHandler(pxpipe.HandlerOptions{
		AnthropicUpstream: &url.URL{Scheme: "http", Host: "api.anthropic.com"},
		Transport:         transport,
		ProtocolOf:        p.protocolOf,
		UpstreamFor: func(r *http.Request, _ pxpipe.Protocol) *url.URL {
			host := r.Host
			if host == "" {
				host = "api.anthropic.com"
			}
			return &url.URL{Scheme: "http", Host: host}
		},
	})
}

func stripTransformPath(path, transformPath string) (string, bool) {
	if !strings.HasPrefix(path, transformPath+"/") {
		return "", false
	}
	return strings.TrimPrefix(path, transformPath), true
}

func (p profile) protocolOf(path string) pxpipe.Protocol {
	if p.kind == profileClaude {
		if strings.HasSuffix(path, "/messages") {
			return pxpipe.ProtocolAnthropicMessages
		}
		return pxpipe.ProtocolNone
	}
	if strings.HasSuffix(path, "/responses") {
		return pxpipe.ProtocolOpenAIResponses
	}
	if p.kind != profileCodex {
		switch {
		case strings.HasSuffix(path, "/chat/completions"):
			return pxpipe.ProtocolOpenAIChat
		case strings.HasSuffix(path, "/messages"):
			return pxpipe.ProtocolAnthropicMessages
		}
	}
	return pxpipe.ProtocolNone
}

func (p profile) certificateEnvironment() string {
	if p.kind == profileCodex {
		return "CODEX_CA_CERTIFICATE"
	}
	return "NODE_EXTRA_CA_CERTS"
}

func (p profile) environment(proxyURL, certificatePath string) (map[string]string, []string) {
	set := map[string]string{
		"HTTPS_PROXY":              proxyURL,
		"https_proxy":              proxyURL,
		"HTTP_PROXY":               proxyURL,
		"http_proxy":               proxyURL,
		p.certificateEnvironment(): certificatePath,
	}
	unset := []string{"NO_PROXY", "no_proxy"}
	if p.kind == profileOpenCode {
		set["OPENCODE_EXPERIMENTAL_WEBSOCKETS"] = "false"
	}
	return set, unset
}
