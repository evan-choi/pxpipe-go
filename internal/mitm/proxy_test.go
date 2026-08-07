package mitm

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProxyRoutesMatchingPathAndPreservesOtherTraffic(t *testing.T) {
	targetRequests := make(chan *http.Request, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests <- r.Clone(r.Context())
		_, _ = io.WriteString(w, "routed")
	}))
	defer target.Close()
	targetURL, _ := url.Parse(target.URL)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "origin")
	}))
	defer origin.Close()
	rawOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "raw")
	}))
	defer rawOrigin.Close()

	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	originTransport := origin.Client().Transport.(*http.Transport).Clone()
	originTransport.TLSClientConfig.RootCAs.AddCert(rawOrigin.Certificate())
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: origin.Listener.Addr().String(), PathPrefix: "/match", Upstream: targetURL}},
		Authority: authority,
		Transport: originTransport,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- proxy.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
		if err := <-serveDone; err != nil {
			t.Errorf("serve proxy: %v", err)
		}
	})

	proxyURL, _ := url.Parse("http://" + listener.Addr().String())
	rootPEM, err := os.ReadFile(authority.CertificatePath())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatal("load proxy CA")
	}
	roots.AddCert(origin.Certificate())
	roots.AddCert(rawOrigin.Certificate())
	client := &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{RootCAs: roots},
		ForceAttemptHTTP2: false,
	}, Timeout: 3 * time.Second}

	if got := requestBody(t, client, origin.URL+"/match?trace=1"); got != "routed" {
		t.Fatalf("matching response = %q", got)
	}
	seen := <-targetRequests
	if seen.Host != origin.Listener.Addr().String() || seen.URL.Path != "/match" || seen.URL.RawQuery != "trace=1" {
		t.Fatalf("routed request = host %q URL %q", seen.Host, seen.URL.String())
	}
	if got := seen.Header.Get(OriginalSchemeHeader); got != "https" {
		t.Fatalf("original scheme header = %q", got)
	}
	if got := requestBody(t, client, origin.URL+"/other"); got != "origin" {
		t.Fatalf("same-host fallback response = %q", got)
	}
	response, err := client.Get(rawOrigin.URL + "/other")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "raw" {
		t.Fatalf("raw tunnel response = %q", body)
	}
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 ||
		!response.TLS.PeerCertificates[0].Equal(rawOrigin.Certificate()) {
		t.Fatal("unrelated host was not kept as end-to-end TLS")
	}
}

func TestProxyStreamsRoutedResponses(t *testing.T) {
	firstChunk := make(chan struct{})
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(firstChunk)
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer target.Close()
	defer close(release)
	targetURL, _ := url.Parse(target.URL)

	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	defer origin.Close()
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: origin.Listener.Addr().String(), PathPrefix: "/stream", Upstream: targetURL}},
		Authority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = proxy.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	})

	proxyURL, _ := url.Parse("http://" + listener.Addr().String())
	roots := x509.NewCertPool()
	roots.AddCert(authority.certificate)
	client := &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{RootCAs: roots},
		ForceAttemptHTTP2: false,
	}, Timeout: 2 * time.Second}
	response, err := client.Get(origin.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-firstChunk:
	case <-time.After(time.Second):
		t.Fatal("routed stream did not reach target")
	}
	buffer := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "data: first\n\n" {
		t.Fatalf("first streamed chunk = %q", buffer)
	}
}

func TestProxySupportsHTTP2InsideMITMTunnel(t *testing.T) {
	firstChunk := make(chan struct{})
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(firstChunk)
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer target.Close()
	defer close(release)
	targetURL, _ := url.Parse(target.URL)
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	defer origin.Close()

	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: origin.Listener.Addr().String(), PathPrefix: "/h2", Upstream: targetURL}},
		Authority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve proxy: %v", err)
		}
	})

	proxyURL, _ := url.Parse("http://" + listener.Addr().String())
	roots := x509.NewCertPool()
	roots.AddCert(authority.certificate)
	client := &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{RootCAs: roots},
		ForceAttemptHTTP2: true,
	}, Timeout: 3 * time.Second}
	response, err := client.Get(origin.URL + "/h2")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("response protocol = %s, want HTTP/2", response.Proto)
	}
	select {
	case <-firstChunk:
	case <-time.After(time.Second):
		t.Fatal("HTTP/2 stream did not reach target")
	}
	buffer := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "data: first\n\n" {
		t.Fatalf("first streamed chunk = %q", buffer)
	}
}

func TestProxyShutdownClosesStalledMITMHandshake(t *testing.T) {
	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse("http://127.0.0.1:1")
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: "route.test", PathPrefix: "/", Upstream: upstream}},
		Authority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = io.WriteString(client, "CONNECT route.test:443 HTTP/1.1\r\nHost: route.test:443\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("stalled hijacked connection remained open after shutdown")
	}
}

