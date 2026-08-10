package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	pxpipe "github.com/evan-choi/pxpipe-go"
	"github.com/evan-choi/pxpipe-go/internal/mitm"
	"github.com/mattn/go-runewidth"
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

func runServer(ctx context.Context, force <-chan struct{}, port int, stdin io.Reader, stdout io.Writer) error {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("locate user config directory: %w", err)
	}
	authority, err := mitm.LoadOrCreateAuthority(filepath.Join(configDir, "pxpipe"))
	if err != nil {
		return err
	}
	certificatePath, removeCertificateBundle, err := certificateBundle(
		filepath.Join(configDir, "pxpipe"), authority.CertificatePath(), os.Getenv("CODEX_CA_CERTIFICATE"),
	)
	if err != nil {
		return err
	}
	defer removeCertificateBundle()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}
	defer listener.Close()

	logger := newRequestLog(stdout, writerIsTerminal(stdout))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	proxy, err := newServeProxy(logger, authority, transport, listener.Addr().String())
	if err != nil {
		return err
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	var tuiDone <-chan error
	if logger.terminal {
		tuiDone = logger.startTUI(stdin, actualPort, certificatePath)
	} else {
		fmt.Fprintf(stdout, "Listening on 127.0.0.1:%d\n", actualPort)
		fmt.Fprintf(stdout, "ANTHROPIC_BASE_URL=http://localhost:%d claude\n", actualPort)
		fmt.Fprintln(stdout, newRequestTUIModel(actualPort, certificatePath).codexCommand())
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- proxy.Serve(listener) }()

	select {
	case err := <-serveErrors:
		return errors.Join(err, logger.stopTUI(tuiDone))
	case err := <-tuiDone:
		return errors.Join(err, shutdownServerAfterTUI(ctx, proxy))
	case <-ctx.Done():
		tuiErr := logger.stopTUI(tuiDone)
		return errors.Join(tuiErr, shutdownServerAfterSignal(proxy, force))
	}
}

func newServeProxy(logger *requestLog, authority *mitm.Authority, transport http.RoundTripper, address string) (*mitm.Proxy, error) {
	transformPath, err := newTransformPath()
	if err != nil {
		return nil, err
	}
	transformURL := &url.URL{Scheme: "http", Host: address, Path: transformPath}
	profile := codexProfile("codex", nil)
	transformHandler := newObservedServeHandler(
		logger,
		profile.handlerWithOptions(transport, transformPath, observedHandlerOptions(pxpipe.HandlerOptions{})),
		transformPath,
	)
	directHandler := newServeHandler(logger, pxpipe.HandlerOptions{Transport: transport})
	nonproxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, transformPath+"/") {
			transformHandler.ServeHTTP(w, r)
			return
		}
		r.Header.Del(mitm.OriginalSchemeHeader)
		directHandler.ServeHTTP(w, r)
	})
	return mitm.NewProxy(mitm.Options{
		Routes: profile.routes(transformURL), Authority: authority, Transport: transport,
		NonproxyHandler: nonproxyHandler,
	})
}

type serverLifecycle interface {
	Shutdown(context.Context) error
	Close() error
}

func shutdownServerAfterSignal(server serverLifecycle, force <-chan struct{}) error {
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Shutdown(shutdownContext) }()
	select {
	case err := <-done:
		return err
	case <-force:
		return server.Close()
	}
}

func shutdownServerAfterTUI(interrupt context.Context, server serverLifecycle) error {
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Shutdown(shutdownContext) }()
	select {
	case err := <-done:
		return err
	case <-interrupt.Done():
		return errors.Join(server.Close(), interrupt.Err())
	}
}

func observedHandlerOptions(opts pxpipe.HandlerOptions) pxpipe.HandlerOptions {
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
	return opts
}

func newServeHandler(logger *requestLog, opts pxpipe.HandlerOptions) http.Handler {
	return newObservedServeHandler(logger, pxpipe.NewHandler(observedHandlerOptions(opts)), "")
}

type requestRowSink interface {
	add(requestLogRow)
}

