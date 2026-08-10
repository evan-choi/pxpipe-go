package pxpipe

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func upstreamImageBlock() map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo=",
		},
	}
}

func TestToolResultProfitabilityUsesConfiguredCapBeforeHeadroom(t *testing.T) {
	cpt := 20.0
	o := resolveOptions(&TransformOptions{CharsPerToken: &cpt})
	o.MinToolResultChars = 0
	denseGeo := denseGateGeometry(o)
	var candidate string
	for chars := 20_000; chars <= 200_000; chars += 1_000 {
		text := strings.Repeat("x ", chars/2)
		inner := maybeReflow(compactSlabWhitespace(text), o.Reflow)
		onePageGate := isCompressionProfitable(inner, denseGeo.cols, 1, o.CharsPerToken, 0, 0, true, denseGeo.maxChars, denseGeo)
		configuredGate := isCompressionProfitable(inner, denseGeo.cols, o.MaxImagesPerToolResult, o.CharsPerToken, 0, 0, true, denseGeo.maxChars, denseGeo)
		if onePageGate && !configuredGate {
			candidate = text
			break
		}
	}
	if candidate == "" {
		t.Fatal("failed to construct a low-headroom profitability boundary")
	}

	for _, arrayContent := range []bool{false, true} {
		name := "string"
		var content any = candidate
		if arrayContent {
			name = "array"
			content = []any{map[string]any{"type": "text", "text": candidate}}
		}
		t.Run(name, func(t *testing.T) {
			req := map[string]any{"messages": []any{map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "t1", "content": content,
				}},
			}}}
			info := &TransformInfo{NativeImages: 94}
			if err := compressToolResults(req, o, info, denseGeo, map[rune]int{}); err != nil {
				t.Fatal(err)
			}
			if info.ImageCount != 0 || info.PassthroughReasons["not_profitable"] != 1 {
				t.Fatalf("low-headroom result was truncated into images: %+v", info)
			}
		})
	}
}

func TestZeroImageHeadroomDoesNotConsumeCacheDeadSignal(t *testing.T) {
	sessions := newSessionStateStore()
	key := "session"
	now := time.Now()
	sessions.noteHistoryRequest(key, now)
	sessions.noteCacheOutcome(key, 1, 0)
	sessions.markCacheDead(key)
	o := resolveOptions(&TransformOptions{historySessions: sessions})
	req := map[string]any{"messages": historyMessages(20, 200)}
	info := &TransformInfo{FirstUserSha8: key, NativeImages: 95}
	if _, err := runHistoryCollapse(req, info, o, map[rune]int{}, 0, false); err != nil {
		t.Fatal(err)
	}
	if got := sessions.noteHistoryRequest(key, now.Add(time.Second)); !got.cold {
		t.Fatal("zero-headroom history attempt consumed the cache-dead signal")
	}
}

func TestStripBillingLineAnywhere(t *testing.T) {
	header := "x-anthropic-billing-header: cc_prev_req=req_123"
	head := strings.Repeat("head ", 20)
	tail := strings.Repeat("tail ", 20)
	clean := head + "\n" + tail

	for name, input := range map[string]string{
		"leading":  header + "\n" + clean,
		"middle":   head + "\n" + header + "\n" + tail,
		"trailing": clean + "\n" + header,
	} {
		t.Run(name, func(t *testing.T) {
			kept, body := stripBillingLine(input)
			if kept != header {
				t.Fatalf("kept = %q, want %q", kept, header)
			}
			if body != clean {
				t.Fatalf("body differs after removing billing line:\n got %q\nwant %q", body, clean)
			}
		})
	}
}

func TestCountNativeImagesAtBothNestingLevels(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": []any{upstreamImageBlock(), map[string]any{
			"type": "tool_result", "tool_use_id": "t1", "content": []any{
				map[string]any{"type": "text", "text": "ok"}, upstreamImageBlock(), upstreamImageBlock(),
			},
		}}},
		map[string]any{"role": "assistant", "content": "plain text"},
	}
	if got := countNativeImages(msgs); got != 3 {
		t.Fatalf("countNativeImages() = %d, want 3", got)
	}
	info := &TransformInfo{ImageCount: 7, NativeImages: 8}
	if got := imageHeadroom(info); got != 80 {
		t.Fatalf("imageHeadroom() = %d, want 80", got)
	}
}

