package pxpipe

import (
	"encoding/json"
	"strings"
	"testing"
)

type pinSSEFrame struct {
	event string
	data  map[string]any
	done  bool
}

func parsePinSSE(t *testing.T, body []byte) []pinSSEFrame {
	t.Helper()
	var frames []pinSSEFrame
	for _, raw := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		var frame pinSSEFrame
		var data string
		for _, line := range strings.Split(raw, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				frame.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "[DONE]" {
			frame.done = true
		} else if err := json.Unmarshal([]byte(data), &frame.data); err != nil {
			t.Fatalf("decode SSE data %q: %v", data, err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func decodePinReply(t *testing.T, reply *pinCommandReply) map[string]any {
	t.Helper()
	if reply == nil {
		t.Fatal("expected local pin reply")
	}
	var body map[string]any
	if err := json.Unmarshal(reply.body, &body); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return body
}

func pinReplyContent(t *testing.T, body map[string]any, path ...any) any {
	t.Helper()
	var value any = body
	for _, part := range path {
		switch part := part.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("%q parent is %T, want object", part, value)
			}
			value = object[part]
		case int:
			list, ok := value.([]any)
			if !ok || part < 0 || part >= len(list) {
				t.Fatalf("index %d parent is %T", part, value)
			}
			value = list[part]
		default:
			t.Fatalf("unsupported path part %T", part)
		}
	}
	return value
}

func TestIsPinOnlyRequest(t *testing.T) {
	text := func(role, content string) map[string]any {
		return map[string]any{"role": role, "content": content}
	}
	cases := []struct {
		name     string
		messages []any
		want     bool
	}{
		{"pin", []any{text("user", "@pxpipe pin be concise")}, true},
		{"unpin", []any{text("user", "@pxpipe unpin all")}, true},
		{"trailing system metadata", []any{text("user", "@pxpipe pin be concise"), text("system", "agent catalogue")}, true},
		{"reminder plus pin", []any{text("user", "<system-reminder>notice</system-reminder>\n@pxpipe pin be concise")}, true},
		{"mixed prose", []any{text("user", "@pxpipe pin be concise\ndo the work")}, false},
		{"tool result", []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "ok"}}}}, false},
		{"assistant prefill", []any{text("user", "@pxpipe pin be concise"), text("assistant", "prefill")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPinOnlyRequest(tc.messages); got != tc.want {
				t.Fatalf("isPinOnlyRequest() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPinCommandResponseMessages(t *testing.T) {
	request := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"@pxpipe pin be concise"}]}`)
	reply := pinCommandResponse(request)
	body := decodePinReply(t, reply)
	if reply.contentType != "application/json" {
		t.Fatalf("content type = %q", reply.contentType)
	}
	if id, _ := body["id"].(string); !strings.HasPrefix(id, "msg_pxpipe_pin_") {
		t.Fatalf("id = %q", id)
	}
	if body["type"] != "message" || body["role"] != "assistant" || body["model"] != "claude-test" {
		t.Fatalf("unexpected message envelope: %#v", body)
	}
	if body["stop_reason"] != "end_turn" || body["stop_sequence"] != nil {
		t.Fatalf("unexpected stop fields: %#v", body)
	}
	text, _ := pinReplyContent(t, body, "content", 0, "text").(string)
	if want := "@pxpipe 1 pinned\nsession   (@pxpipe unpin <n>, or unpin all)\n\n1. be concise"; text != want {
		t.Fatalf("reply text = %q, want %q", text, want)
	}
	if got := pinReplyContent(t, body, "usage", "input_tokens"); got != float64(0) {
		t.Fatalf("input tokens = %v", got)
	}

	streamRequest := []byte(`{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"@pxpipe pin be concise"}]}`)
	stream := pinCommandResponse(streamRequest)
	if stream == nil || stream.contentType != "text/event-stream" {
		t.Fatalf("stream reply = %#v", stream)
	}
	frames := parsePinSSE(t, stream.body)
	wantEvents := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(frames) != len(wantEvents) {
		t.Fatalf("events = %d, want %d: %s", len(frames), len(wantEvents), stream.body)
	}
	for i, want := range wantEvents {
		if frames[i].event != want || frames[i].data["type"] != want {
			t.Fatalf("event %d = %#v, want %q", i, frames[i], want)
		}
	}
	if got := pinReplyContent(t, frames[2].data, "delta", "text"); got != text {
		t.Fatalf("stream text = %q, want %q", got, text)
	}
}

func TestPinCommandResponseOpenAIChat(t *testing.T) {
	request := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"@pxpipe pin be concise"}]}`)
	reply := pinCommandResponseOpenAI(request, ProtocolOpenAIChat)
	body := decodePinReply(t, reply)
	if reply.contentType != "application/json" || body["object"] != "chat.completion" || body["model"] != "gpt-test" {
		t.Fatalf("unexpected chat reply: %#v", body)
	}
	if id, _ := body["id"].(string); !strings.HasPrefix(id, "chatcmpl_pxpipe_pin_") {
		t.Fatalf("id = %q", id)
	}
	text, _ := pinReplyContent(t, body, "choices", 0, "message", "content").(string)
	if !strings.Contains(text, "1. be concise") {
		t.Fatalf("reply text = %q", text)
	}
	if pinReplyContent(t, body, "choices", 0, "finish_reason") != "stop" || pinReplyContent(t, body, "choices", 0, "logprobs") != nil {
		t.Fatalf("unexpected choice: %#v", pinReplyContent(t, body, "choices", 0))
	}
	if got := pinReplyContent(t, body, "usage", "total_tokens"); got != float64(0) {
		t.Fatalf("total tokens = %v", got)
	}

	streamRequest := []byte(`{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"@pxpipe pin be concise"}]}`)
	stream := pinCommandResponseOpenAI(streamRequest, ProtocolOpenAIChat)
	if stream == nil || stream.contentType != "text/event-stream" {
		t.Fatalf("stream reply = %#v", stream)
	}
	frames := parsePinSSE(t, stream.body)
	if len(frames) != 4 || !frames[3].done {
		t.Fatalf("chat stream frames = %#v", frames)
	}
	for i := 0; i < 3; i++ {
		if frames[i].data["object"] != "chat.completion.chunk" {
			t.Fatalf("frame %d object = %v", i, frames[i].data["object"])
		}
	}
	if got := pinReplyContent(t, frames[1].data, "choices", 0, "delta", "content"); got != text {
		t.Fatalf("stream text = %q, want %q", got, text)
	}
	if got := pinReplyContent(t, frames[2].data, "choices", 0, "finish_reason"); got != "stop" {
		t.Fatalf("finish reason = %v", got)
	}
}

