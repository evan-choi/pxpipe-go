package pxpipe

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

// HandlerOptions configures the embeddable reverse-proxy handler.
type HandlerOptions struct {
	// Upstream is the Anthropic API base. Default https://api.anthropic.com.
	Upstream *url.URL
	// OpenAIUpstream is the OpenAI API base. Default https://api.openai.com.
	OpenAIUpstream *url.URL
	// APIKey overrides or supplies the Anthropic x-api-key header.
	APIKey string
	// AuthToken overrides or supplies the Anthropic authorization bearer.
	AuthToken string
	// AuthTokenFunc resolves the Anthropic authorization bearer per request.
	// When set, it takes precedence over AuthToken.
	AuthTokenFunc func() string
	// OpenAIAPIKey overrides or supplies the OpenAI authorization bearer.
	OpenAIAPIKey string
	// Transform supplies per-request transform options; Model is filled from
	// the request body. Nil = defaults.
	Transform *TransformOptions
	// Transport is used for upstream requests. Nil = http.DefaultTransport.
	Transport http.RoundTripper
	// OnResult observes every transform outcome (nil-safe). Called before the
	// request is forwarded; the result must not be mutated.
	OnResult func(r *http.Request, res *TransformResult)
	// MaxBodyBytes caps buffered request bodies (0 = 256 MiB default). Bodies
	// over the cap pass through untransformed.
	MaxBodyBytes int64
	// ProtocolOf overrides wire-protocol detection by request path. Nil uses
	// DefaultProtocolOf. Return ProtocolNone to pass a request through
	// untransformed.
	ProtocolOf func(path string) Protocol
	// RewritePath optionally maps the outbound path after protocol detection and
	// before upstream selection. It is useful with custom ProtocolOf routes.
	RewritePath func(path string, protocol Protocol) string
	// UpstreamHeadersTimeout aborts when upstream response headers do not arrive.
	// Nil defaults to 5 minutes; zero or a negative duration disables it.
	UpstreamHeadersTimeout *time.Duration
	// UpstreamIdleTimeout aborts a response stream after no bytes arrive.
	// Nil defaults to 2 minutes; zero or a negative duration disables it.
	UpstreamIdleTimeout *time.Duration
	// DuplicateHold rejects an identical in-flight request during this window.
	// Nil defaults to 1 minute; zero or a negative duration disables it.
	DuplicateHold *time.Duration
}

const defaultMaxBodyBytes = 256 << 20

type handler struct {
	opts  HandlerOptions
	proxy *httputil.ReverseProxy
}

type protocolContextKey struct{}

var passthroughPrefixes = []string{
	"/anthropic/",
	"/openai/",
	"/google-ai-studio/",
	"/compat/",
}

// NewHandler returns an http.Handler that forwards everything to the
// configured provider upstream, rewriting POST bodies on supported Anthropic
// and OpenAI routes. Responses (including SSE streams) pass through untouched.
func NewHandler(opts HandlerOptions) http.Handler {
	if opts.Upstream == nil {
		opts.Upstream = &url.URL{Scheme: "https", Host: "api.anthropic.com"}
	}
	if opts.OpenAIUpstream == nil {
		opts.OpenAIUpstream = &url.URL{Scheme: "https", Host: "api.openai.com"}
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	h := &handler{opts: opts}
	transport := newReliabilityTransport(opts.Transport, resolveReliabilityConfig(opts))
	h.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			protocol, _ := pr.In.Context().Value(protocolContextKey{}).(Protocol)
			originalPath := pr.In.URL.Path
			outPath := originalPath
			if opts.RewritePath != nil {
				outPath = opts.RewritePath(originalPath, protocol)
				pr.Out.URL.Path = outPath
				pr.Out.URL.RawPath = ""
			}
			providerPrefixed := isProviderPrefixedPath(originalPath)
			openAI := !providerPrefixed && isCanonicalOpenAIPath(outPath, pr.In.Header, opts.OpenAIAPIKey != "")
			target := opts.Upstream
			if openAI {
				target = opts.OpenAIUpstream
			}
			pr.SetURL(target)
			pr.Out.Host = target.Host
			if openAI {
				pr.Out.Header.Del("X-Api-Key")
				for name := range pr.Out.Header {
					if strings.HasPrefix(strings.ToLower(name), "anthropic-") {
						pr.Out.Header.Del(name)
					}
				}
				if opts.OpenAIAPIKey != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+opts.OpenAIAPIKey)
				}
			} else if !providerPrefixed || strings.HasPrefix(originalPath, "/anthropic/") {
				if opts.APIKey != "" {
					pr.Out.Header.Set("X-Api-Key", opts.APIKey)
				}
				if token := resolveAuthToken(opts); token != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+token)
				}
			}
		},
		Transport:    transport,
		ErrorHandler: proxyErrorHandler,
		// Negative FlushInterval streams SSE tokens as they arrive.
		FlushInterval: -1,
	}
	return h
}

func isProviderPrefixedPath(pathname string) bool {
	for _, prefix := range passthroughPrefixes {
		if strings.HasPrefix(pathname, prefix) {
			return true
		}
	}
	return false
}

