package mitm

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Options configures a provider-independent forward proxy.
type Options struct {
	Routes    []Route
	Authority *Authority
	Transport http.RoundTripper
}

// Proxy tunnels unrelated destinations and decrypts only hosts that could
// match a configured route.
type Proxy struct {
	routes       []Route
	authority    *Authority
	forwarder    *httputil.ReverseProxy
	server       *http.Server
	mitmServer   *http.Server
	mitmListener *connectionListener

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
		routes:       append([]Route(nil), options.Routes...),
		authority:    options.Authority,
		mitmListener: newConnectionListener(),
		connections:  make(map[net.Conn]struct{}),
	}
	p.forwarder = &httputil.ReverseProxy{
		Rewrite:       p.rewrite,
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "proxy upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.handleProxy), ReadHeaderTimeout: 15 * time.Second}
	p.mitmServer = &http.Server{
		Handler:           http.HandlerFunc(p.handleMITM),
		ReadHeaderTimeout: 15 * time.Second,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			if connection, ok := connection.(*authorityConnection); ok {
				return context.WithValue(ctx, connectAuthorityKey{}, connection.authority)
			}
			return ctx
		},
	}
	return p, nil
}

// Serve handles proxy traffic until Shutdown is called.
func (p *Proxy) Serve(listener net.Listener) error {
	if !isLoopback(listener.Addr()) {
		return errors.New("MITM proxy listener must be loopback")
	}
	go func() {
		_ = p.mitmServer.Serve(p.mitmListener)
	}()
	err := p.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting proxy traffic and drains active HTTP requests.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.closeConnections()
	err := p.server.Shutdown(ctx)
	p.mitmListener.Close()
	mitmErr := p.mitmServer.Shutdown(ctx)
	if err != nil {
		return err
	}
	return mitmErr
}

func (p *Proxy) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	if !r.URL.IsAbs() || (r.URL.Scheme != "http" && r.URL.Scheme != "https") {
		http.Error(w, "absolute HTTP(S) URL required", http.StatusBadRequest)
		return
	}
	p.forwarder.ServeHTTP(w, r)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT is not supported", http.StatusInternalServerError)
		return
	}
	if !hostCouldMatch(p.routes, r.Host) {
		p.tunnel(w, hijacker, r.Host)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		client.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		client.Close()
		return
	}
	client = withBufferedReader(client, buffered.Reader)
	client = p.trackConnection(client)
	host, _ := splitAuthority(r.Host)
	go func() {
		tlsClient := tlsServer(client, p.authority.tlsConfig(host))
		_ = tlsClient.SetDeadline(time.Now().Add(15 * time.Second))
		if err := tlsClient.Handshake(); err != nil {
			tlsClient.Close()
			return
		}
		_ = tlsClient.SetDeadline(time.Time{})
		connection := &authorityConnection{Conn: tlsClient, authority: r.Host}
		if !p.mitmListener.submit(connection) {
			tlsClient.Close()
		}
	}()
}

func (p *Proxy) tunnel(w http.ResponseWriter, hijacker http.Hijacker, authority string) {
	upstream, err := net.DialTimeout("tcp", authority, 15*time.Second)
	if err != nil {
		http.Error(w, "connect upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	client = withBufferedReader(client, buffered.Reader)
	client = p.trackConnection(client)
	upstream = p.trackConnection(upstream)
	go pipeConnections(client, upstream)
}

func (p *Proxy) handleMITM(w http.ResponseWriter, r *http.Request) {
	authority, _ := r.Context().Value(connectAuthorityKey{}).(string)
	if !sameTLSDestination(authority, r.Host) {
		http.Error(w, "request host does not match CONNECT authority", http.StatusMisdirectedRequest)
		return
	}
	p.forwarder.ServeHTTP(w, r)
}

func (p *Proxy) rewrite(request *httputil.ProxyRequest) {
	in := request.In
	originalHost := in.Host
	authority, _ := in.Context().Value(connectAuthorityKey{}).(string)
	matchHost := originalHost
	if authority != "" {
		matchHost = authority
	}
	path := in.URL.Path
	if path == "" {
		path = "/"
	}
	if route := matchingRoute(p.routes, matchHost, path); route != nil {
		request.Out.URL = routeURL(route, in.URL)
	} else if in.URL.IsAbs() {
		outURL := *in.URL
		request.Out.URL = &outURL
	} else {
		request.Out.URL = &url.URL{Scheme: "https", Host: matchHost, Path: in.URL.Path, RawQuery: in.URL.RawQuery}
	}
	request.Out.Host = originalHost
	request.Out.Header.Del("Proxy-Authorization")
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

type connectAuthorityKey struct{}

type authorityConnection struct {
	net.Conn
	authority string
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

type bufferedConnection struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConnection) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}

func (c *bufferedConnection) CloseWrite() error {
	if connection, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return connection.CloseWrite()
	}
	return c.Conn.Close()
}

func withBufferedReader(connection net.Conn, reader *bufio.Reader) net.Conn {
	if reader.Buffered() == 0 {
		return connection
	}
	return &bufferedConnection{Conn: connection, reader: io.MultiReader(reader, connection)}
}

func pipeConnections(left, right net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(left, right)
		closeWrite(left)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(right, left)
		closeWrite(right)
		done <- struct{}{}
	}()
	<-done
	<-done
	left.Close()
	right.Close()
}

func closeWrite(connection net.Conn) {
	if connection, ok := connection.(interface{ CloseWrite() error }); ok {
		_ = connection.CloseWrite()
		return
	}
	_ = connection.Close()
}

type connectionListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func newConnectionListener() *connectionListener {
	return &connectionListener{connections: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *connectionListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *connectionListener) submit(connection net.Conn) bool {
	select {
	case l.connections <- connection:
		return true
	case <-l.closed:
		return false
	}
}

func (l *connectionListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *connectionListener) Addr() net.Addr {
	return staticAddress("mitm")
}

type staticAddress string

func (a staticAddress) Network() string { return string(a) }
func (a staticAddress) String() string  { return string(a) }

type tlsConnection interface {
	net.Conn
	Handshake() error
}

var tlsServer = func(connection net.Conn, config *tls.Config) tlsConnection {
	return tls.Server(connection, config)
}