func TestSlabRenderIsAtomicWhenActualPagesExceedHeadroom(t *testing.T) {
	images := make([]any, 94)
	for i := range images {
		images[i] = upstreamImageBlock()
	}
	req := map[string]any{
		"model":  "claude-3-5-sonnet",
		"system": strings.Repeat("system slab text ", 2500),
		"messages": []any{
			map[string]any{"role": "user", "content": images},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	cpt := 1.0
	out, info := TransformRequest(body, &TransformOptions{CharsPerToken: &cpt})
	if info.ImageCount != 0 || info.ImageBudgetSkips == 0 {
		t.Fatalf("imageCount=%d imageBudgetSkips=%d, want atomic skip", info.ImageCount, info.ImageBudgetSkips)
	}
	if !strings.HasPrefix(info.Reason, "image_budget (slab needs ") {
		t.Fatalf("reason = %q", info.Reason)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs, _ := asArr(got["messages"])
	if wire := countNativeImages(msgs); wire != 94 || info.WireImages != wire {
		t.Fatalf("wire images: body=%d telemetry=%d, want 94", wire, info.WireImages)
	}
}

func TestToolResultAndHistoryShareImageHeadroom(t *testing.T) {
	images := make([]any, 94)
	for i := range images {
		images[i] = upstreamImageBlock()
	}
	cpt := 1.0
	req := map[string]any{
		"model":  "claude-3-5-sonnet",
		"system": strings.Repeat("s", 20_000),
		"messages": []any{
			map[string]any{"role": "user", "content": images},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "t1", "content": "RESULT " + strings.Repeat("r", 50_000),
			}}},
		},
	}
	body, _ := json.Marshal(req)
	out, info := TransformRequest(body, &TransformOptions{CharsPerToken: &cpt})
	if info.ImageCount != 1 || info.ToolResultImgs != 0 || info.ImageBudgetSkips == 0 {
		t.Fatalf("imageCount=%d toolResultImgs=%d skips=%d", info.ImageCount, info.ToolResultImgs, info.ImageBudgetSkips)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs, _ := asArr(got["messages"])
	if wire := countNativeImages(msgs); wire != 95 || info.WireImages != wire {
		t.Fatalf("wire images: body=%d telemetry=%d, want 95", wire, info.WireImages)
	}
	if !strings.Contains(string(out), "RESULT ") {
		t.Fatal("over-budget tool result text was dropped")
	}

	// With no slab, the early-finalize history path observes the same exhausted
	// headroom and leaves the transcript untouched rather than adding image 96.
	historyImages := append(append([]any{}, images...), upstreamImageBlock())
	historyReq := map[string]any{"model": "claude-3-5-sonnet", "messages": []any{
		map[string]any{"role": "user", "content": historyImages},
	}}
	for i := 0; i < 20; i++ {
		historyReq["messages"] = append(historyReq["messages"].([]any), map[string]any{
			"role": "assistant", "content": "turn " + strings.Repeat("h", 3000),
		})
	}
	historyBody, _ := json.Marshal(historyReq)
	historyOut, historyInfo := TransformRequest(historyBody, &TransformOptions{CharsPerToken: &cpt})
	if historyInfo.CollapsedImages != 0 || historyInfo.HistoryReason != "too_many_images" || historyInfo.WireImages != 95 {
		t.Fatalf("history cap telemetry: %+v", historyInfo)
	}
	if count := strings.Count(string(historyOut), `"type":"image"`); count != 95 {
		t.Fatalf("history wire images = %d, want 95", count)
	}
}

func TestHistoryImageHashSelectsSyntheticMessage(t *testing.T) {
	slabData := "slab-image-data"
	historyData := "history-image-data"
	image := func(data string) map[string]any {
		return map[string]any{"type": "image", "source": map[string]any{"data": data}}
	}
	messages := []any{
		map[string]any{"role": "user", "content": []any{image(slabData)}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": HistorySyntheticIntro}, image(historyData),
		}},
	}
	if got, want := historyImageSha8(messages), sha8(historyData); got != want {
		t.Fatalf("historyImageSha8() = %q, want history digest %q", got, want)
	}
	if historyImageSha8(messages) == sha8(slabData) {
		t.Fatal("history digest incorrectly describes the slab message")
	}
}