func newObservedServeHandler(logger requestRowSink, handler http.Handler, transformPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint := r.URL.Path
		if transformPath != "" {
			if originalPath, ok := stripTransformPath(endpoint, transformPath); ok {
				endpoint = originalPath
			}
		}
		observation := &requestObservation{endpoint: endpoint}
		recorder := &statusResponseWriter{ResponseWriter: w}
		if strings.HasSuffix(endpoint, "/messages") {
			// Usage accounting observes the response without changing its bytes, so
			// request an uncompressed Anthropic stream rather than decoding in-proxy.
			r.Header.Set("Accept-Encoding", "identity")
		}
		handler.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), requestObservationKey{}, observation)))
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
	saving, known := o.estimatedSaving()
	if !o.applied {
		saving, known = 0, true
	}
	if known {
		row.saved = int64Pointer(saving)
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
	if known {
		row.asText = int64Pointer(actual + saving)
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

type summaryMetric struct {
	value   int64
	samples int
}

func (m *summaryMetric) add(value *int64) {
	if value == nil {
		return
	}
	m.value += *value
	m.samples++
}

type runSummary struct {
	mu     sync.Mutex
	asText summaryMetric
	sent   summaryMetric
}

func newRunSummary() *runSummary { return &runSummary{} }

func (s *runSummary) add(row requestLogRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row.asText == nil || row.sent == nil {
		return
	}
	s.asText.add(row.asText)
	s.sent.add(row.sent)
}

func (s *runSummary) write(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(w, "\npxpipe summary")
	if s.asText.samples == 0 || s.asText.value <= 0 {
		fmt.Fprintln(w, "  token usage unavailable")
		return
	}
	fmt.Fprintf(w, "  estimated : %s tokens\n", formatInteger(s.asText.value))
	change := 100 * float64(s.sent.value-s.asText.value) / float64(s.asText.value)
	fmt.Fprintf(w, "  actual    : %s tokens (%.1f%%)\n", formatInteger(s.sent.value), change)
}

type requestLog struct {
	mu          sync.Mutex
	out         io.Writer
	terminal    bool
	rows        []requestLogRow
	wroteHeader bool
	program     *tea.Program
	updates     chan requestLogRowsMsg
	updatesMu   sync.Mutex
	sequence    uint64
}

func newRequestLog(out io.Writer, terminal bool) *requestLog {
	return &requestLog{out: out, terminal: terminal}
}

func (l *requestLog) add(row requestLogRow) {
	l.mu.Lock()
	l.rows = append(l.rows, row)
	if len(l.rows) > 20 {
		l.rows = l.rows[len(l.rows)-20:]
	}
	if l.terminal {
		l.sequence++
		message := requestLogRowsMsg{sequence: l.sequence, rows: append([]requestLogRow(nil), l.rows...)}
		updates := l.updates
		l.mu.Unlock()
		offerRequestLogRows(&l.updatesMu, updates, message)
		return
	}
	if !l.wroteHeader {
		fmt.Fprintln(l.out)
		fmt.Fprintln(l.out, requestLogHeader())
		fmt.Fprintln(l.out, requestLogSeparator())
		l.wroteHeader = true
	}
	fmt.Fprintln(l.out, formatRequestLogRow(row))
	l.mu.Unlock()
}

func offerRequestLogRows(mu *sync.Mutex, updates chan requestLogRowsMsg, message requestLogRowsMsg) {
	if updates == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	select {
	case updates <- message:
		return
	default:
	}
	select {
	case queued := <-updates:
		if queued.sequence > message.sequence {
			message = queued
		}
	default:
	}
	select {
	case updates <- message:
	default:
	}
}

func (l *requestLog) startTUI(input io.Reader, port int, certificatePath string) <-chan error {
	done := make(chan error, 1)
	stopped := make(chan struct{})
	options := []tea.ProgramOption{
		tea.WithOutput(l.out),
		tea.WithoutSignalHandler(),
		tea.WithMouseCellMotion(),
		tea.WithAltScreen(),
	}
	if file, ok := input.(*os.File); ok && !term.IsTerminal(file.Fd()) {
		options = append(options, tea.WithInputTTY())
	} else {
		options = append(options, tea.WithInput(input))
	}
	program := tea.NewProgram(newRequestTUIModel(port, certificatePath), options...)
	l.mu.Lock()
	l.program = program
	l.updates = make(chan requestLogRowsMsg, 1)
	l.sequence++
	initial := requestLogRowsMsg{sequence: l.sequence, rows: append([]requestLogRow(nil), l.rows...)}
	updates := l.updates
	l.mu.Unlock()
	go func() {
		_, err := program.Run()
		close(stopped)
		done <- err
	}()
	go func() {
		for {
			select {
			case message := <-updates:
				program.Send(message)
			case <-stopped:
				return
			}
		}
	}()
	offerRequestLogRows(&l.updatesMu, updates, initial)
	return done
}

func (l *requestLog) stopTUI(done <-chan error) error {
	if done == nil {
		return nil
	}
	l.mu.Lock()
	program := l.program
	l.program = nil
	l.updates = nil
	l.mu.Unlock()
	if program != nil {
		go program.Quit()
	}
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		if program != nil {
			program.Kill()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		return errors.New("timed out stopping terminal UI")
	}
}

type requestLogRowsMsg struct {
	sequence uint64
	rows     []requestLogRow
}

type clearClipboardSequenceMsg uint64

const (
	claudeCommandRow = 2
	codexCommandRow  = 3
)

type requestTUIModel struct {
	port              int
	certificatePath   string
	rows              []requestLogRow
	width             int
	height            int
	sequence          uint64
	clipboardSequence string
	clipboardCopy     uint64
	copyNotice        string
}

func newRequestTUIModel(port int, certificatePath string) requestTUIModel {
	return requestTUIModel{port: port, certificatePath: certificatePath}
}

func (requestTUIModel) Init() tea.Cmd { return nil }

func (m requestTUIModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
	case requestLogRowsMsg:
		if message.sequence > m.sequence {
			m.sequence = message.sequence
			m.rows = append([]requestLogRow(nil), message.rows...)
		}
	case tea.MouseMsg:
		event := tea.MouseEvent(message)
		if event.Action == tea.MouseActionPress && event.Button == tea.MouseButtonLeft && event.X >= 0 {
			switch event.Y {
			case claudeCommandRow:
				row := m.commandRow("Claude", 2, m.claudeCommand())
				if event.X >= row.copyStart && event.X < row.copyEnd {
					m.clipboardCopy++
					m.clipboardSequence = terminalClipboardSequence(m.claudeCommand())
					m.copyNotice = "Copied Claude command"
					return m, clearClipboardSequenceAfterDelay(m.clipboardCopy)
				}
			case codexCommandRow:
				row := m.commandRow("Codex", 3, m.codexCommand())
				if event.X >= row.copyStart && event.X < row.copyEnd {
					m.clipboardCopy++
					m.clipboardSequence = terminalClipboardSequence(m.codexCommand())
					m.copyNotice = "Copied Codex command"
					return m, clearClipboardSequenceAfterDelay(m.clipboardCopy)
				}
			}
		}
	case tea.KeyMsg:
		switch message.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+z":
			return m, tea.Suspend
		}
	case clearClipboardSequenceMsg:
		if uint64(message) == m.clipboardCopy {
			m.clipboardSequence = ""
		}
	}
	return m, nil
}

