package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	pxpipe "github.com/evan-choi/pxpipe-go"
	"github.com/evan-choi/pxpipe-go/internal/mitm"
	"github.com/evan-choi/pxpipe-go/internal/runner"
)

// Run parses the CLI and returns the process exit status.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	command, exitCode := newRootCommand(stdin, stdout, stderr, func(p profile) (int, error) {
		return runProfile(context.Background(), p, stdin, stdout, stderr)
	})
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return *exitCode
}

func runProfile(ctx context.Context, p profile, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
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

	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transformListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1, fmt.Errorf("listen for transform server: %w", err)
	}
	transformURL := &url.URL{Scheme: "http", Host: transformListener.Addr().String()}
	transformServer := &http.Server{
		Handler:           pxpipe.NewHandler(p.handlerOptions(transport)),
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
	childDone := make(chan struct {
		code int
		err  error
	}, 1)
	childContext, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
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
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	proxyErr := proxy.Shutdown(shutdownContext)
	transformErr := transformServer.Shutdown(shutdownContext)
	if result.err == nil {
		result.err = errors.Join(proxyErr, transformErr)
	}
	return result.code, result.err
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
		return "", func() {}, fmt.Errorf("read existing NODE_EXTRA_CA_CERTS: %w", err)
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
