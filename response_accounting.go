package pxpipe

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

const (
	maxAnthropicJSONResponse = 4 << 20
	maxAnthropicSSEEvent     = 256 << 10
	maxAnthropicErrorBody    = 8 << 10
)

var anthropicSizeErrorPattern = regexp.MustCompile(`(?i)prompt[\s_-]*(is\s*)?too[\s_-]*long|request[\s_-]*too[\s_-]*large|too many (images|tokens)`)

type anthropicUsage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

type parsedAnthropicUsage struct {
	usage anthropicUsage
	seen  bool
}

// accountAnthropicResponse applies the response-outcome matrix. In particular,
// missing usage on a successful response is not equivalent to zero usage.
func (s *sessionStateStore) accountAnthropicResponse(
	sessionKey string,
	status int,
	usage *anthropicUsage,
	errorBody []byte,
) {
	if s == nil || sessionKey == "" {
		return
	}
	if status >= 200 && status < 300 {
		if usage != nil {
			s.noteCacheOutcome(sessionKey, usage.CacheReadInputTokens, usage.CacheCreationInputTokens)
		}
		return
	}
	if status == 413 || (status == 400 && anthropicSizeErrorPattern.Match(errorBody)) {
		s.markCacheDead(sessionKey)
	}
}

// wrapAnthropicResponseBody observes a body without teeing or pre-reading it.
// Reads are returned byte-for-byte as the source produces them, and state is
// updated only after a clean EOF. Closing or failing mid-stream records nothing.
func wrapAnthropicResponseBody(
	body io.ReadCloser,
	status int,
	contentType string,
	sessionKey string,
	sessions *sessionStateStore,
) io.ReadCloser {
	if body == nil {
		return nil
	}
	return &anthropicAccountingReadCloser{
		body: body,
		observer: newAnthropicAccountingObserver(status, contentType, func(result anthropicResponseAccounting) {
			sessions.accountAnthropicResponse(sessionKey, status, result.usage, result.errorBody)
		}),
	}
}

type anthropicAccountingReadCloser struct {
	body     io.ReadCloser
	observer *anthropicAccountingObserver
	failed   bool
}

func (r *anthropicAccountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 && !r.failed {
		r.observer.observeResponse(p[:n])
	}
	if err == io.EOF {
		if !r.failed {
			r.observer.completeResponse()
		}
	} else if err != nil {
		r.failed = true
	}
	return n, err
}

func (r *anthropicAccountingReadCloser) Close() error {
	return r.body.Close()
}

type anthropicAccountingObserver struct {
	scanner  *anthropicResponseScanner
	onEOF    func(anthropicResponseAccounting)
	finished bool
}

func newAnthropicAccountingObserver(
	status int,
	contentType string,
	onEOF func(anthropicResponseAccounting),
) *anthropicAccountingObserver {
	return &anthropicAccountingObserver{
		scanner: newAnthropicResponseScanner(status, contentType),
		onEOF:   onEOF,
	}
}

func (o *anthropicAccountingObserver) observeResponse(p []byte) {
	if !o.finished {
		o.scanner.write(p)
	}
}

func (o *anthropicAccountingObserver) completeResponse() {
	if o.finished {
		return
	}
	o.finished = true
	o.onEOF(o.scanner.finish())
}

type anthropicResponseAccounting struct {
	usage     *anthropicUsage
	errorBody []byte
}

type anthropicResponseScanner struct {
	status int
	sse    bool

	jsonBody     []byte
	jsonOverflow bool
	errorBody    []byte

	line          []byte
	eventData     []byte
	eventOverflow bool
	eventHasData  bool
	afterCR       bool
	streamUsage   parsedAnthropicUsage
}

func newAnthropicResponseScanner(status int, contentType string) *anthropicResponseScanner {
	return &anthropicResponseScanner{
		status: status,
		sse:    strings.Contains(strings.ToLower(contentType), "text/event-stream"),
	}
}

func (s *anthropicResponseScanner) write(p []byte) {
	if s.status == 400 {
		remaining := maxAnthropicErrorBody - len(s.errorBody)
		if remaining > len(p) {
			remaining = len(p)
		}
		if remaining > 0 {
			s.errorBody = append(s.errorBody, p[:remaining]...)
		}
		return
	}
	if s.status < 200 || s.status >= 300 {
		return
	}
	if !s.sse {
		if s.jsonOverflow {
			return
		}
		if len(s.jsonBody)+len(p) > maxAnthropicJSONResponse {
			s.jsonBody = nil
			s.jsonOverflow = true
			return
		}
		s.jsonBody = append(s.jsonBody, p...)
		return
	}
	for _, c := range p {
		if s.afterCR {
			s.afterCR = false
			if c == '\n' {
				continue
			}
		}
		switch c {
		case '\r':
			s.endSSELine()
			s.afterCR = true
		case '\n':
			s.endSSELine()
		default:
			if len(s.line) < maxAnthropicSSEEvent {
				s.line = append(s.line, c)
			} else {
				s.eventOverflow = true
			}
		}
	}
}