func clearClipboardSequenceAfterDelay(copy uint64) tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return clearClipboardSequenceMsg(copy) })
}

func terminalClipboardSequence(value string) string {
	sequence := osc52.New(value)
	if os.Getenv("STY") != "" {
		sequence = sequence.Screen()
	}
	return sequence.String()
}

func (m requestTUIModel) claudeCommand() string {
	return fmt.Sprintf("ANTHROPIC_BASE_URL=http://localhost:%d claude", m.port)
}

func (m requestTUIModel) codexCommand() string {
	proxyURL := fmt.Sprintf("http://localhost:%d", m.port)
	return fmt.Sprintf(
		"NO_PROXY= no_proxy= HTTPS_PROXY=%s https_proxy=%s HTTP_PROXY=%s http_proxy=%s CODEX_CA_CERTIFICATE=%s codex",
		proxyURL, proxyURL, proxyURL, proxyURL, shellQuote(m.certificatePath),
	)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type commandRowView struct {
	line               string
	copyStart, copyEnd int
}

func (m requestTUIModel) commandRow(label string, gap int, command string) commandRowView {
	const copyLabel = "[copy]"
	copyWidth := runewidth.StringWidth(copyLabel)
	if m.width > 0 && m.width <= copyWidth {
		button := truncateMiddle(copyLabel, m.width)
		return commandRowView{line: tuiCopyStyle.Render(button), copyEnd: runewidth.StringWidth(button)}
	}

	labelWidth := runewidth.StringWidth(label)
	commandWidth := runewidth.StringWidth(command)
	copyStart := labelWidth + gap + commandWidth + 2
	if m.width > 0 {
		copyStart = m.width - copyWidth
		label = truncateMiddle(label, copyStart)
		labelWidth = runewidth.StringWidth(label)
		gap = min(gap, max(0, copyStart-labelWidth))
		commandWidth = max(0, copyStart-labelWidth-gap-2)
	}
	command = truncateMiddle(command, commandWidth)
	prefixWidth := labelWidth + gap + runewidth.StringWidth(command)
	padding := max(0, copyStart-prefixWidth)
	return commandRowView{
		line: tuiLabelStyle.Render(label) + strings.Repeat(" ", gap) + tuiCommandStyle.Render(command) +
			strings.Repeat(" ", padding) + tuiCopyStyle.Render(copyLabel),
		copyStart: copyStart,
		copyEnd:   copyStart + copyWidth,
	}
}

var (
	tuiTitleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	tuiLabelStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	tuiCommandStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	tuiCopyStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	tuiHeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	tuiMutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	tuiWarningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	tuiErrorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

func (m requestTUIModel) View() string {
	address := fmt.Sprintf("127.0.0.1:%d", m.port)
	title := "pxpipe serve"
	addressLabel := "listening on " + address
	if m.width > 0 {
		title = truncateMiddle(title, m.width)
		addressLabel = truncateMiddle(addressLabel, max(0, m.width-runewidth.StringWidth(title)-2))
	}
	titleLine := tuiTitleStyle.Render(title)
	if addressLabel != "" {
		titleLine += "  " + tuiMutedStyle.Render(addressLabel)
	}
	footer := "Click [copy] to copy a command. Ctrl+C to stop"
	if m.copyNotice != "" {
		footer = m.copyNotice
	}
	footer = fitTerminalLine(footer, m.width)
	lines := []string{
		titleLine,
		"",
		m.commandRow("Claude", 2, m.claudeCommand()).line,
		m.commandRow("Codex", 3, m.codexCommand()).line,
		"",
	}
	if m.height > 0 && m.height < 10 {
		lines = lines[:min(4, m.height)]
		if m.height >= 5 {
			lines = append(lines, tuiMutedStyle.Render(footer))
		}
		return m.clipboardSequence + strings.Join(lines, "\n")
	}
	compact := m.width > 0 && m.width < len(requestLogHeader())
	if compact {
		lines = append(lines, tuiHeaderStyle.Render(fitTerminalLine("Recent requests", m.width)), tuiMutedStyle.Render(strings.Repeat("-", max(1, m.width))))
	} else {
		lines = append(lines, tuiHeaderStyle.Render(requestLogHeader()), tuiMutedStyle.Render(requestLogSeparator()))
	}
	rows := m.visibleRows(compact)
	if len(rows) == 0 {
		lines = append(lines, tuiMutedStyle.Render(fitTerminalLine("Waiting for requests...", m.width)))
	} else {
		for _, row := range rows {
			rowLines := []string{formatRequestLogRow(row)}
			if compact {
				rowLines = formatCompactRequestLogRow(row, m.width)
			}
			switch {
			case row.status >= 500:
				for i := range rowLines {
					rowLines[i] = tuiErrorStyle.Render(rowLines[i])
				}
			case row.status >= 400:
				for i := range rowLines {
					rowLines[i] = tuiWarningStyle.Render(rowLines[i])
				}
			}
			lines = append(lines, rowLines...)
		}
	}
	lines = append(lines, "", tuiMutedStyle.Render(footer))
	return m.clipboardSequence + strings.Join(lines, "\n")
}

func (m requestTUIModel) visibleRows(compact bool) []requestLogRow {
	rows := m.rows
	if m.height <= 0 {
		return rows
	}
	rowHeight := 1
	if compact {
		rowHeight = compactRequestLogRowHeight(m.width)
	}
	capacity := max(0, (m.height-9)/rowHeight)
	if len(rows) > capacity {
		rows = rows[len(rows)-capacity:]
	}
	return rows
}

func formatCompactRequestLogRow(row requestLogRow, width int) []string {
	status := strconv.Itoa(row.status)
	sentAs := sanitizeCell(row.sentAs)
	available := max(2, width-len(status)-len(sentAs)-6)
	endpointWidth := available / 2
	modelWidth := available - endpointWidth
	lines := []string{
		fmt.Sprintf("%s  %s  %s  %s", status, truncateMiddle(row.endpoint, endpointWidth), truncateMiddle(valueOrDash(row.model), modelWidth), sentAs),
	}
	if compactRequestLogRowHeight(width) == 3 {
		lines = append(lines,
			fmt.Sprintf("  cache hits %s  as text %s", formatMetric(row.cacheHits), formatMetric(row.asText)),
			fmt.Sprintf("  sent %s  saved/lost %s", formatMetric(row.sent), formatMetric(row.saved)),
		)
		return fitTerminalLines(lines, width)
	}
	lines = append(lines,
		"  cache hits "+formatMetric(row.cacheHits),
		"  as text "+formatMetric(row.asText),
		"  sent "+formatMetric(row.sent),
		"  saved/lost "+formatMetric(row.saved),
	)
	return fitTerminalLines(lines, width)
}

func fitTerminalLines(lines []string, width int) []string {
	for i := range lines {
		lines[i] = fitTerminalLine(lines[i], width)
	}
	return lines
}

func fitTerminalLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	return runewidth.Truncate(line, width, "")
}

func compactRequestLogRowHeight(width int) int {
	if width < 36 {
		return 5
	}
	return 3
}

func requestLogHeader() string {
	return fmt.Sprintf("%-6s  %-20s  %-26s  %-7s  %10s  %10s  %10s  %11s",
		"Result", "Endpoint", "Model", "Sent as", "Cache hits", "As text", "Sent", "Saved/lost")
}

func requestLogSeparator() string {
	return strings.Repeat("-", len(requestLogHeader()))
}

func formatRequestLogRow(row requestLogRow) string {
	return strings.Join([]string{
		padCell(strconv.Itoa(row.status), 6),
		padCell(row.endpoint, 20),
		padCell(valueOrDash(row.model), 26),
		padCell(row.sentAs, 7),
		alignRightCell(formatMetric(row.cacheHits), 10),
		alignRightCell(formatMetric(row.asText), 10),
		alignRightCell(formatMetric(row.sent), 10),
		alignRightCell(formatMetric(row.saved), 11),
	}, "  ")
}

func padCell(value string, width int) string {
	value = truncateMiddle(value, width)
	return value + strings.Repeat(" ", max(0, width-runewidth.StringWidth(value)))
}

func alignRightCell(value string, width int) string {
	value = truncateMiddle(value, width)
	return strings.Repeat(" ", max(0, width-runewidth.StringWidth(value))) + value
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
	value = sanitizeCell(value)
	cellWidth := runewidth.StringWidth(value)
	if cellWidth <= width {
		return value
	}
	if width <= 3 {
		return runewidth.Truncate(value, max(0, width), "")
	}
	left := (width - 3) / 2
	right := width - 3 - left
	return runewidth.Truncate(value, left, "") + "..." + runewidth.TruncateLeft(value, cellWidth-right, "")
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
	return term.IsTerminal(file.Fd())
}
