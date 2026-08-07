package pxpipe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type fragmentReadCloser struct {
	fragments [][]byte
	index     int
	offset    int
	closed    bool
	err       error
}

func (r *fragmentReadCloser) Read(p []byte) (int, error) {
	if r.index >= len(r.fragments) {
		if r.err != nil {
			err := r.err
			r.err = nil
			return 0, err
		}
		return 0, io.EOF
	}
	fragment := r.fragments[r.index]
	n := copy(p, fragment[r.offset:])
	r.offset += n
	if r.offset == len(fragment) {
		r.index++
		r.offset = 0
	}
	return n, nil
}

func (r *fragmentReadCloser) Close() error {
	r.closed = true
	return nil
}

func fragmented(data []byte, widths ...int) [][]byte {
	var out [][]byte
	for i, widthIndex := 0, 0; i < len(data); widthIndex++ {
		width := widths[widthIndex%len(widths)]
		end := i + width
		if end > len(data) {
			end = len(data)
		}
		out = append(out, append([]byte(nil), data[i:end]...))
		i = end
	}
	return out
}

func TestAnthropicResponseFragmentedSSEPreservesBytesAndUpdatesAtEOF(t *testing.T) {
	body := []byte("event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"cache_creation_input_tokens\":34,\"cache_read_input_tokens\":0}}}\r\n\r\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":56}}\n\n")
	s := newSessionStateStore()
	now := time.Unix(4_000, 0)
	s.noteHistoryRequest("s", now)
	source := &fragmentReadCloser{fragments: fragmented(body, 1, 2, 7, 3)}
	wrapped := wrapAnthropicResponseBody(source, 200, "text/event-stream; charset=utf-8", "s", s)

	buf := make([]byte, 5)
	var got bytes.Buffer
	for {
		n, err := wrapped.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		// A create is not visible until the complete stream reaches EOF.
		if state := s.noteHistoryRequest("s", now); state.cold {
			t.Fatal("state changed before EOF")
		}
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("stream bytes changed\ngot:  %q\nwant: %q", got.Bytes(), body)
	}
	if state := s.noteHistoryRequest("s", now.Add(4*time.Hour)); state.cold {
		t.Fatal("fragmented SSE cache creation was not recorded as alive")
	}
	if err := wrapped.Close(); err != nil || !source.closed {
		t.Fatalf("Close() = %v, source closed = %v", err, source.closed)
	}
}

func TestAnthropicResponseJSONOutcomeMatrix(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCold   bool
		primeAlive bool
	}{
		{name: "2xx usage alive", status: 200, body: `{"usage":{"input_tokens":1,"cache_read_input_tokens":9}}`, primeAlive: true},
		{name: "2xx usage zero after alive", status: 200, body: `{"usage":{"input_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`, primeAlive: true, wantCold: true},
		{name: "2xx no usage", status: 200, body: `{"content":[]}`, primeAlive: true},
		{name: "413", status: 413, body: `{"error":"large"}`, wantCold: true},
		{name: "size 400", status: 400, body: `{"error":{"message":"prompt_too_long"}}`, wantCold: true},
		{name: "other 400", status: 400, body: `{"error":{"message":"bad key"}}`, primeAlive: true},
		{name: "429", status: 429, body: `rate limited`, primeAlive: true},
		{name: "500", status: 500, body: `failed`, primeAlive: true},
		{name: "529", status: 529, body: `overloaded`, primeAlive: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSessionStateStore()
			now := time.Unix(5_000, 0)
			s.noteHistoryRequest("s", now)
			if tt.primeAlive {
				s.noteCacheOutcome("s", 1, 0)
			}
			wrapped := wrapAnthropicResponseBody(io.NopCloser(bytes.NewBufferString(tt.body)), tt.status, "application/json", "s", s)
			if _, err := io.ReadAll(wrapped); err != nil {
				t.Fatal(err)
			}
			got := s.noteHistoryRequest("s", now.Add(time.Minute)).cold
			if got != tt.wantCold {
				t.Fatalf("cold = %v, want %v", got, tt.wantCold)
			}
		})
	}
}