func (s *anthropicResponseScanner) endSSELine() {
	if len(s.line) == 0 {
		s.endSSEEvent()
		return
	}
	if !s.eventOverflow && bytes.HasPrefix(s.line, []byte("data:")) {
		data := s.line[len("data:"):]
		if len(data) > 0 && data[0] == ' ' {
			data = data[1:]
		}
		needed := len(data)
		if s.eventHasData {
			needed++
		}
		if len(s.eventData)+needed > maxAnthropicSSEEvent {
			s.eventData = nil
			s.eventOverflow = true
		} else {
			if s.eventHasData {
				s.eventData = append(s.eventData, '\n')
			}
			s.eventData = append(s.eventData, data...)
			s.eventHasData = true
		}
	}
	s.line = s.line[:0]
}

func (s *anthropicResponseScanner) endSSEEvent() {
	if s.eventHasData && !s.eventOverflow && mayContainAnthropicUsage(s.eventData) {
		mergeAnthropicEnvelope(s.eventData, &s.streamUsage)
	}
	s.line = s.line[:0]
	s.eventData = s.eventData[:0]
	s.eventOverflow = false
	s.eventHasData = false
}

func (s *anthropicResponseScanner) finish() anthropicResponseAccounting {
	if s.status == 400 {
		return anthropicResponseAccounting{errorBody: s.errorBody}
	}
	if s.status < 200 || s.status >= 300 {
		return anthropicResponseAccounting{}
	}
	if s.sse {
		if len(s.line) > 0 {
			s.endSSELine()
		}
		if s.eventHasData || s.eventOverflow {
			s.endSSEEvent()
		}
		if s.streamUsage.seen {
			usage := s.streamUsage.usage
			return anthropicResponseAccounting{usage: &usage}
		}
		return anthropicResponseAccounting{}
	}
	if s.jsonOverflow {
		return anthropicResponseAccounting{}
	}
	var usage parsedAnthropicUsage
	mergeAnthropicEnvelope(s.jsonBody, &usage)
	if !usage.seen {
		return anthropicResponseAccounting{}
	}
	return anthropicResponseAccounting{usage: &usage.usage}
}

func mayContainAnthropicUsage(data []byte) bool {
	// Anthropic emits canonical keys. Fall back to decoding when a key could
	// contain a JSON Unicode escape so valid escaped spellings remain accepted.
	return bytes.Contains(data, []byte(`"usage"`)) || bytes.Contains(data, []byte(`\u`))
}

type optionalAnthropicInt64 struct {
	value int64
	set   bool
}

func (n *optionalAnthropicInt64) UnmarshalJSON(data []byte) error {
	var value int64
	if json.Unmarshal(data, &value) == nil {
		n.value = value
		n.set = true
	}
	// An invalid individual usage field does not hide other valid fields.
	return nil
}

type anthropicUsageFields struct {
	InputTokens              optionalAnthropicInt64 `json:"input_tokens"`
	OutputTokens             optionalAnthropicInt64 `json:"output_tokens"`
	CacheCreationInputTokens optionalAnthropicInt64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     optionalAnthropicInt64 `json:"cache_read_input_tokens"`
}

func (fields *anthropicUsageFields) merge(dst *parsedAnthropicUsage) {
	if fields.InputTokens.set {
		dst.usage.InputTokens = fields.InputTokens.value
		dst.seen = true
	}
	if fields.OutputTokens.set {
		dst.usage.OutputTokens = fields.OutputTokens.value
		dst.seen = true
	}
	if fields.CacheCreationInputTokens.set {
		dst.usage.CacheCreationInputTokens = fields.CacheCreationInputTokens.value
		dst.seen = true
	}
	if fields.CacheReadInputTokens.set {
		dst.usage.CacheReadInputTokens = fields.CacheReadInputTokens.value
		dst.seen = true
	}
}

func mergeAnthropicEnvelope(data []byte, dst *parsedAnthropicUsage) {
	var envelope struct {
		Usage   anthropicUsageFields `json:"usage"`
		Message struct {
			Usage anthropicUsageFields `json:"usage"`
		} `json:"message"`
	}
	if len(data) == 0 || json.Unmarshal(data, &envelope) != nil {
		return
	}
	envelope.Message.Usage.merge(dst)
	envelope.Usage.merge(dst)
}
