package pxpipe

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
)

// HandlerOptions configures the embeddable reverse-proxy handler.
type HandlerOptions struct {
	// Upstream is the Anthropic API base. Default https://api.anthropic.com.
	Upstream *url.URL
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
}

const defaultMaxBodyBytes = 256 << 20

type handler struct {
	opts  HandlerOptions
	proxy *httputil.ReverseProxy
}

// NewHandler returns an http.Handler that forwards everything to the
// configured upstream, rewriting POST bodies on the Anthropic Messages routes
// through TransformAnthropicMessages. Responses (including SSE streams) pass
// through untouched.
func NewHandler(opts HandlerOptions) http.Handler {
	if opts.Upstream == nil {
		opts.Upstream = &url.URL{Scheme: "https", Host: "api.anthropic.com"}
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	h := &handler{opts: opts}
	h.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(opts.Upstream)
			pr.Out.Host = opts.Upstream.Host
		},
		Transport: opts.Transport,
		// Negative FlushInterval streams SSE tokens as they arrive.
		FlushInterval: -1,
	}
	return h
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

func (h *handler) transformFor(surface Protocol, body []byte) *TransformResult {
	var base TransformOptions
	if h.opts.Transform != nil {
		base = *h.opts.Transform
	}
	model := extractModel(body)
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
		res := h.transformFor(surface, body)
		if h.opts.OnResult != nil {
			h.opts.OnResult(r, res)
		}
		body = res.Body
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