func TestHistoryCollapseSeesOriginalToolResults(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "t_old", "name": "Read", "input": map[string]any{"path": "notes.txt"},
		}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "t_old", "content": "RESULT t_old\n" + strings.Repeat("x", 40_000),
		}}},
	}
	for i := 0; i < 60; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{
			"role": role, "content": "turn " + strconv.Itoa(i) + ": " + strings.Repeat("y", 3500),
		})
	}
	body, err := json.Marshal(map[string]any{
		"model": "claude-3-5-sonnet",
		"system": []any{map[string]any{
			"type": "text", "text": "SLAB\n" + strings.Repeat("s", 80_000),
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"messages": messages,
	})
	if err != nil {
		t.Fatal(err)
	}

	withImaging := &TransformOptions{historySessions: newSessionStateStore()}
	_, got := TransformRequest(body, withImaging)
	without := false
	withoutImaging := &TransformOptions{CompressToolResults: &without, historySessions: newSessionStateStore()}
	_, want := TransformRequest(body, withoutImaging)
	if got.CollapsedTurns == 0 || got.CollapsedChars <= 40_000 {
		t.Fatalf("fixture did not collapse the old tool result: %+v", got)
	}
	if got.CollapsedTurns != want.CollapsedTurns || got.CollapsedChars != want.CollapsedChars {
		t.Fatalf("history changed after enabling tool-result imaging: turns %d/%d, chars %d/%d",
			got.CollapsedTurns, want.CollapsedTurns, got.CollapsedChars, want.CollapsedChars)
	}
}

func TestClaudeHistoryUsesProfileGeometry(t *testing.T) {
	fable := historyGateGeometry(resolveOptions(&TransformOptions{Model: "claude-fable-5"}))
	if fable.cols != 312 || fable.style.Font == "jetbrains-mono-14" {
		t.Fatalf("Fable history geometry = cols %d, font %q", fable.cols, fable.style.Font)
	}
	opus := historyGateGeometry(resolveOptions(&TransformOptions{Model: "claude-opus-5"}))
	if opus.cols != 172 || opus.style.Font != "jetbrains-mono-14" {
		t.Fatalf("Opus history geometry = cols %d, font %q", opus.cols, opus.style.Font)
	}
	cols := 160
	overridden := historyGateGeometry(resolveOptions(&TransformOptions{Model: "claude-opus-5", Cols: &cols}))
	if overridden.cols != cols || overridden.style.Font != "jetbrains-mono-14" {
		t.Fatalf("overridden history geometry = cols %d, font %q", overridden.cols, overridden.style.Font)
	}
}

func TestCachePrefixComponentAndMarkedDiagnostics(t *testing.T) {
	marked := map[string]any{"type": "image", "source": map[string]any{"data": "a"}, "cache_control": map[string]any{"type": "ephemeral"}}
	req := map[string]any{
		"tools":  []any{map[string]any{"name": "Read"}},
		"system": []any{map[string]any{"type": "text", "text": "system"}},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			marked, map[string]any{"type": "text", "text": endOfRenderedContext}, map[string]any{"type": "text", "text": "live"},
		}}},
	}
	a, ok := cachePrefixDiagnostics(req)
	if !ok {
		t.Fatal("cachePrefixDiagnostics() did not find the slab boundary")
	}
	if a.toolsSha8 == "" || a.systemSha8 == "" || a.headSha8 == "" || a.markedSha8 == "" {
		t.Fatalf("missing component digest: %+v", a)
	}
	if a.markerPos != "m0.b0" || a.markedBytes >= a.bytes {
		t.Fatalf("markerPos=%q markedBytes=%d bytes=%d", a.markerPos, a.markedBytes, a.bytes)
	}

	req["tools"] = []any{map[string]any{"name": "Read"}, map[string]any{"name": "Grep"}}
	b, _ := cachePrefixDiagnostics(req)
	if b.toolsSha8 == a.toolsSha8 || b.systemSha8 != a.systemSha8 || b.headSha8 != a.headSha8 {
		t.Fatalf("tool-only change was not attributed: before=%+v after=%+v", a, b)
	}
}

