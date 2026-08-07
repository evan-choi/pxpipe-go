package pxpipe

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultUpstreamHeadersTimeout = 5 * time.Minute
	defaultUpstreamIdleTimeout    = 2 * time.Minute
	defaultDuplicateHold          = time.Minute
	inFlightSweepSize             = 256
	inFlightSweepMinInterval      = time.Second
)

type reliabilityConfig struct {
	headersTimeout time.Duration
	idleTimeout    time.Duration
	duplicateHold  time.Duration
}

func resolveReliabilityConfig(opts HandlerOptions) reliabilityConfig {
	return reliabilityConfig{
		headersTimeout: durationOrDefault(opts.UpstreamHeadersTimeout, defaultUpstreamHeadersTimeout),
		idleTimeout:    durationOrDefault(opts.UpstreamIdleTimeout, defaultUpstreamIdleTimeout),
		duplicateHold:  durationOrDefault(opts.DuplicateHold, defaultDuplicateHold),
	}
}

func durationOrDefault(value *time.Duration, fallback time.Duration) time.Duration {
	if value == nil {
		return fallback
	}
	return *value
}

type bodyDigestContextKey struct{}

func withBodyDigest(r *http.Request, body []byte) *http.Request {
	if len(body) == 0 {
		return r
	}
	digest := sha256.Sum256(body)
	return r.WithContext(context.WithValue(r.Context(), bodyDigestContextKey{}, digest))
}

func applySerializedRequestLimit(res *TransformResult, model string) (string, bool) {
	if res.Info != nil {
		res.Info.SerializedRequestBytes = len(res.Body)
	}
	if !res.Applied || model == "" {
		return "", false
	}
	limit := ResolveGptProfile(model).MaxSerializedRequestBytes
	if limit <= 0 {
		return "", false
	}
	if len(res.Body) <= limit {
		if res.Info != nil {
			res.Info.SizeLimitOutcome = "within_limit"
		}
		return "", false
	}
	if res.Info != nil {
		res.Info.SizeLimitOutcome = "rejected"
	}
	message := fmt.Sprintf(
		"pxpipe serialized request exceeds model limit (%d > %d bytes)",
		len(res.Body), limit,
	)
	return message, true
}

var errDuplicateRequestInFlight = errors.New("duplicate_request_in_flight")

type upstreamHeadersTimeoutError struct {
	after time.Duration
}

func (e *upstreamHeadersTimeoutError) Error() string {
	return fmt.Sprintf("pxpipe: upstream headers timeout after %dms", e.after.Milliseconds())
}

type upstreamIdleTimeoutError struct {
	after time.Duration
}

func (e *upstreamIdleTimeoutError) Error() string {
	return fmt.Sprintf("pxpipe: upstream stalled for %dms", e.after.Milliseconds())
}

func proxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	protocol, _ := r.Context().Value(protocolContextKey{}).(Protocol)
	if errors.Is(err, errDuplicateRequestInFlight) {
		writeProtocolError(
			w,
			protocol,
			http.StatusConflict,
			"duplicate_request_in_flight",
			"An identical request is already in progress",
		)
		return
	}
	var timeout *upstreamHeadersTimeoutError
	if errors.As(err, &timeout) {
		detail := fmt.Sprintf("no response headers within %dms", timeout.after.Milliseconds())
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"error": "pxpipe upstream timeout (" + detail + ")",
		})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"error": "pxpipe upstream unreachable",
	})
}

func writeProtocolError(w http.ResponseWriter, protocol Protocol, status int, errorType, message string) {
	apiError := map[string]any{"type": errorType, "message": message}
	if protocol == ProtocolAnthropicMessages {
		writeJSON(w, status, map[string]any{
			"type":  "error",
			"error": apiError,
		})
		return
	}
	writeJSON(w, status, map[string]any{"error": apiError})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, _ := jsonMarshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type inFlightEntry struct {
	lease     uint64
	startedAt time.Time
}

type reliabilityTransport struct {
	base   http.RoundTripper
	config reliabilityConfig

	mu          sync.Mutex
	inFlight    map[[32]byte]inFlightEntry
	nextLease   uint64
	lastSweptAt time.Time
}

func newReliabilityTransport(base http.RoundTripper, config reliabilityConfig) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &reliabilityTransport{
		base:     base,
		config:   config,
		inFlight: make(map[[32]byte]inFlightEntry),
	}
}