func isCanonicalOpenAIPath(pathname string, headers http.Header, hasOpenAIKey bool) bool {
	isModelsPath := pathname == "/v1/models" || strings.HasPrefix(pathname, "/v1/models/")
	auth := strings.Fields(headers.Get("Authorization"))
	bearerIsAnthropic := len(auth) >= 2 && strings.EqualFold(auth[0], "Bearer") &&
		strings.HasPrefix(strings.ToLower(auth[1]), "sk-ant-")
	looksOpenAIAuth := hasOpenAIKey ||
		(len(headers.Values("Authorization")) > 0 && len(headers.Values("X-Api-Key")) == 0 && !bearerIsAnthropic)
	return pathname == "/v1/chat/completions" ||
		pathname == "/v1/responses" ||
		strings.HasPrefix(pathname, "/v1/responses/") ||
		(isModelsPath && looksOpenAIAuth)
}

func resolveAuthToken(opts HandlerOptions) string {
	if opts.AuthTokenFunc != nil {
		return opts.AuthTokenFunc()
	}
	return opts.AuthToken
}

func extractModel(body []byte) string {
	node, err := sonic.Get(body, "model")
	if err != nil {
		return ""
	}
	model, err := node.String()
	if err != nil {
		return ""
	}
	return model
}

// Protocol identifies the wire protocol of a request body, i.e. which API
// shape pxpipe should transform it as.
type Protocol int

const (
	// ProtocolNone marks requests pxpipe forwards untransformed.
	ProtocolNone Protocol = iota
	// ProtocolAnthropicMessages is the Anthropic Messages API.
	ProtocolAnthropicMessages
	// ProtocolOpenAIChat is the OpenAI Chat Completions API.
	ProtocolOpenAIChat
	// ProtocolOpenAIResponses is the OpenAI Responses API.
	ProtocolOpenAIResponses
)

// DefaultProtocolOf is the built-in path matcher used when
// HandlerOptions.ProtocolOf is nil. It recognizes the Anthropic Messages
// routes plus the OpenAI Chat Completions / Responses routes (with one
// optional gateway/provider prefix segment).
func DefaultProtocolOf(pathname string) Protocol {
	switch {
	case IsAnthropicMessagesPath(pathname):
		return ProtocolAnthropicMessages
	case IsOpenAIChatPath(pathname):
		return ProtocolOpenAIChat
	case IsOpenAIResponsesPath(pathname):
		return ProtocolOpenAIResponses
	}
	return ProtocolNone
}

func (h *handler) transformFor(surface Protocol, body []byte, model string) *TransformResult {
	var base TransformOptions
	if h.opts.Transform != nil {
		base = *h.opts.Transform
	}
	if surface == ProtocolAnthropicMessages {
		return TransformAnthropicMessages(TransformInput{Body: body, Model: model, Options: &base})
	}
	res := &TransformResult{}
	if !IsSupportedGptModel(model) {
		res.Body = body
		res.Reason = ReasonUnsupportedModel
		res.Detail = model
		res.Info = &TransformInfo{Reason: "unsupported_model"}
		return res
	}
	base.Model = model
	var out []byte
	var info *TransformInfo
	if surface == ProtocolOpenAIChat {
		out, info = TransformOpenAIChatCompletions(body, &base)
	} else {
		out, info = TransformOpenAIResponses(body, &base)
	}
	res.Body = out
	res.Applied = info.Compressed
	res.Reason = classifyReason(info)
	res.Detail = info.Reason
	res.Info = info
	return res
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	protocolOf := h.opts.ProtocolOf
	if protocolOf == nil {
		protocolOf = DefaultProtocolOf
	}
	surface := protocolOf(r.URL.Path)
	r = r.WithContext(context.WithValue(r.Context(), protocolContextKey{}, surface))
	bypassValue, hasBypass := r.Header[http.CanonicalHeaderKey("X-Pxpipe-Bypass")]
	r.Header.Del("X-Pxpipe-Bypass")
	bypass := hasBypass && len(bypassValue) > 0 &&
		!falseyBypassValue(bypassValue[0])
	if !bypass && r.Method == http.MethodPost && surface != ProtocolNone && r.Body != nil &&
		(r.ContentLength < 0 || r.ContentLength <= h.opts.MaxBodyBytes) {
		originalBody := r.Body
		body, err := io.ReadAll(io.LimitReader(originalBody, h.opts.MaxBodyBytes+1))
		if err != nil {
			originalBody.Close()
			http.Error(w, "pxpipe: failed to read request body", http.StatusBadGateway)
			return
		}
		if int64(len(body)) > h.opts.MaxBodyBytes {
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(body), originalBody), originalBody}
			h.proxy.ServeHTTP(w, r)
			return
		}
		originalBody.Close()
		model := extractModel(body)
		res := h.transformFor(surface, body, model)
		body = res.Body
		limitMessage, overLimit := applySerializedRequestLimit(res, model)
		if h.opts.OnResult != nil {
			h.opts.OnResult(r, res)
		}
		if overLimit {
			writeProtocolError(w, surface, http.StatusRequestEntityTooLarge, "request_too_large", limitMessage)
			return
		}
		r = withBodyDigest(r, body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
		r.Header.Del("Transfer-Encoding")
		r.TransferEncoding = nil
	}
	h.proxy.ServeHTTP(w, r)
}

func falseyBypassValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}
