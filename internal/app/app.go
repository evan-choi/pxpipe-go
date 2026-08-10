package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	pxpipe "github.com/evan-choi/pxpipe-go"
	"github.com/evan-choi/pxpipe-go/internal/mitm"
	"github.com/evan-choi/pxpipe-go/internal/runner"
)

// Run parses the CLI and returns the process exit status.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	command, exitCode := newRootCommand(
		stdin, stdout, stderr,
		func(p profile) (int, error) {
			return runProfile(context.Background(), p, stdin, stdout, stderr)
		},
		func(port int) error {
			ctx, force, stop := notifyServeSignals()
			defer stop()
			return runServer(ctx, force, port, stdin, stdout)
		},
	)
	command.SetArgs(normalizeCLIArgs(args))
	if err := command.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return *exitCode
}

func notifyServeSignals() (context.Context, <-chan struct{}, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	force := make(chan struct{}, 1)
	stopped := make(chan struct{})
	signal.Notify(signals, shutdownSignals()...)
	go func() {
		select {
		case <-signals:
			cancel()
		case <-stopped:
			return
		}
		select {
		case <-signals:
			force <- struct{}{}
		case <-stopped:
		}
	}()
	var once sync.Once
	return ctx, force, func() {
		once.Do(func() {
			signal.Stop(signals)
			close(stopped)
			cancel()
		})
	}
}

func runProfile(ctx context.Context, p profile, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if p.claudeDesktop {
		return runClaudeDesktopProfile(ctx, p, stdin, stdout, stderr)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return 1, fmt.Errorf("locate user config directory: %w", err)
	}
	authority, err := mitm.LoadOrCreateAuthority(filepath.Join(configDir, "pxpipe"))
	if err != nil {
		return 1, err
	}
	certificatePath, removeCertificateBundle, err := certificateBundle(
		filepath.Join(configDir, "pxpipe"), authority.CertificatePath(), os.Getenv(p.certificateEnvironment()),
	)
	if err != nil {
		return 1, err
	}
	defer removeCertificateBundle()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	var unixServer *http.Server
	var unixTransport *http.Transport
	var unixSocketPath string
	var unixErrors <-chan error
	removeUnixSocket := func() {}
	defer func() { removeUnixSocket() }()
	if p.kind == profileClaude && os.Getenv("ANTHROPIC_UNIX_SOCKET") != "" {
		unixListener, socketPath, removeSocket, listenErr := newUnixSocketListener()
		if listenErr != nil {
			return 1, listenErr
		}
		unixSocketPath = socketPath
		removeUnixSocket = removeSocket
		unixTransport = transport.Clone()
		unixTransport.Proxy = nil
		dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		upstreamSocket := os.Getenv("ANTHROPIC_UNIX_SOCKET")
		unixTransport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", upstreamSocket)
		}
		unixServer = &http.Server{
			Handler:           p.unixHandler(unixTransport),
			ReadHeaderTimeout: 15 * time.Second,
		}
		errorChannel := make(chan error, 1)
		unixErrors = errorChannel
		go func() { errorChannel <- serve(unixServer, unixListener) }()
	}
	transformListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1, fmt.Errorf("listen for transform server: %w", err)
	}
	transformPath, err := newTransformPath()
	if err != nil {
		transformListener.Close()
		return 1, err
	}
	transformURL := &url.URL{Scheme: "http", Host: transformListener.Addr().String(), Path: transformPath}
	summary := newRunSummary()
	transformServer := &http.Server{
		Handler: newObservedServeHandler(
			summary,
			p.handlerWithOptions(transport, transformPath, observedHandlerOptions(pxpipe.HandlerOptions{})),
			transformPath,
		),
		ReadHeaderTimeout: 15 * time.Second,
	}
	transformErrors := make(chan error, 1)
	go func() { transformErrors <- serve(transformServer, transformListener) }()

	proxy, err := mitm.NewProxy(mitm.Options{Routes: p.routes(transformURL), Authority: authority, Transport: transport})
	if err != nil {
		transformListener.Close()
		return 1, err
	}
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		transformListener.Close()
		return 1, fmt.Errorf("listen for MITM proxy: %w", err)
	}
	proxyErrors := make(chan error, 1)
	go func() { proxyErrors <- proxy.Serve(proxyListener) }()

	proxyURL := "http://" + proxyListener.Addr().String()
	set, unset := p.environment(proxyURL, certificatePath)
	if unixSocketPath != "" {
		set["ANTHROPIC_UNIX_SOCKET"] = unixSocketPath
	}
	childDone := make(chan struct {
		code int
		err  error
	}, 1)
	childContext, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	defer summary.write(stderr)
	go func() {
		code, runErr := runner.Run(childContext, runner.Options{
			Command: p.command, Args: p.args,
			Env:   runner.Environment(os.Environ(), set, unset),
			Stdin: stdin, Stdout: stdout, Stderr: stderr,
		})
		childDone <- struct {
			code int
			err  error
		}{code: code, err: runErr}
	}()

	var result struct {
		code int
		err  error
	}
	select {
	case result = <-childDone:
	case err := <-transformErrors:
		cancelChild()
		result = <-childDone
		result.code = 1
		result.err = fmt.Errorf("transform server stopped: %w", err)
	case err := <-proxyErrors:
		cancelChild()
		result = <-childDone
		result.code = 1
		result.err = fmt.Errorf("MITM proxy stopped: %w", err)
	case err := <-unixErrors:
		cancelChild()
		result = <-childDone
		result.code = 1
		result.err = fmt.Errorf("Unix socket proxy stopped: %w", err)
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	proxyErr := proxy.Shutdown(shutdownContext)
	transformErr := transformServer.Shutdown(shutdownContext)
	var unixErr error
	if unixServer != nil {
		unixErr = unixServer.Shutdown(shutdownContext)
		unixTransport.CloseIdleConnections()
	}
	if result.err == nil {
		result.err = errors.Join(proxyErr, transformErr, unixErr)
	}
	return result.code, result.err
}

