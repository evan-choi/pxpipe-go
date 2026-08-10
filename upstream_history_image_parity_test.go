package pxpipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func parityMarshal(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func parityDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func parityWireImages(t *testing.T, body []byte) int {
	t.Helper()
	out := parityDecode(t, body)
	messages, _ := asArr(out["messages"])
	return countNativeImages(messages)
}

func parityImageBlock() map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo=",
		},
	}
}

func parityClientImages(n int) map[string]any {
	blocks := make([]any, n)
	for i := range blocks {
		blocks[i] = parityImageBlock()
	}
	return map[string]any{"role": "user", "content": blocks}
}

func parityToolResult(id string, chars int) map[string]any {
	return map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": id,
			"content": "RESULT " + id + "\n" + strings.Repeat("x", chars),
		}},
	}
}

func parityConversation(turns, chars int, anchor string) []any {
	messages := make([]any, 0, turns+1)
	messages = append(messages, map[string]any{
		"role": "user", "content": anchor + strings.Repeat("a", 200),
	})
	for i := 0; i < turns; i++ {
		role := "assistant"
		if i%2 == 1 {
			role = "user"
		}
		messages = append(messages, map[string]any{
			"role": role, "content": fmt.Sprintf("turn %d: ", i) + strings.Repeat("x", chars),
		})
	}
	return messages
}

func parityTransform(t *testing.T, request map[string]any, cols int, sessions *sessionStateStore) ([]byte, *TransformInfo) {
	t.Helper()
	cpt := 1.0
	opts := &TransformOptions{CharsPerToken: &cpt, historySessions: sessions}
	if cols > 0 {
		opts.Cols = &cols
	}
	out, info := TransformRequest(parityMarshal(t, request), opts)
	if strings.HasPrefix(info.Reason, "parse_error") || strings.HasPrefix(info.Reason, "transform_error") {
		t.Fatalf("transform failed: %s", info.Reason)
	}
	return out, info
}