func (t *reliabilityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	release, err := t.acquire(req)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}

	ctx, cancel := context.WithCancelCause(req.Context())
	resp, err := t.roundTripHeaders(req.Clone(ctx), cancel)
	if err != nil {
		cancel(err)
		release()
		return nil, err
	}
	if resp.Request == nil {
		resp.Request = req
	}
	if resp.Body == nil {
		cancel(nil)
		release()
		return resp, nil
	}
	watched := newReliabilityBody(
		resp.Body,
		cancel,
		release,
		t.config.headersTimeout,
		t.config.idleTimeout,
	)
	if readWriter, ok := resp.Body.(io.ReadWriteCloser); ok {
		resp.Body = &reliabilityReadWriteBody{reliabilityBody: watched, writer: readWriter}
	} else {
		resp.Body = watched
	}
	return resp, nil
}

type roundTripResult struct {
	response *http.Response
	err      error
}

func (t *reliabilityTransport) roundTripHeaders(
	req *http.Request,
	cancel context.CancelCauseFunc,
) (*http.Response, error) {
	if t.config.headersTimeout <= 0 {
		return t.base.RoundTrip(req)
	}

	result := make(chan roundTripResult)
	abandoned := make(chan struct{})
	go func() {
		resp, err := t.base.RoundTrip(req)
		select {
		case result <- roundTripResult{response: resp, err: err}:
		case <-abandoned:
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
	}()

	timer := time.NewTimer(t.config.headersTimeout)
	defer timer.Stop()
	select {
	case outcome := <-result:
		return outcome.response, outcome.err
	case <-timer.C:
		timeout := &upstreamHeadersTimeoutError{after: t.config.headersTimeout}
		cancel(timeout)
		t.cancelRequest(req)
		close(abandoned)
		return nil, timeout
	case <-req.Context().Done():
		t.cancelRequest(req)
		close(abandoned)
		cause := context.Cause(req.Context())
		if cause == nil {
			cause = req.Context().Err()
		}
		return nil, cause
	}
}

func (t *reliabilityTransport) cancelRequest(req *http.Request) {
	if canceler, ok := t.base.(interface{ CancelRequest(*http.Request) }); ok {
		canceler.CancelRequest(req)
	}
}

func (t *reliabilityTransport) acquire(req *http.Request) (func(), error) {
	bodyDigest, ok := req.Context().Value(bodyDigestContextKey{}).([32]byte)
	if !ok || t.config.duplicateHold <= 0 {
		return func() {}, nil
	}

	key := duplicateKey(req, bodyDigest)
	now := time.Now()
	t.mu.Lock()
	sinceSweep := now.Sub(t.lastSweptAt)
	if t.lastSweptAt.IsZero() || sinceSweep >= t.config.duplicateHold ||
		(len(t.inFlight) > inFlightSweepSize && sinceSweep >= inFlightSweepMinInterval) {
		for entryKey, entry := range t.inFlight {
			if now.Sub(entry.startedAt) >= t.config.duplicateHold {
				delete(t.inFlight, entryKey)
			}
		}
		t.lastSweptAt = now
	}
	if existing, found := t.inFlight[key]; found && now.Sub(existing.startedAt) < t.config.duplicateHold {
		t.mu.Unlock()
		return nil, errDuplicateRequestInFlight
	}
	t.nextLease++
	lease := t.nextLease
	t.inFlight[key] = inFlightEntry{lease: lease, startedAt: now}
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if current, found := t.inFlight[key]; found && current.lease == lease {
				delete(t.inFlight, key)
			}
			t.mu.Unlock()
		})
	}, nil
}

