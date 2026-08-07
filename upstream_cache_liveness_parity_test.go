package pxpipe

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func cacheLivenessParityImageBlock(data string) map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": data,
		},
	}
}

func cacheLivenessParityCCBody(t *testing.T, turns int) []byte {
	t.Helper()
	messages := make([]any, 0, turns)
	for i := 0; i < turns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{
			"role": role, "content": fmt.Sprintf("turn %d: %s", i, strings.Repeat("x", 3500)),
		})
	}
	body, err := json.Marshal(map[string]any{
		"model": "claude-3-5-sonnet",
		"system": []any{map[string]any{
			"type": "text", "text": strings.Repeat("x", 80_000),
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"messages": messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func cacheLivenessParityImageData(t *testing.T, body []byte) (slab, history string) {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	messages, _ := asArr(req["messages"])
	for i, value := range messages {
		message, _ := asMap(value)
		blocks, _ := asArr(message["content"])
		isHistory := false
		if len(blocks) > 0 && blockType(blocks[0]) == "text" {
			first, _ := asMap(blocks[0])
			text, _ := getStr(first, "text")
			isHistory = text == HistorySyntheticIntro
		}
		for _, value := range blocks {
			if blockType(value) != "image" {
				continue
			}
			block, _ := asMap(value)
			source, _ := asMap(block["source"])
			data, _ := getStr(source, "data")
			if isHistory {
				history += data
			} else if i == 0 {
				slab += data
			}
		}
	}
	return slab, history
}

func TestUpstreamCacheParityHistoryAttributionTracksSyntheticImages(t *testing.T) {
	sessions := newSessionStateStore()
	opts := &TransformOptions{historySessions: sessions}
	outA, infoA := TransformRequest(cacheLivenessParityCCBody(t, 15), opts)
	outB, infoB := TransformRequest(cacheLivenessParityCCBody(t, 65), opts)
	slabA, historyA := cacheLivenessParityImageData(t, outA)
	slabB, historyB := cacheLivenessParityImageData(t, outB)

	if infoA.CollapsedTurns == 0 || infoB.CollapsedTurns == 0 {
		t.Fatalf("history did not collapse: turns=%d/%d reasons=%q/%q", infoA.CollapsedTurns, infoB.CollapsedTurns, infoA.HistoryReason, infoB.HistoryReason)
	}
	if slabA == "" || historyA == "" || slabA == historyA {
		t.Fatalf("invalid slab/history precondition: slab=%d history=%d", len(slabA), len(historyA))
	}
	if slabB != slabA {
		t.Fatal("unchanged system slab rendered different image bytes")
	}
	if historyB == historyA {
		t.Fatal("larger collapsed history rendered identical image bytes")
	}
	if infoA.HistoryImageSha != sha8(historyA) || infoB.HistoryImageSha != sha8(historyB) {
		t.Fatalf("history telemetry hashes the wrong boundary: got=%q/%q want=%q/%q", infoA.HistoryImageSha, infoB.HistoryImageSha, sha8(historyA), sha8(historyB))
	}
	if infoA.HistoryImageSha == infoB.HistoryImageSha {
		t.Fatal("history image hash did not move with the history images")
	}
}

func TestUpstreamCacheParityPrefixAttributionAndMarkedBoundary(t *testing.T) {
	marker := map[string]any{
		"type": "image", "source": map[string]any{"data": "frozen"},
		"cache_control": map[string]any{"type": "ephemeral"},
	}
	request := func(toolName, systemText, liveTail string) map[string]any {
		return map[string]any{
			"tools":  []any{map[string]any{"name": toolName}},
			"system": []any{map[string]any{"type": "text", "text": systemText}},
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": HistorySyntheticIntro},
				marker,
				map[string]any{"type": "text", "text": liveTail},
			}}},
		}
	}

	base, ok := cachePrefixDiagnostics(request("Read", "stable system", "tail A"))
	if !ok {
		t.Fatal("base request had no cache prefix boundary")
	}
	if base.toolsSha8 == "" || base.systemSha8 == "" || base.headSha8 == "" || base.markedSha8 == "" {
		t.Fatalf("missing per-layer hash: %+v", base)
	}
	if base.markerPos != "m0.b1" || base.markedBytes >= base.bytes {
		t.Fatalf("wrong marked boundary: position=%q marked=%d prefix=%d", base.markerPos, base.markedBytes, base.bytes)
	}

	tail, _ := cachePrefixDiagnostics(request("Read", "stable system", "tail B grew"))
	if tail.markerPos != base.markerPos || tail.markedSha8 != base.markedSha8 || tail.markedBytes != base.markedBytes {
		t.Fatalf("live tail moved the Anthropic-marked span: before=%+v after=%+v", base, tail)
	}
	if tail.sha8 == base.sha8 || tail.headSha8 == base.headSha8 {
		t.Fatal("boundary-scoped prefix did not observe the changed live tail")
	}
	if tail.toolsSha8 != base.toolsSha8 || tail.systemSha8 != base.systemSha8 {
		t.Fatal("live-tail change leaked into tools or system attribution")
	}

	tools, _ := cachePrefixDiagnostics(request("Grep", "stable system", "tail A"))
	if tools.toolsSha8 == base.toolsSha8 || tools.systemSha8 != base.systemSha8 || tools.headSha8 != base.headSha8 || tools.sha8 == base.sha8 {
		t.Fatalf("tool-only change was misattributed: before=%+v after=%+v", base, tools)
	}
	system, _ := cachePrefixDiagnostics(request("Read", "changed system", "tail A"))
	if system.systemSha8 == base.systemSha8 || system.toolsSha8 != base.toolsSha8 || system.headSha8 != base.headSha8 || system.sha8 == base.sha8 {
		t.Fatalf("system-only change was misattributed: before=%+v after=%+v", base, system)
	}
}

func TestUpstreamCacheParityResponseOutcomeMatrix(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantDead bool
	}{
		{name: "413", status: 413, wantDead: true},
		{name: "400 prompt is too long", status: 400, body: `{"error":{"message":"prompt is too long: 245000 tokens > 200000 maximum"}}`, wantDead: true},
		{name: "400 prompt_too_long", status: 400, body: `{"error":{"type":"prompt_too_long"}}`, wantDead: true},
		{name: "400 request_too_large", status: 400, body: `{"error":{"type":"request_too_large","message":"request too large"}}`, wantDead: true},
		{name: "400 too many images", status: 400, body: `{"error":{"message":"too many images in request"}}`, wantDead: true},
		{name: "200", status: 200},
		{name: "400 no body", status: 400},
		{name: "400 invalid model", status: 400, body: `{"error":{"message":"invalid model"}}`},
		{name: "401", status: 401},
		{name: "403", status: 403},
		{name: "404", status: 404},
		{name: "429", status: 429},
		{name: "500", status: 500},
		{name: "502", status: 502},
		{name: "503", status: 503},
		{name: "504", status: 504},
		{name: "529", status: 529},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := newSessionStateStore()
			now := time.UnixMilli(1_700_000_000_000)
			sessions.noteHistoryRequest("session", now)
			sessions.noteCacheOutcome("session", 100_000, 0)
			sessions.accountAnthropicResponse("session", tt.status, nil, []byte(tt.body))

			sessions.mu.Lock()
			dead := sessions.entries["session"].Value.(*sessionStateEntry).record.cacheDead
			sessions.mu.Unlock()
			if dead != tt.wantDead {
				t.Fatalf("cacheDead = %v, want %v", dead, tt.wantDead)
			}
			if cold := sessions.noteHistoryRequest("session", now.Add(time.Second)).cold; cold != tt.wantDead {
				t.Fatalf("next request cold = %v, want %v", cold, tt.wantDead)
			}
		})
	}
}

