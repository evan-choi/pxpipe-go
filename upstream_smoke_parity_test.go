package pxpipe

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

type upstreamSmokeRecord struct {
	bytes  int
	images int
}

func TestUpstreamSmokeCollapseParity(t *testing.T) {
	SetAllowedModelBases([]string{"claude-opus-5"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })

	var recordsMu sync.Mutex
	var records []upstreamSmokeRecord
	var failNext atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		request, err := parseOrderedJSON(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		messages, _ := asArr(request["messages"])
		record := upstreamSmokeRecord{bytes: len(body), images: countNativeImages(messages)}
		recordsMu.Lock()
		records = append(records, record)
		recordsMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if failNext.Swap(false) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"too large"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(NewHandler(HandlerOptions{AnthropicUpstream: upstreamURL}))
	defer proxy.Close()

	send := func(messages []any) (int, upstreamSmokeRecord) {
		t.Helper()
		body := parityMarshal(t, map[string]any{
			"model": "claude-opus-5", "max_tokens": 16, "messages": messages,
		})
		request, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", "test")
		request.Header.Set("Anthropic-Version", "2023-06-01")
		response, err := proxy.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		recordsMu.Lock()
		defer recordsMu.Unlock()
		if len(records) == 0 {
			t.Fatal("upstream did not receive request")
		}
		return response.StatusCode, records[len(records)-1]
	}

	status400, turn400 := send(parityConversation(400, 3500, "SESSION ANCHOR: "))
	if status400 != http.StatusOK || turn400.images == 0 {
		t.Fatalf("400-turn request: status=%d images=%d", status400, turn400.images)
	}
	if turn400.images > AnthropicMaxImages || turn400.bytes >= 12_000_000 {
		t.Fatalf("400-turn forwarded body: images=%d bytes=%d", turn400.images, turn400.bytes)
	}

	status410, turn410 := send(parityConversation(410, 3500, "SESSION ANCHOR: "))
	if status410 != http.StatusOK || turn410.images > AnthropicMaxImages {
		t.Fatalf("410-turn growth request: status=%d images=%d", status410, turn410.images)
	}

	failNext.Store(true)
	status420, turn420 := send(parityConversation(420, 3500, "SESSION ANCHOR: "))
	if status420 != http.StatusInternalServerError || turn420.images > AnthropicMaxImages {
		t.Fatalf("420-turn forced failure: status=%d images=%d", status420, turn420.images)
	}
	status430, turn430 := send(parityConversation(430, 3500, "SESSION ANCHOR: "))
	if status430 != http.StatusOK || turn430.images > AnthropicMaxImages {
		t.Fatalf("post-500 growth request: status=%d images=%d", status430, turn430.images)
	}

	saturated := append([]any{parityClientImages(AnthropicMaxImages)}, parityConversation(200, 3500, "SESSION ANCHOR: ")[1:]...)
	statusFull, full := send(saturated)
	if statusFull != http.StatusOK || full.images != AnthropicMaxImages {
		t.Fatalf("client-saturated request: status=%d images=%d, want %d", statusFull, full.images, AnthropicMaxImages)
	}

	for name, record := range map[string]upstreamSmokeRecord{
		"400": turn400, "410": turn410, "420": turn420, "430": turn430,
	} {
		if record.images > AnthropicMaxImages {
			t.Errorf("%s-turn request forwarded %d images", name, record.images)
		}
	}
	t.Logf("forwarded: 400=%dimg/%.1fMB 410=%dimg/%.1fMB 420=%dimg/%.1fMB 430=%dimg/%.1fMB full=%dimg/%.1fMB",
		turn400.images, float64(turn400.bytes)/1e6,
		turn410.images, float64(turn410.bytes)/1e6,
		turn420.images, float64(turn420.bytes)/1e6,
		turn430.images, float64(turn430.bytes)/1e6,
		full.images, float64(full.bytes)/1e6,
	)
}