func TestUpstreamHistoryImageParity(t *testing.T) {
	t.Run("oversized user prompts batch by bytes", func(t *testing.T) {
		messages := make([]any, 0, 120)
		for i := 0; i < 120; i++ {
			role := "assistant"
			content := fmt.Sprintf("turn %d: ", i) + strings.Repeat("x", 400)
			if i%2 == 0 {
				role = "user"
				content = "PASTED " + strings.Repeat("y", 3500)
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		}

		o := historyDefaults()
		o.keepTail = 2
		o.minCollapsePrefix = 5
		o.collapseChunk = 0
		o.cols = 100
		o.pageChars = 9000
		o.imageBudget = 100
		out, info, err := collapseHistory(messages, func(string, int) bool { return true }, o)
		if err != nil {
			t.Fatal(err)
		}
		content, _ := asArr(out[0].(map[string]any)["content"])
		images := 0
		var cues strings.Builder
		for _, block := range content {
			switch blockType(block) {
			case "image":
				images++
			case "text":
				m, _ := asMap(block)
				text, _ := getStr(m, "text")
				if strings.Contains(text, "too long to carry as text") {
					cues.WriteString(text)
					cues.WriteByte('\n')
				}
			}
		}
		if images > 100 || images >= 59 {
			t.Fatalf("batched prompts emitted %d images; want <=100 and fewer than one per oversized prompt", images)
		}
		if info.collapsedImages != images {
			t.Fatalf("collapsedImages=%d, outgoing history images=%d", info.collapsedImages, images)
		}
		for _, turn := range []int{0, 10, 20, 58} {
			if !strings.Contains(cues.String(), fmt.Sprintf(`<user t="%d">`, turn)) {
				t.Errorf("batch cues do not attribute user turn %d", turn)
			}
		}
	})

	t.Run("history cap uses actual render width", func(t *testing.T) {
		for _, cols := range []int{80, 100, 200, 312} {
			t.Run(fmt.Sprintf("cols_%d", cols), func(t *testing.T) {
				request := map[string]any{
					"model":    "claude-3-5-sonnet",
					"messages": parityConversation(400, 3500, "SESSION ANCHOR: "),
				}
				out, info := parityTransform(t, request, cols, newSessionStateStore())
				if wire := parityWireImages(t, out); wire > AnthropicMaxImages {
					t.Fatalf("wire images=%d, provider cap=%d", wire, AnthropicMaxImages)
				}
				if info.CollapsedTurns == 0 {
					t.Fatal("large history was not collapsed")
				}
				if cols == 312 && info.HistoryFreezeStep <= 1 && !info.HistoryBudgetTrimmed {
					t.Fatalf("history fit without reporting coarsening or trimming: %+v", info)
				}
			})
		}
	})

	t.Run("slab and history share the provider cap", func(t *testing.T) {
		request := map[string]any{
			"model":  "claude-3-5-sonnet",
			"system": []any{map[string]any{"type": "text", "text": strings.Repeat("slab ", 20_000)}},
			"tools": []any{map[string]any{
				"name": "read", "description": strings.Repeat("tool docs ", 3000),
				"input_schema": map[string]any{"type": "object"},
			}},
			"messages": parityConversation(300, 3500, "SESSION ANCHOR: "),
		}
		out, info := parityTransform(t, request, 312, newSessionStateStore())
		if wire := parityWireImages(t, out); wire > AnthropicMaxImages {
			t.Fatalf("slab/tool-doc/history wire images=%d, provider cap=%d", wire, AnthropicMaxImages)
		}
		if info.ImageCount == 0 || info.CollapsedTurns == 0 {
			t.Fatalf("test did not exercise both slab and history imaging: imageCount=%d collapsedTurns=%d", info.ImageCount, info.CollapsedTurns)
		}
	})

	t.Run("freeze grid is monotonic and session scoped", func(t *testing.T) {
		sessions := newSessionStateStore()
		steps := make([]int, 0, 4)
		for _, turns := range []int{40, 120, 260, 400} {
			_, info := parityTransform(t, map[string]any{
				"model":    "claude-3-5-sonnet",
				"messages": parityConversation(turns, 3500, "SESSION ANCHOR: "),
			}, 312, sessions)
			if info.HistoryFreezeStep > 0 {
				steps = append(steps, info.HistoryFreezeStep)
			}
		}
		if len(steps) < 2 {
			t.Fatalf("only observed freeze steps %v", steps)
		}
		for i := 1; i < len(steps); i++ {
			if steps[i] < steps[i-1] {
				t.Fatalf("freeze grid became finer as history grew: %v", steps)
			}
		}
		coarse := steps[len(steps)-1]
		_, shrunk := parityTransform(t, map[string]any{
			"model":    "claude-3-5-sonnet",
			"messages": parityConversation(60, 3500, "SESSION ANCHOR: "),
		}, 312, sessions)
		if shrunk.HistoryFreezeStep != 0 && shrunk.HistoryFreezeStep < coarse {
			t.Fatalf("short replay re-cut grid from %d to %d", coarse, shrunk.HistoryFreezeStep)
		}

		_, other := parityTransform(t, map[string]any{
			"model":    "claude-3-5-sonnet",
			"messages": parityConversation(40, 3500, "A COMPLETELY DIFFERENT ANCHOR: "),
		}, 312, sessions)
		if other.HistoryPackFill {
			t.Fatal("new session inherited the old session's cache state")
		}
		if other.HistoryFreezeStep == 0 {
			t.Fatalf("new session did not independently establish a freeze grid; prior session step=%d", coarse)
		}
	})

	t.Run("pack fill requires and consumes a dead cache signal", func(t *testing.T) {
		sessions := newSessionStateStore()
		request := map[string]any{
			"model":    "claude-3-5-sonnet",
			"messages": parityConversation(120, 3500, "SESSION ANCHOR: "),
		}
		_, first := parityTransform(t, request, 312, sessions)
		if first.HistoryPackFill || first.FirstUserSha8 == "" || first.CollapsedTurns == 0 {
			t.Fatalf("warm baseline did not exercise history normally: %+v", first)
		}
		sessions.markCacheDead(first.FirstUserSha8)
		_, second := parityTransform(t, request, 312, sessions)
		if !second.HistoryPackFill {
			t.Fatal("dead cache did not authorize dense repacking")
		}
		if second.ImageCount > first.ImageCount {
			t.Fatalf("pack fill increased images from %d to %d", first.ImageCount, second.ImageCount)
		}
		_, third := parityTransform(t, map[string]any{
			"model":    "claude-3-5-sonnet",
			"messages": parityConversation(130, 3500, "SESSION ANCHOR: "),
		}, 312, sessions)
		if third.HistoryPackFill {
			t.Fatal("dead-cache signal was not consumed")
		}
	})

	t.Run("native image counting and headroom share one budget", func(t *testing.T) {
		nested := map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "t1",
				"content": []any{parityImageBlock(), map[string]any{"type": "text", "text": "hi"}, parityImageBlock()},
			}},
		}
		if got := countNativeImages([]any{parityClientImages(7)}); got != 7 {
			t.Fatalf("top-level native images=%d, want 7", got)
		}
		if got := countNativeImages([]any{nested}); got != 2 {
			t.Fatalf("nested native images=%d, want 2", got)
		}
		for name, messages := range map[string][]any{
			"nil":            nil,
			"empty":          {},
			"string_content": {map[string]any{"role": "user", "content": "plain text"}},
		} {
			if got := countNativeImages(messages); got != 0 {
				t.Errorf("%s native images=%d, want 0", name, got)
			}
		}
		base := imageHeadroom(&TransformInfo{})
		if base >= AnthropicMaxImages {
			t.Fatalf("headroom=%d does not include a safety margin below cap %d", base, AnthropicMaxImages)
		}
		if got := imageHeadroom(&TransformInfo{ImageCount: 10}); got != base-10 {
			t.Fatalf("our images cost %d headroom, want %d", got, base-10)
		}
		if got := imageHeadroom(&TransformInfo{NativeImages: 10}); got != base-10 {
			t.Fatalf("client images cost %d headroom, want %d", got, base-10)
		}
		if got := imageHeadroom(&TransformInfo{NativeImages: 500}); got != 0 {
			t.Fatalf("overfull headroom=%d, want 0", got)
		}
	})

	t.Run("tool result imaging degrades without exceeding wire cap", func(t *testing.T) {
		withSlab := func(messages []any) map[string]any {
			return map[string]any{
				"model":    "claude-3-5-sonnet",
				"system":   []any{map[string]any{"type": "text", "text": "SLAB\n" + strings.Repeat("s", 30_000)}},
				"messages": messages,
			}
		}

		out, empty := parityTransform(t, withSlab([]any{
			map[string]any{"role": "user", "content": "go"}, parityToolResult("t1", 40_000),
		}), 312, newSessionStateStore())
		if empty.NativeImages != 0 || empty.ToolResultImgs == 0 || parityWireImages(t, out) == 0 {
			t.Fatalf("empty-wire positive control did not image tool result: %+v", empty)
		}

		out, full := parityTransform(t, withSlab([]any{
			parityClientImages(AnthropicMaxImages), parityToolResult("t1", 40_000),
		}), 312, newSessionStateStore())
		if full.NativeImages != AnthropicMaxImages || parityWireImages(t, out) != AnthropicMaxImages {
			t.Fatalf("full-cap wire mismatch: native=%d wire=%d", full.NativeImages, parityWireImages(t, out))
		}
		if full.ImageBudgetSkips == 0 || full.PassthroughReasons["image_budget"] == 0 {
			t.Fatalf("full-cap degradation was not reported: %+v", full)
		}
		if !bytes.Contains(out, []byte("RESULT t1")) {
			t.Fatal("full-cap degradation dropped tool result text")
		}

		near := AnthropicMaxImages - 3
		out, _ = parityTransform(t, withSlab([]any{
			parityClientImages(near), parityToolResult("t1", 40_000),
			parityToolResult("t2", 40_000), parityToolResult("t3", 40_000),
		}), 312, newSessionStateStore())
		if wire := parityWireImages(t, out); wire > AnthropicMaxImages {
			t.Fatalf("near-cap tool results emitted %d wire images", wire)
		}
	})

	t.Run("multi-page slab admission is quantitative and atomic", func(t *testing.T) {
		for _, tc := range []struct {
			clients, slabChars int
		}{{94, 400_000}, {90, 400_000}, {80, 800_000}} {
			name := fmt.Sprintf("clients_%d_slab_%d", tc.clients, tc.slabChars)
			t.Run(name, func(t *testing.T) {
				request := map[string]any{
					"model":  "claude-3-5-sonnet",
					"system": []any{map[string]any{"type": "text", "text": "SLAB\n" + strings.Repeat("s", tc.slabChars)}},
					"messages": []any{parityClientImages(tc.clients), map[string]any{
						"role": "user", "content": "hi " + strings.Repeat("h", 200),
					}},
				}
				out, info := parityTransform(t, request, 312, newSessionStateStore())
				if wire := parityWireImages(t, out); wire > AnthropicMaxImages {
					t.Fatalf("wire images=%d, provider cap=%d", wire, AnthropicMaxImages)
				}
				if info.ImageCount != 0 || !strings.HasPrefix(info.Reason, "image_budget") {
					t.Fatalf("oversized slab was partially admitted: imageCount=%d reason=%q", info.ImageCount, info.Reason)
				}
			})
		}
	})

	t.Run("wire telemetry counts outgoing images after history absorption", func(t *testing.T) {
		messages := []any{map[string]any{"role": "user", "content": "ANCHOR " + strings.Repeat("a", 200)}}
		for i := 0; i < 60; i++ {
			messages = append(messages, map[string]any{
				"role": "assistant", "content": fmt.Sprintf("turn %d: ", i) + strings.Repeat("x", 4000),
			})
			if i%3 == 0 {
				messages = append(messages, parityToolResult(fmt.Sprintf("t%d", i), 90_000))
			} else {
				messages = append(messages, map[string]any{
					"role": "user", "content": fmt.Sprintf("reply %d: ", i) + strings.Repeat("r", 2000),
				})
			}
		}
		out, info := parityTransform(t, map[string]any{
			"model":    "claude-opus-5",
			"system":   []any{map[string]any{"type": "text", "text": "SLAB\n" + strings.Repeat("s", 50_000)}},
			"messages": messages,
		}, 312, newSessionStateStore())
		actual := parityWireImages(t, out)
		if info.WireImages != actual {
			t.Fatalf("wire telemetry=%d, outgoing body=%d", info.WireImages, actual)
		}
		if info.ImageCount <= actual {
			t.Fatalf("test did not observe absorbed rendered images: render=%d wire=%d", info.ImageCount, actual)
		}
		if actual > AnthropicMaxImages {
			t.Fatalf("outgoing body has %d images, cap=%d", actual, AnthropicMaxImages)
		}

		plainOut, plain := parityTransform(t, map[string]any{
			"model":    "claude-3-5-sonnet",
			"system":   []any{map[string]any{"type": "text", "text": "SLAB\n" + strings.Repeat("s", 30_000)}},
			"messages": []any{parityClientImages(2), map[string]any{"role": "user", "content": "hi"}},
		}, 312, newSessionStateStore())
		plainActual := parityWireImages(t, plainOut)
		if plain.WireImages != plainActual || plain.WireImages != plain.ImageCount+plain.NativeImages {
			t.Fatalf("unabsorbed wire telemetry=%d actual=%d rendered=%d native=%d", plain.WireImages, plainActual, plain.ImageCount, plain.NativeImages)
		}
	})

	t.Run("billing line becomes a header without changing slab bytes", func(t *testing.T) {
		const header = "x-anthropic-billing-header: cc_version=2.1.222; cc_prev_req=req_011Cdk3"
		head := strings.Repeat("real prompt text. ", 1250)
		tail := strings.Repeat("more ground truth. ", 1250)
		clean := head + "\n" + tail
		transformSystem := func(system any) ([]byte, *TransformInfo) {
			return parityTransform(t, map[string]any{
				"model":    "claude-3-5-sonnet",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"system":   system,
			}, 312, newSessionStateStore())
		}
		_, baseline := transformSystem(clean)
		if !baseline.Compressed || len(baseline.ImagePNGs) == 0 {
			t.Fatalf("clean positive control did not render a slab: %+v", baseline)
		}
		for name, system := range map[string]any{
			"leading":  header + "\n" + clean,
			"middle":   head + "\n" + header + "\n" + tail,
			"trailing": clean + "\n" + header,
			"remainder_block": []any{
				map[string]any{"type": "text", "text": header},
				map[string]any{"type": "text", "text": clean, "cache_control": map[string]any{"type": "ephemeral"}},
			},
		} {
			t.Run(name, func(t *testing.T) {
				out, info := transformSystem(system)
				if len(info.ImagePNGs) != len(baseline.ImagePNGs) {
					t.Fatalf("slab page count changed from %d to %d", len(baseline.ImagePNGs), len(info.ImagePNGs))
				}
				for i := range baseline.ImagePNGs {
					if !bytes.Equal(info.ImagePNGs[i], baseline.ImagePNGs[i]) {
						t.Fatalf("slab PNG page %d changed when billing header was %s", i, name)
					}
				}
				if info.BillingLine != header || bytes.Contains(out, []byte(header)) {
					t.Fatalf("billing line = %q, still in body=%v", info.BillingLine, bytes.Contains(out, []byte(header)))
				}
			})
		}
	})
}