func TestUpstreamCacheParitySessionTransitions(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	turn := func(s *sessionStateStore, at time.Time, read, create int64) historySessionState {
		state := s.noteHistoryRequest("session", at)
		s.noteCacheOutcome("session", read, create)
		return state
	}

	t.Run("mark bookkeeping and isolation", func(t *testing.T) {
		s := newSessionStateStore()
		s.markCacheDead("")
		if len(s.entries) != 0 {
			t.Fatal("empty session key invented state")
		}
		s.markCacheDead("failed")
		if s.entries["failed"] == nil || !s.entries["failed"].Value.(*sessionStateEntry).record.cacheDead {
			t.Fatal("explicit rejection did not flag its session")
		}
		if s.entries["unrelated"] != nil {
			t.Fatal("explicit rejection invented unrelated state")
		}
	})

	t.Run("read and create survive long gaps", func(t *testing.T) {
		for _, usage := range []struct {
			name         string
			read, create int64
		}{{name: "read", read: 100_000}, {name: "create", create: 66_119}} {
			t.Run(usage.name, func(t *testing.T) {
				s := newSessionStateStore()
				turn(s, t0, usage.read, usage.create)
				if s.noteHistoryRequest("session", t0.Add(10*time.Minute)).cold {
					t.Fatal("provider-confirmed cache went cold from wall clock")
				}
			})
		}
	})

	t.Run("always zero is not a dead cache", func(t *testing.T) {
		s := newSessionStateStore()
		turn(s, t0, 0, 0)
		turn(s, t0.Add(time.Minute), 0, 0)
		if s.noteHistoryRequest("session", t0.Add(2*time.Minute)).cold {
			t.Fatal("never-cached session went cold")
		}
	})

	t.Run("live then zero is immediately cold", func(t *testing.T) {
		s := newSessionStateStore()
		turn(s, t0, 120_000, 0)
		turn(s, t0.Add(time.Minute), 0, 0)
		if !s.noteHistoryRequest("session", t0.Add(2*time.Minute)).cold {
			t.Fatal("provider-confirmed missing cache stayed warm")
		}
	})

	t.Run("rejection is consumed and creation restores warm", func(t *testing.T) {
		s := newSessionStateStore()
		turn(s, t0, 100_000, 0)
		s.markCacheDead("session")
		if !s.noteHistoryRequest("session", t0.Add(time.Second)).cold {
			t.Fatal("rejection did not make the next request cold")
		}
		s.noteCacheOutcome("session", 0, 50_000)
		if s.noteHistoryRequest("session", t0.Add(2*time.Second)).cold {
			t.Fatal("fresh cache creation did not restore warm state")
		}
	})

	t.Run("unknown fallback uses one hour horizon", func(t *testing.T) {
		inside := newSessionStateStore()
		if inside.noteHistoryRequest("session", t0).cold || inside.noteHistoryRequest("session", t0.Add(30*time.Minute)).cold {
			t.Fatal("unknown session went cold inside fallback horizon")
		}
		outside := newSessionStateStore()
		outside.noteHistoryRequest("session", t0)
		if !outside.noteHistoryRequest("session", t0.Add(90*time.Minute)).cold {
			t.Fatal("unknown session stayed warm past fallback horizon")
		}
	})

	t.Run("new and ghost sessions stay unknown", func(t *testing.T) {
		s := newSessionStateStore()
		if s.noteHistoryRequest("new", time.Time{}).cold {
			t.Fatal("never-seen session was cold")
		}
		ghost := newSessionStateStore()
		ghost.noteCacheOutcome("ghost", 0, 0)
		if len(ghost.entries) != 0 {
			t.Fatal("outcome for an untransformed session invented state")
		}
	})
}

