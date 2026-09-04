package pxpipe

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func imageByteBudgetBody(messages []any) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-3-5-sonnet",
		"system":   "SLAB\n" + strings.Repeat("s", 60_000),
		"messages": messages,
	})
	return body
}

func imageByteBudgetOptions(maxImageBytes int) *TransformOptions {
	return &TransformOptions{
		Model:           "claude-3-5-sonnet",
		MaxImageBytes:   &maxImageBytes,
		historySessions: newSessionStateStore(),
	}
}

func TestCountNativeImageBytesAtBothNestingLevels(t *testing.T) {
	image := func(bytes int) map[string]any {
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png",
			"data": base64.StdEncoding.EncodeToString(make([]byte, bytes)),
		}}
	}
	messages := []any{map[string]any{"role": "user", "content": []any{
		image(3_000), map[string]any{"type": "tool_result", "content": []any{image(6_000)}},
	}}}
	if got := countNativeImageBytes(messages); got != 9_000 {
		t.Fatalf("countNativeImageBytes() = %d, want 9000", got)
	}
}

func TestImageByteBudgetAdmitsGroupsAtomically(t *testing.T) {
	low := 1_000
	body := imageByteBudgetBody([]any{map[string]any{"role": "user", "content": "go"}})
	out, info := TransformRequest(body, imageByteBudgetOptions(low))
	if info.ImageCount != 0 || info.ImageByteSkips == 0 || !strings.HasPrefix(info.Reason, "image_bytes") {
		t.Fatalf("low-budget slab was partially admitted: %+v", info)
	}
	if !strings.Contains(string(out), "SLAB") {
		t.Fatal("low-budget slab source text was dropped")
	}

	high := 18 << 20
	_, slab := TransformRequest(body, imageByteBudgetOptions(high))
	if slab.ImageCount == 0 || slab.ImageBytes == 0 {
		t.Fatalf("slab fixture did not render: %+v", slab)
	}
	limit := slab.ImageBytes
	toolBody := imageByteBudgetBody([]any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "t1", "content": "RESULT t1\n" + strings.Repeat("x", 40_000),
		}}},
	})
	out, info = TransformRequest(toolBody, imageByteBudgetOptions(limit))
	if info.ImageCount == 0 || info.ToolResultImgs != 0 || info.ImageByteSkips == 0 || !info.ImageBytesNearLimit {
		t.Fatalf("tool-result byte admission = %+v", info)
	}
	if !strings.Contains(string(out), "RESULT t1") {
		t.Fatal("rejected tool-result image group dropped its source text")
	}
}

func TestCallerImagesConsumeByteBudgetWithoutBeingRemoved(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(make([]byte, 3_000))
	body := imageByteBudgetBody([]any{
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": data},
		}}},
		map[string]any{"role": "user", "content": "go"},
	})
	limit := 3_200
	out, info := TransformRequest(body, imageByteBudgetOptions(limit))
	if info.NativeImageBytes != 3_000 || info.ImageCount != 0 || info.ImageByteSkips == 0 || !info.ImageBytesNearLimit {
		t.Fatalf("caller image byte budget = %+v", info)
	}
	if !strings.Contains(string(out), data) {
		t.Fatal("caller image was removed to make room")
	}
}