func TestCachePrefixDiagnosticsMatchesJoinedParts(t *testing.T) {
	tool := map[string]any{"name": "Read", "description": "한글"}
	systemA := map[string]any{"type": "text", "text": "system"}
	systemB := map[string]any{"type": "text", "text": "😀"}
	history := map[string]any{"type": "text", "text": HistorySyntheticIntro}
	marker := map[string]any{
		"type":          "text",
		"text":          "marked",
		"cache_control": map[string]any{"type": "ephemeral"},
	}
	req := map[string]any{
		"tools":  []any{tool},
		"system": []any{systemA, systemB},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{history, "raw\x00text"}},
			map[string]any{"role": "assistant", "content": []any{marker, map[string]any{"type": "text", "text": "tail"}}},
		},
	}
	got, ok := cachePrefixDiagnostics(req)
	if !ok {
		t.Fatal("cachePrefixDiagnostics() did not find history boundary")
	}
	join := func(values ...any) string {
		parts := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				parts = append(parts, text)
			} else {
				parts = append(parts, jsStringifyString(value))
			}
		}
		return strings.Join(parts, "\x00")
	}
	toolsText := join(tool)
	systemText := join(systemA, systemB)
	headText := join(history, "raw\x00text")
	allText := join(tool, systemA, systemB, history, "raw\x00text")
	markedText := join(tool, systemA, systemB, history, "raw\x00text", marker)
	if got.sha8 != sha8(allText) || got.bytes != u16len(allText) {
		t.Fatalf("aggregate digest = %+v, want sha=%s bytes=%d", got, sha8(allText), u16len(allText))
	}
	if got.toolsSha8 != sha8(toolsText) || got.systemSha8 != sha8(systemText) || got.headSha8 != sha8(headText) {
		t.Fatalf("component digests = %+v", got)
	}
	if got.markedSha8 != sha8(markedText) || got.markedBytes != u16len(markedText) || got.markerPos != "m1.b0" {
		t.Fatalf("marked digest = %+v, want sha=%s bytes=%d", got, sha8(markedText), u16len(markedText))
	}
}

func TestCachePrefixDiagnosticsWithoutMarkerStopsAfterSystem(t *testing.T) {
	tool := map[string]any{"name": "Read"}
	system := map[string]any{"type": "text", "text": "system"}
	history := map[string]any{"type": "text", "text": HistorySyntheticIntro}
	req := map[string]any{
		"tools":  []any{tool},
		"system": []any{system},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			history, map[string]any{"type": "text", "text": "tail"},
		}}},
	}
	got, ok := cachePrefixDiagnostics(req)
	if !ok {
		t.Fatal("cachePrefixDiagnostics() did not find history boundary")
	}
	marked := jsStringifyString(tool) + "\x00" + jsStringifyString(system)
	if got.markedSha8 != sha8(marked) || got.markedBytes != u16len(marked) || got.markerPos != "" {
		t.Fatalf("unmarked prefix = %+v, want sha=%s bytes=%d", got, sha8(marked), u16len(marked))
	}
}

func TestHistoryTuningUsesRenderedWidth(t *testing.T) {
	cols := 100
	o := resolveOptions(&TransformOptions{Cols: &cols})
	if o.historySessions == nil {
		t.Fatal("direct transforms must retain a sticky history session store")
	}
	geo := denseGateGeometry(o)
	if geo.maxChars != 9000 {
		t.Fatalf("maxChars = %d, want 9000 at 100 columns", geo.maxChars)
	}
	tuning := resolveHistoryGridTuning(&TransformInfo{}, geo)
	if tuning.pageChars != geo.maxChars || tuning.imageBudget != 80 || tuning.packFill || tuning.minFreezeStep != 0 {
		t.Fatalf("unexpected history tuning: %+v", tuning)
	}
	ho := historyDefaults()
	applyHistoryGridTuning(&ho, tuning)
	if ho.pageChars != tuning.pageChars || ho.imageBudget != tuning.imageBudget || ho.packFill || ho.minFreezeStep != 0 {
		t.Fatalf("history options not wired: %+v", ho)
	}
	info := &TransformInfo{}
	recordHistoryGridInfo(info, &historyCollapseInfo{freezeStep: 20, budgetTrimmed: true}, tuning)
	if info.HistoryFreezeStep != 20 || !info.HistoryBudgetTrimmed || info.HistoryPackFill {
		t.Fatalf("history telemetry not wired: %+v", info)
	}
}