func runClaudeDesktopProfile(ctx context.Context, p profile, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	upstream := &url.URL{Scheme: "https", Host: "api.anthropic.com"}
	if configured := os.Getenv("ANTHROPIC_BASE_URL"); configured != "" {
		parsed, err := url.Parse(configured)
		if err != nil {
			return 1, fmt.Errorf("parse ANTHROPIC_BASE_URL: %w", err)
		}
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return 1, errors.New("ANTHROPIC_BASE_URL must use http or https and include a host")
		}
		upstream = parsed
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return 1, fmt.Errorf("locate user config directory: %w", err)
	}
	authority, err := mitm.LoadOrCreateAuthority(filepath.Join(configDir, "pxpipe"))
	if err != nil {
		return 1, err
	}
	certificatePath, removeCertificateBundle, err := certificateBundle(
		filepath.Join(configDir, "pxpipe"), authority.CertificatePath(), os.Getenv("NODE_EXTRA_CA_CERTS"),
	)
	if err != nil {
		return 1, err
	}
	defer removeCertificateBundle()

	listener, socketPath, removeSocket, err := newUnixSocketListener()
	if err != nil {
		return 1, err
	}
	defer removeSocket()
	listener = claudeDesktopListener(listener, authority, upstream)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	summary := newRunSummary()
	server := &http.Server{
		Handler: newObservedServeHandler(
			summary,
			p.desktopHandlerWithOptions(transport, upstream, observedHandlerOptions(pxpipe.HandlerOptions{})),
			"",
		),
		ReadHeaderTimeout: 15 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- serve(server, listener) }()

	childDone := make(chan struct {
		code int
		err  error
	}, 1)
	childContext, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	defer summary.write(stderr)
	go func() {
		code, runErr := runner.Run(childContext, runner.Options{
			Command: p.command, Args: p.args,
			Env: runner.Environment(os.Environ(), map[string]string{
				"ANTHROPIC_UNIX_SOCKET": socketPath,
				"NODE_EXTRA_CA_CERTS":   certificatePath,
			}, nil),
			Stdin: stdin, Stdout: stdout, Stderr: stderr,
		})
		childDone <- struct {
			code int
			err  error
		}{code: code, err: runErr}
	}()

	var result struct {
		code int
		err  error
	}
	select {
	case result = <-childDone:
	case err := <-serverErrors:
		cancelChild()
		result = <-childDone
		result.code = 1
		result.err = fmt.Errorf("Unix socket proxy stopped: %w", err)
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownContext); result.err == nil {
		result.err = err
	}
	return result.code, result.err
}

func claudeDesktopListener(listener net.Listener, authority *mitm.Authority, upstream *url.URL) net.Listener {
	if upstream.Scheme != "https" {
		return listener
	}
	return tls.NewListener(listener, authority.TLSConfig(upstream.Hostname()))
}

func newTransformPath() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate transform route token: %w", err)
	}
	return "/" + hex.EncodeToString(token[:]), nil
}

func newUnixSocketListener() (net.Listener, string, func(), error) {
	tempDir := os.TempDir()
	if len(tempDir) > 40 {
		if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
			tempDir = "/tmp"
		}
	}
	dir, err := os.MkdirTemp(tempDir, "pxpipe-")
	if err != nil {
		return nil, "", func() {}, fmt.Errorf("create Unix socket directory: %w", err)
	}
	path := filepath.Join(dir, "claude.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, "", func() {}, fmt.Errorf("listen for Claude Unix socket: %w", err)
	}
	cleanup := func() {
		_ = listener.Close()
		_ = os.RemoveAll(dir)
	}
	return listener, path, cleanup, nil
}

func serve(server *http.Server, listener net.Listener) error {
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func certificateBundle(dir, authorityPath, extraPath string) (string, func(), error) {
	if extraPath == "" || extraPath == authorityPath {
		return authorityPath, func() {}, nil
	}
	extra, err := os.ReadFile(extraPath)
	if err != nil {
		return "", func() {}, fmt.Errorf("read existing CA bundle: %w", err)
	}
	authority, err := os.ReadFile(authorityPath)
	if err != nil {
		return "", func() {}, fmt.Errorf("read pxpipe CA certificate: %w", err)
	}
	file, err := os.CreateTemp(dir, ".child-ca-bundle-*.pem")
	if err != nil {
		return "", func() {}, fmt.Errorf("create child CA bundle: %w", err)
	}
	path := file.Name()
	remove := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		remove()
		return "", func() {}, fmt.Errorf("secure child CA bundle: %w", err)
	}
	content := make([]byte, 0, len(extra)+len(authority)+1)
	content = append(content, extra...)
	content = append(content, '\n')
	content = append(content, authority...)
	if _, err := file.Write(content); err != nil {
		file.Close()
		remove()
		return "", func() {}, fmt.Errorf("write child CA bundle: %w", err)
	}
	if err := file.Close(); err != nil {
		remove()
		return "", func() {}, fmt.Errorf("close child CA bundle: %w", err)
	}
	return path, remove, nil
}