func TestAnthropicResponseNoMutationBeforeEOFOrAfterReadError(t *testing.T) {
	for _, tt := range []struct {
		name  string
		close bool
	}{
		{name: "close early", close: true},
		{name: "read error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSessionStateStore()
			now := time.Unix(6_000, 0)
			s.noteHistoryRequest("s", now)
			s.noteCacheOutcome("s", 1, 0)
			source := &fragmentReadCloser{
				fragments: [][]byte{[]byte(`{"usage":{"cache_read_input_tokens":0}}`)},
				err:       errors.New("stream failed"),
			}
			wrapped := wrapAnthropicResponseBody(source, 200, "application/json", "s", s)
			buf := make([]byte, 128)
			if _, err := wrapped.Read(buf); err != nil {
				t.Fatal(err)
			}
			if tt.close {
				if err := wrapped.Close(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := wrapped.Read(buf); err == nil {
				t.Fatal("expected source error")
			}
			if got := s.noteHistoryRequest("s", now.Add(time.Minute)); got.cold {
				t.Fatal("partial response mutated cache state")
			}
		})
	}
}

func TestAnthropicResponseScannerBoundsOversizedInputs(t *testing.T) {
	scanner := newAnthropicResponseScanner(200, "application/json")
	scanner.write(bytes.Repeat([]byte{'x'}, maxAnthropicJSONResponse+1))
	if result := scanner.finish(); result.usage != nil || cap(scanner.jsonBody) > maxAnthropicJSONResponse {
		t.Fatal("oversized JSON was retained or parsed")
	}

	scanner = newAnthropicResponseScanner(200, "text/event-stream")
	scanner.write([]byte("data: "))
	scanner.write(bytes.Repeat([]byte{'x'}, maxAnthropicSSEEvent+1))
	scanner.write([]byte("\n\ndata: {\"usage\":{\"cache_read_input_tokens\":3}}\n\n"))
	result := scanner.finish()
	if result.usage == nil || result.usage.CacheReadInputTokens != 3 {
		t.Fatal("oversized SSE event prevented a later bounded event from parsing")
	}
}

func TestAnthropicResponseScannerAcceptsEscapedUsageKeyAndPartialFields(t *testing.T) {
	scanner := newAnthropicResponseScanner(200, "text/event-stream")
	scanner.write([]byte("data: {\"\\u0075sage\":{\"input_tokens\":\"bad\",\"cache_read_input_tokens\":7}}\n\n"))
	result := scanner.finish()
	if result.usage == nil || result.usage.CacheReadInputTokens != 7 {
		t.Fatalf("escaped usage accounting = %+v", result.usage)
	}
}

func TestAnthropicAccountingCompletesBeforeReliabilityRelease(t *testing.T) {
	s := newSessionStateStore()
	s.noteHistoryRequest("s", time.Unix(7_000, 0))
	s.noteCacheOutcome("s", 1, 0)

	_, cancel := context.WithCancelCause(context.Background())
	released := make(chan bool, 1)
	body := newReliabilityBody(
		io.NopCloser(strings.NewReader(`{"usage":{"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`)),
		cancel,
		func() {
			s.mu.Lock()
			rec := s.entries["s"].Value.(*sessionStateEntry).record
			s.mu.Unlock()
			released <- rec.cacheObserved && !rec.lastCacheAlive
		},
		0,
		0,
	)
	observer := newAnthropicAccountingObserver(200, "application/json", func(result anthropicResponseAccounting) {
		s.accountAnthropicResponse("s", 200, result.usage, result.errorBody)
	})
	if !attachReliabilityBodyObserver(body, observer) {
		t.Fatal("observer was not attached")
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if updated := <-released; !updated {
		t.Fatal("lease was released before cache accounting committed")
	}
}

var benchmarkAnthropicAccounting anthropicResponseAccounting

func BenchmarkAnthropicResponseScannerJSON(b *testing.B) {
	body := []byte(`{"id":"msg_1","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1200,"output_tokens":80,"cache_creation_input_tokens":400,"cache_read_input_tokens":800}}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for range b.N {
		scanner := newAnthropicResponseScanner(200, "application/json")
		scanner.write(body)
		benchmarkAnthropicAccounting = scanner.finish()
	}
}

func BenchmarkAnthropicResponseScannerSSE(b *testing.B) {
	var stream strings.Builder
	stream.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1200,\"cache_creation_input_tokens\":400,\"cache_read_input_tokens\":800}}}\n\n")
	for i := 0; i < 100; i++ {
		stream.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"abcdefghijklmnopqrstuvwx\"}}\n\n")
	}
	stream.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":80}}\n\n")
	body := []byte(stream.String())
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for range b.N {
		scanner := newAnthropicResponseScanner(200, "text/event-stream")
		for offset := 0; offset < len(body); {
			end := offset + 4096
			if end > len(body) {
				end = len(body)
			}
			scanner.write(body[offset:end])
			offset = end
		}
		benchmarkAnthropicAccounting = scanner.finish()
	}
}
