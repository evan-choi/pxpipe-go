package app

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	pxpipe "github.com/evan-choi/pxpipe-go"
)

const defaultServePort = 47821

type requestObservationKey struct{}

type requestObservation struct {
	endpoint string
	model    string
	applied  bool
	info     transformEstimate
	response pxpipe.ResponseResult
}

type transformEstimate struct {
	compressedChars     int
	imagePixels         int
	imageTokens         int
	baselineImageTokens int
	nativeTokens        int
}

func runServer(ctx context.Context, port int, stdout io.Writer) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}
	defer listener.Close()

	logger := newRequestLog(stdout, writerIsTerminal(stdout))
	server := &http.Server{
		Handler:           newServeHandler(logger, pxpipe.HandlerOptions{}),
		ReadHeaderTimeout: 15 * time.Second,
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(stdout, "Listening on 127.0.0.1:%d\n", actualPort)
	fmt.Fprintf(stdout, "ANTHROPIC_BASE_URL=http://localhost:%d claude\n", actualPort)
	fmt.Fprintf(stdout, "OPENAI_BASE_URL=http://localhost:%d/v1 codex\n", actualPort)
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- serve(server, listener) }()

	select {
	case err := <-serveErrors:
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}
}

func newServeHandler(logger *requestLog, opts pxpipe.HandlerOptions) http.Handler {
	opts.OnResult = func(r *http.Request, result *pxpipe.TransformResult) {
		observation, _ := r.Context().Value(requestObservationKey{}).(*requestObservation)
		if observation == nil || result == nil {
			return
		}
		observation.model = result.Model
		observation.applied = result.Applied
		if result.Info != nil {
			observation.info = transformEstimate{
				compressedChars:     result.Info.CompressedChars,
				imagePixels:         result.Info.ImagePixels,
				imageTokens:         result.Info.ImageTokens,
				baselineImageTokens: result.Info.BaselineImagedTokens,
				nativeTokens:        result.Info.NativeInjectedTokens,
			}
		}
	}
	opts.OnResponseComplete = func(r *http.Request, result pxpipe.ResponseResult) {
		observation, _ := r.Context().Value(requestObservationKey{}).(*requestObservation)
		if observation != nil {
			observation.response = result
		}
	}
	proxy := pxpipe.NewHandler(opts)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observation := &requestObservation{endpoint: r.URL.Path}
		recorder := &statusResponseWriter{ResponseWriter: w}
		if strings.HasSuffix(r.URL.Path, "/messages") {
			// Usage accounting observes the response without changing its bytes, so
			// request an uncompressed Anthropic stream rather than decoding in-proxy.
			r.Header.Set("Accept-Encoding", "identity")
		}
		proxy.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), requestObservationKey{}, observation)))
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
			if r.Header.Get("Upgrade") != "" {
				status = http.StatusSwitchingProtocols
			}
		}
		if observation.response.StatusCode != 0 {
			status = observation.response.StatusCode
		}
		logger.add(observation.row(status))
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (o *requestObservation) row(status int) requestLogRow {
	row := requestLogRow{
		status:   status,
		endpoint: o.endpoint,
		model:    o.model,
		sentAs:   "text",
	}
	if o.model == "" {
		row.sentAs = "-"
	} else if o.applied {
		row.sentAs = "image"
	}
	usage := o.response.Usage
	if usage == nil {
		return row
	}
	row.cacheHits = int64Pointer(usage.CacheReadInputTokens)
	actual := int64(math.Round(float64(usage.InputTokens) +
		1.25*float64(usage.CacheCreationInputTokens) +
		0.1*float64(usage.CacheReadInputTokens)))
	row.sent = int64Pointer(actual)
	saving, known := o.estimatedSaving()
	if !o.applied {
		saving, known = 0, true
	}
	if known {
		row.asText = int64Pointer(actual + saving)
		row.saved = int64Pointer(saving)
	}
	return row
}

func (o *requestObservation) estimatedSaving() (int64, bool) {
	if o.info.baselineImageTokens > 0 || o.info.imageTokens > 0 {
		return int64(o.info.baselineImageTokens - o.info.imageTokens - o.info.nativeTokens), true
	}
	if o.info.compressedChars <= 0 || o.info.imagePixels <= 0 {
		return 0, false
	}
	textTokens := float64(o.info.compressedChars) / 4
	imageTokens := float64(o.info.imagePixels) / (28 * 28)
	return int64(math.Round(textTokens - imageTokens)), true
}

func int64Pointer(value int64) *int64 { return &value }

type requestLogRow struct {
	status    int
	endpoint  string
	model     string
	sentAs    string
	cacheHits *int64
	asText    *int64
	sent      *int64
	saved     *int64
}

type requestLog struct {
	mu            sync.Mutex
	out           io.Writer
	terminal      bool
	rows          []requestLogRow
	renderedLines int
	wroteHeader   bool
}

func newRequestLog(out io.Writer, terminal bool) *requestLog {
	return &requestLog{out: out, terminal: terminal}
}

func (l *requestLog) add(row requestLogRow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, row)
	if len(l.rows) > 20 {
		l.rows = l.rows[len(l.rows)-20:]
	}
	if !l.terminal {
		if !l.wroteHeader {
			fmt.Fprintln(l.out)
			fmt.Fprintln(l.out, requestLogHeader())
			fmt.Fprintln(l.out, requestLogSeparator())
			l.wroteHeader = true
		}
		fmt.Fprintln(l.out, formatRequestLogRow(row))
		return
	}
	if l.renderedLines > 0 {
		fmt.Fprintf(l.out, "\x1b[%dA\r\x1b[J", l.renderedLines)
	} else {
		fmt.Fprintln(l.out)
	}
	fmt.Fprintln(l.out, requestLogHeader())
	fmt.Fprintln(l.out, requestLogSeparator())
	for _, recent := range l.rows {
		fmt.Fprintln(l.out, formatRequestLogRow(recent))
	}
	l.renderedLines = len(l.rows) + 2
}

func requestLogHeader() string {
	return fmt.Sprintf("%-6s  %-20s  %-26s  %-7s  %10s  %10s  %10s  %11s",
		"Result", "Endpoint", "Model", "Sent as", "Cache hits", "As text", "Sent", "Saved/lost")
}

func requestLogSeparator() string {
	return strings.Repeat("-", len(requestLogHeader()))
}

func formatRequestLogRow(row requestLogRow) string {
	return fmt.Sprintf("%-6d  %-20s  %-26s  %-7s  %10s  %10s  %10s  %11s",
		row.status,
		truncateMiddle(row.endpoint, 20),
		truncateMiddle(valueOrDash(row.model), 26),
		row.sentAs,
		formatMetric(row.cacheHits),
		formatMetric(row.asText),
		formatMetric(row.sent),
		formatMetric(row.saved),
	)
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func formatMetric(value *int64) string {
	if value == nil {
		return "-"
	}
	return formatInteger(*value)
}

func formatInteger(value int64) string {
	digits := strconv.FormatInt(value, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign = "-"
		digits = digits[1:]
	}
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return sign + digits
}

func truncateMiddle(value string, width int) string {
	runes := []rune(sanitizeCell(value))
	if len(runes) <= width {
		return string(runes)
	}
	left := (width - 3) / 2
	right := width - 3 - left
	return string(runes[:left]) + "..." + string(runes[len(runes)-right:])
}

func sanitizeCell(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

func writerIsTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
