package pxpipe

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

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

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && IsAnthropicMessagesPath(r.URL.Path) && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, h.opts.MaxBodyBytes+1))
		if err != nil {
			http.Error(w, "pxpipe: failed to read request body", http.StatusBadGateway)
			return
		}
		r.Body.Close()
		if int64(len(body)) <= h.opts.MaxBodyBytes {
			var base TransformOptions
			if h.opts.Transform != nil {
				base = *h.opts.Transform
			}
			res := TransformAnthropicMessages(TransformInput{
				Body:    body,
				Model:   extractModel(body),
				Options: &base,
			})
			if h.opts.OnResult != nil {
				h.opts.OnResult(r, res)
			}
			body = res.Body
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
		r.Header.Del("Transfer-Encoding")
	}
	h.proxy.ServeHTTP(w, r)
}