func TestPinCommandResponseOpenAIResponses(t *testing.T) {
	request := []byte(`{
		"model":"gpt-test",
		"instructions":"@pxpipe pin follow AGENTS",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"@pxpipe pin use tabs"}]}]
	}`)
	reply := pinCommandResponseOpenAI(request, ProtocolOpenAIResponses)
	body := decodePinReply(t, reply)
	if reply.contentType != "application/json" || body["object"] != "response" || body["status"] != "completed" {
		t.Fatalf("unexpected Responses reply: %#v", body)
	}
	if id, _ := body["id"].(string); !strings.HasPrefix(id, "resp_pxpipe_pin_") {
		t.Fatalf("id = %q", id)
	}
	text, _ := pinReplyContent(t, body, "output", 0, "content", 0, "text").(string)
	if !strings.Contains(text, "follow AGENTS") || !strings.Contains(text, "1. use tabs") {
		t.Fatalf("reply text = %q", text)
	}
	if pinReplyContent(t, body, "output", 0, "content", 0, "type") != "output_text" {
		t.Fatalf("output part = %#v", pinReplyContent(t, body, "output", 0, "content", 0))
	}
	if got := pinReplyContent(t, body, "usage", "total_tokens"); got != float64(0) {
		t.Fatalf("total tokens = %v", got)
	}

	streamRequest := []byte(strings.Replace(string(request), `"model":"gpt-test",`, `"model":"gpt-test","stream":true,`, 1))
	stream := pinCommandResponseOpenAI(streamRequest, ProtocolOpenAIResponses)
	if stream == nil || stream.contentType != "text/event-stream" {
		t.Fatalf("stream reply = %#v", stream)
	}
	frames := parsePinSSE(t, stream.body)
	wantEvents := []string{
		"response.created", "response.in_progress", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("events = %d, want %d: %s", len(frames), len(wantEvents), stream.body)
	}
	for i, want := range wantEvents {
		if frames[i].event != want || frames[i].data["type"] != want || frames[i].data["sequence_number"] != float64(i) {
			t.Fatalf("event %d = %#v, want %q", i, frames[i], want)
		}
	}
	if frames[4].data["delta"] != text {
		t.Fatalf("delta = %q, want %q", frames[4].data["delta"], text)
	}
	if got := pinReplyContent(t, frames[8].data, "response", "output", 0, "content", 0, "text"); got != text {
		t.Fatalf("completed text = %q, want %q", got, text)
	}
}

func TestPinCommandResponseFailsOpen(t *testing.T) {
	if got := pinCommandResponse([]byte(`{"messages":`)); got != nil {
		t.Fatalf("malformed request got reply: %#v", got)
	}
	if got := pinCommandResponse([]byte(`{"messages":[{"role":"user","content":"@pxpipe pin concise\ndo work"}]}`)); got != nil {
		t.Fatalf("mixed request got reply: %#v", got)
	}
	toolLoop := []byte(`{"input":[
		{"type":"message","role":"user","content":"@pxpipe pin concise"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
	]}`)
	if got := pinCommandResponseOpenAI(toolLoop, ProtocolOpenAIResponses); got != nil {
		t.Fatalf("tool loop got reply: %#v", got)
	}
}