func duplicateKey(req *http.Request, bodyDigest [32]byte) [32]byte {
	h := sha256.New()
	writeHashPart(h, []byte(req.Method))
	writeHashPart(h, []byte(req.URL.String()))

	type headerEntry struct {
		name   string
		values []string
	}
	headers := make([]headerEntry, 0, len(req.Header))
	for name, values := range req.Header {
		headers = append(headers, headerEntry{
			name:   strings.ToLower(name),
			values: append([]string(nil), values...),
		})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })
	for _, header := range headers {
		writeHashPart(h, []byte(header.name))
		for _, value := range header.values {
			writeHashPart(h, []byte(value))
		}
	}
	writeHashPart(h, bodyDigest[:])

	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func writeHashPart(w io.Writer, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = w.Write(size[:])
	_, _ = w.Write(value)
}

type reliabilityBody struct {
	body     io.ReadCloser
	cancel   context.CancelCauseFunc
	release  func()
	idle     time.Duration
	observer reliabilityBodyObserver

	mu         sync.Mutex
	timer      *time.Timer
	deadline   time.Time
	budget     time.Duration
	done       bool
	timeoutErr error
}

type reliabilityReadWriteBody struct {
	*reliabilityBody
	writer io.Writer
}

type reliabilityBodyObserver interface {
	observeResponse([]byte)
	completeResponse()
}

type reliabilityBodyObserverTarget interface {
	attachResponseObserver(reliabilityBodyObserver) bool
}

func attachReliabilityBodyObserver(body io.ReadCloser, observer reliabilityBodyObserver) bool {
	target, ok := body.(reliabilityBodyObserverTarget)
	return ok && target.attachResponseObserver(observer)
}

func (b *reliabilityBody) attachResponseObserver(observer reliabilityBodyObserver) bool {
	if observer == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done || b.observer != nil {
		return false
	}
	b.observer = observer
	return true
}

func (b *reliabilityReadWriteBody) Write(p []byte) (int, error) {
	return b.writer.Write(p)
}

func newReliabilityBody(
	body io.ReadCloser,
	cancel context.CancelCauseFunc,
	release func(),
	firstChunkTimeout time.Duration,
	idleTimeout time.Duration,
) *reliabilityBody {
	watched := &reliabilityBody{
		body:    body,
		cancel:  cancel,
		release: release,
		idle:    idleTimeout,
	}
	if firstChunkTimeout <= 0 {
		firstChunkTimeout = idleTimeout
	}
	if idleTimeout > 0 {
		watched.arm(firstChunkTimeout)
	}
	return watched
}

func (b *reliabilityBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.mu.Lock()
	if b.timeoutErr != nil {
		timeoutErr := b.timeoutErr
		b.mu.Unlock()
		return n, timeoutErr
	}
	if b.done {
		b.mu.Unlock()
		return n, err
	}
	if n > 0 && (err == nil || err == io.EOF) && b.observer != nil {
		b.observer.observeResponse(p[:n])
	}
	if err == nil {
		if n > 0 && b.idle > 0 {
			b.armLocked(b.idle)
		}
		b.mu.Unlock()
		return n, nil
	}
	b.done = true
	if b.timer != nil {
		b.timer.Stop()
	}
	observer := b.observer
	b.mu.Unlock()

	if err == io.EOF && observer != nil {
		observer.completeResponse()
	}
	b.cancel(err)
	b.release()
	return n, err
}

func (b *reliabilityBody) Close() error {
	b.finish(nil)
	return b.body.Close()
}

func (b *reliabilityBody) arm(budget time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armLocked(budget)
}

func (b *reliabilityBody) armLocked(budget time.Duration) {
	if b.done {
		return
	}
	b.deadline = time.Now().Add(budget)
	b.budget = budget
	if b.timer == nil {
		b.timer = time.AfterFunc(budget, b.onTimer)
		return
	}
	b.timer.Reset(budget)
}

func (b *reliabilityBody) onTimer() {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	if remaining := time.Until(b.deadline); remaining > 0 {
		b.timer.Reset(remaining)
		b.mu.Unlock()
		return
	}
	timeout := &upstreamIdleTimeoutError{after: b.budget}
	b.timeoutErr = timeout
	b.done = true
	b.mu.Unlock()

	b.cancel(timeout)
	b.release()
	_ = b.body.Close()
}

func (b *reliabilityBody) finish(cause error) {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.done = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()

	b.cancel(cause)
	b.release()
}