func TestUpstreamContextMapParityTransformImageTelemetry(t *testing.T) {
	t.Run("client images and ordinary wire agreement", func(t *testing.T) {
		images := make([]any, 12)
		for i := range images {
			images[i] = cacheLivenessParityImageBlock("client")
		}
		body, err := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": images}}})
		if err != nil {
			t.Fatal(err)
		}
		out, info := TransformRequest(body, &TransformOptions{historySessions: newSessionStateStore()})
		var request map[string]any
		if err := json.Unmarshal(out, &request); err != nil {
			t.Fatal(err)
		}
		messages, _ := asArr(request["messages"])
		if info.NativeImages != 12 || info.WireImages != 12 || info.WireImages != countNativeImages(messages) {
			t.Fatalf("native/wire telemetry = %d/%d, body=%d", info.NativeImages, info.WireImages, countNativeImages(messages))
		}
		if info.WireImages != info.ImageCount+info.NativeImages {
			t.Fatalf("ordinary wire count disagrees with rendered + native: %+v", info)
		}
	})

	t.Run("rendered pages absorbed by history", func(t *testing.T) {
		messages := make([]any, 0, 60)
		for i := 0; i < 60; i++ {
			content := []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("turn %d: %s", i, strings.Repeat("h", 1000))},
			}
			if i < 50 {
				content = append(content, cacheLivenessParityImageBlock("rendered"))
			} else if i >= 58 {
				content = append(content, cacheLivenessParityImageBlock("client"))
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": content})
		}
		req := map[string]any{"messages": messages}
		info := &TransformInfo{ImageCount: 50, NativeImages: 2, FirstUserSha8: "session"}
		cpt := 1.0
		opts := resolveOptions(&TransformOptions{CharsPerToken: &cpt, historySessions: newSessionStateStore()})
		collapsed, err := runHistoryCollapse(req, info, opts, map[rune]int{}, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if !collapsed {
			t.Fatalf("history did not collapse: reason=%q", info.HistoryReason)
		}
		finalizeWireTelemetry(req, info)
		outgoing, _ := asArr(req["messages"])
		if info.WireImages != countNativeImages(outgoing) {
			t.Fatalf("wire telemetry=%d body=%d", info.WireImages, countNativeImages(outgoing))
		}
		if info.WireImages >= info.ImageCount+info.NativeImages {
			t.Fatalf("absorbed rendered pages were not visible: rendered=%d native=%d wire=%d", info.ImageCount, info.NativeImages, info.WireImages)
		}
	})

	t.Run("budget skips count blocks left as text", func(t *testing.T) {
		for _, count := range []int{1, 3} {
			t.Run(fmt.Sprintf("%d", count), func(t *testing.T) {
				blocks := make([]any, count)
				keptText := "kept as text " + strings.Repeat("x", 7000)
				for i := range blocks {
					blocks[i] = map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("t%d", i), "content": keptText}
				}
				req := map[string]any{"messages": []any{map[string]any{"role": "user", "content": blocks}}}
				info := &TransformInfo{NativeImages: 95}
				opts := resolveOptions(nil)
				if err := compressToolResults(req, opts, info, denseGateGeometry(opts), map[rune]int{}); err != nil {
					t.Fatal(err)
				}
				if info.ImageBudgetSkips != count || info.PassthroughReasons["image_budget"] != count || info.ImageCount != 0 {
					t.Fatalf("skip telemetry for %d blocks: skips=%d reasons=%v images=%d", count, info.ImageBudgetSkips, info.PassthroughReasons, info.ImageCount)
				}
				messages, _ := asArr(req["messages"])
				message, _ := asMap(messages[0])
				kept, _ := asArr(message["content"])
				for _, value := range kept {
					block, _ := asMap(value)
					if content, _ := getStr(block, "content"); content != keptText {
						t.Fatalf("over-budget block was not preserved as text: %#v", value)
					}
				}
			})
		}
	})
}