func TestProxyCloseClosesStalledMITMHandshake(t *testing.T) {
	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse("http://127.0.0.1:1")
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: "route.test", PathPrefix: "/", Upstream: upstream}},
		Authority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = io.WriteString(client, "CONNECT route.test:443 HTTP/1.1\r\nHost: route.test:443\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("stalled hijacked connection remained open after close")
	}
}

func TestProxyShutdownClosesIdleRawTunnel(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	originConnection := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := origin.Accept()
		if acceptErr == nil {
			originConnection <- connection
		}
	}()

	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse("http://127.0.0.1:1")
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: "route.test", PathPrefix: "/", Upstream: upstream}},
		Authority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", origin.Addr(), origin.Addr())
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	var accepted net.Conn
	select {
	case accepted = <-originConnection:
	case <-time.After(time.Second):
		t.Fatal("raw upstream was not connected")
	}
	defer accepted.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = accepted.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := accepted.Read(make([]byte, 1)); err == nil {
		t.Fatal("raw upstream remained open after shutdown")
	}
}

func TestRawTunnelPreservesResponseAfterClientHalfClose(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	originDone := make(chan error, 1)
	go func() {
		connection, err := origin.Accept()
		if err != nil {
			originDone <- err
			return
		}
		defer connection.Close()
		body, err := io.ReadAll(connection)
		if err == nil && string(body) != "request" {
			err = fmt.Errorf("request body = %q", body)
		}
		if err == nil {
			_, err = io.WriteString(connection, "response")
		}
		originDone <- err
	}()

	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse("http://127.0.0.1:1")
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: "route.test", PathPrefix: "/", Upstream: upstream}},
		Authority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = proxy.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	})

	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", origin.Addr(), origin.Addr())
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if _, err := io.WriteString(client, "request"); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(responseBody) != "response" {
		t.Fatalf("response after half-close = %q", responseBody)
	}
	if err := <-originDone; err != nil {
		t.Fatal(err)
	}
}

func TestTLSDestinationIdentity(t *testing.T) {
	if !sameTLSDestination("example.com:443", "EXAMPLE.com") {
		t.Fatal("default TLS port should match an omitted request port")
	}
	if sameTLSDestination("example.com:8443", "example.com") || sameTLSDestination("example.com:443", "other.com") {
		t.Fatal("different CONNECT destinations matched")
	}
	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.tlsConfig("example.com").GetCertificate(&tls.ClientHelloInfo{ServerName: "other.com"}); err == nil {
		t.Fatal("certificate was issued for an SNI name different from the CONNECT host")
	}
}

func TestProxyRejectsMismatchedMITMRequestHost(t *testing.T) {
	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse("http://127.0.0.1:1")
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: "route.test", PathPrefix: "/", Upstream: upstream}},
		Authority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve proxy: %v", err)
		}
	})

	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = io.WriteString(connection, "CONNECT route.test:443 HTTP/1.1\r\nHost: route.test:443\r\n\r\n")
	connectResponse, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	connectResponse.Body.Close()
	if connectResponse.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", connectResponse.StatusCode)
	}

	roots := x509.NewCertPool()
	roots.AddCert(authority.certificate)
	tlsConnection := tls.Client(connection, &tls.Config{ServerName: "route.test", RootCAs: roots})
	if err := tlsConnection.Handshake(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(tlsConnection, "GET / HTTP/1.1\r\nHost: other.test\r\nConnection: close\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(tlsConnection), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("mismatched host status = %d, want %d", response.StatusCode, http.StatusMisdirectedRequest)
	}
}

func TestProxyDefaultTransportVerifiesUpstreamTLS(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "unexpected")
	}))
	defer origin.Close()
	originURL, _ := url.Parse(origin.URL)

	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deadUpstream, _ := url.Parse("http://127.0.0.1:1")
	proxy, err := NewProxy(Options{
		Routes:    []Route{{Host: originURL.Host, PathPrefix: "/match", Upstream: deadUpstream}},
		Authority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve proxy: %v", err)
		}
	})

	proxyURL, _ := url.Parse("http://" + listener.Addr().String())
	roots := x509.NewCertPool()
	roots.AddCert(authority.certificate)
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: roots},
	}, Timeout: 2 * time.Second}
	response, err := client.Get(origin.URL + "/fallback")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("untrusted upstream status = %d, want %d", response.StatusCode, http.StatusBadGateway)
	}
}

func TestMITMPackageHasNoProviderKnowledge(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		content, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(content))
		for _, forbidden := range []string{"anthropic", "openai", "claude", "codex"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s contains provider-specific term %q", entry.Name(), forbidden)
			}
		}
	}
}

func requestBody(t *testing.T, client *http.Client, target string) string {
	t.Helper()
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
