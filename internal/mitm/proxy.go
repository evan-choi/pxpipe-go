package mitm

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
)

// Options configures a provider-independent forward proxy.
type Options struct {
	Routes    []Route
	Authority *Authority
	Transport http.RoundTripper
	// NonproxyHandler serves origin-form requests received on the proxy listener.
	NonproxyHandler http.Handler
}

// OriginalSchemeHeader carries the pre-rewrite request scheme over the private
// transform hop. Callers must remove it before forwarding upstream.
const OriginalSchemeHeader = "X-Pxpipe-Original-Scheme"

// Proxy tunnels unrelated destinations and decrypts hosts that could match a
// configured route. A wildcard route decrypts every CONNECT destination so its
// request path can be inspected.
type Proxy struct {
	routes    []Route
	transport http.RoundTripper
	server    *http.Server

	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	closing       bool
}

// NewProxy validates options and constructs a proxy. Call Serve with a
// loopback listener owned by the caller.
func NewProxy(options Options) (*Proxy, error) {
	if options.Authority == nil {
		return nil, errors.New("MITM authority is required")
	}
	if err := validateRoutes(options.Routes); err != nil {
		return nil, err
	}
	transport := options.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	p := &Proxy{
		routes:      append([]Route(nil), options.Routes...),
		transport:   transport,
		connections: make(map[net.Conn]struct{}),
	}

	engine := goproxy.NewProxyHttpServer()
	if options.NonproxyHandler != nil {
		engine.NonproxyHandler = options.NonproxyHandler
	}
	engine.Logger = log.New(io.Discard, "", 0)
	engine.KeepAcceptEncoding = true
	engine.AllowHTTP2 = true
	engine.ConnectDial = nil
	if secureTransport, ok := transport.(*http.Transport); ok {
		// goproxy's default transport skips upstream certificate verification.
		engine.Tr = secureTransport
	} else {
		engine.Tr = secureDefaultTransport()
	}
	mitmAction := &goproxy.ConnectAction{
		Action: goproxy.ConnectMitm,
		TLSConfig: func(authority string, _ *goproxy.ProxyCtx) (*tls.Config, error) {
			host, _ := splitAuthority(authority)
			config := options.Authority.TLSConfig(host)
			config.NextProtos = []string{"h2", "http/1.1"}
			return config, nil
		},
	}
	engine.OnRequest().HandleConnectFunc(func(authority string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		if !hostCouldMatch(p.routes, authority) {
			return goproxy.OkConnect, authority
		}
		ctx.UserData = authority
		return mitmAction, authority
	})
	engine.OnRequest().DoFunc(p.handleRequest)
	p.server = &http.Server{Handler: engine, ReadHeaderTimeout: 15 * time.Second}
	return p, nil
}

func secureDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

// Serve handles proxy traffic until Shutdown is called.
func (p *Proxy) Serve(listener net.Listener) error {
	if !isLoopback(listener.Addr()) {
		return errors.New("MITM proxy listener must be loopback")
	}
	err := p.server.Serve(&trackingListener{Listener: listener, proxy: p})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting proxy traffic and closes hijacked tunnels that
// net/http cannot drain itself.
func (p *Proxy) Shutdown(ctx context.Context) error {
	err := p.server.Shutdown(ctx)
	p.closeConnections()
	return err
}

// Close immediately stops the proxy and closes hijacked tunnels that net/http
// does not own after CONNECT handling.
func (p *Proxy) Close() error {
	err := p.server.Close()
	p.closeConnections()
	return err
}

func (p *Proxy) handleRequest(request *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	ctx.RoundTripper = proxyRoundTripper{transport: p.transport}
	authority, _ := ctx.UserData.(string)
	if authority != "" && !sameTLSDestination(authority, request.Host) {
		return request, goproxy.NewResponse(
			request,
			goproxy.ContentTypeText,
			http.StatusMisdirectedRequest,
			"request host does not match CONNECT authority\n",
		)
	}

	matchHost := request.Host
	if authority != "" {
		matchHost = authority
	}
	path := request.URL.Path
	if path == "" {
		path = "/"
	}
	request.Header.Del(OriginalSchemeHeader)
	if route := matchingRoute(p.routes, matchHost, path); route != nil {
		scheme := request.URL.Scheme
		if authority != "" {
			scheme = "https"
		} else if scheme == "" {
			scheme = "http"
		}
		request.Header.Set(OriginalSchemeHeader, scheme)
		request.URL = routeURL(route, request.URL)
	}
	request.Header.Del("Proxy-Authorization")
	return request, nil
}

type proxyRoundTripper struct {
	transport http.RoundTripper
}

func (r proxyRoundTripper) RoundTrip(request *http.Request, _ *goproxy.ProxyCtx) (*http.Response, error) {
	response, err := r.transport.RoundTrip(request)
	if err == nil {
		return response, nil
	}
	return goproxy.NewResponse(
		request,
		goproxy.ContentTypeText,
		http.StatusBadGateway,
		"proxy upstream error: "+err.Error()+"\n",
	), nil
}

func sameTLSDestination(authority, requestHost string) bool {
	authorityHost, authorityPort := splitAuthority(authority)
	requestHostname, requestPort := splitAuthority(requestHost)
	if authorityPort == "" {
		authorityPort = "443"
	}
	if requestPort == "" {
		requestPort = "443"
	}
	return strings.EqualFold(authorityHost, requestHostname) && authorityPort == requestPort
}

func (p *Proxy) trackConnection(connection net.Conn) net.Conn {
	tracked := &trackedConnection{Conn: connection}
	tracked.onClose = func() {
		p.connectionsMu.Lock()
		delete(p.connections, tracked)
		p.connectionsMu.Unlock()
	}
	p.connectionsMu.Lock()
	if p.closing {
		p.connectionsMu.Unlock()
		_ = tracked.Close()
		return tracked
	}
	p.connections[tracked] = struct{}{}
	p.connectionsMu.Unlock()
	return tracked
}

func (p *Proxy) closeConnections() {
	p.connectionsMu.Lock()
	p.closing = true
	connections := make([]net.Conn, 0, len(p.connections))
	for connection := range p.connections {
		connections = append(connections, connection)
	}
	p.connectionsMu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

type trackingListener struct {
	net.Listener
	proxy *Proxy
}

func (l *trackingListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.proxy.trackConnection(connection), nil
}

type trackedConnection struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *trackedConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.onClose)
	return err
}

func (c *trackedConnection) CloseRead() error {
	if connection, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return connection.CloseRead()
	}
	return nil
}

func (c *trackedConnection) CloseWrite() error {
	if connection, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return connection.CloseWrite()
	}
	return c.Close()
}

func isLoopback(address net.Addr) bool {
	tcpAddress, ok := address.(*net.TCPAddr)
	return ok && tcpAddress.IP.IsLoopback()
}
