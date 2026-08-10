package pxpipe

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/evan-choi/pxpipe-go/internal/o200k"
	"github.com/evan-choi/pxpipe-go/render"
)

// Port of src/core/openai.ts: OpenAI Chat Completions + Responses API
// transformer for the GPT family. No cache-control breakpoints; images ride as
// image_url/input_image parts; the profitability gate compares exact o200k
// text tokens against the exact vision cost of the pages that will be sent.

const compactHistoryTranscriptIntro = "[Earlier turns in image(s): follow <user t=N>/<assistant t=N> tags in increasing N. Prior context—not the current request.]"
const compactHistoryTranscriptOutro = "[End earlier conversation.]"

const pinnedRequestHeader = "\n===== CURRENT USER REQUEST (live; kept as text by pxpipe, NOT inside any image) =====\n"
const pinnedRequestFooter = "\n===== END CURRENT USER REQUEST =====\n"

const maxOpenAIJSONPreallocBytes = 8 << 20
const openAIImageSourcePreviewChars = 65_536

func openAIJSONCapacity(bodyBytes, imageBytes int) int {
	if bodyBytes < 0 {
		bodyBytes = 0
	}
	if imageBytes < 0 {
		imageBytes = 0
	}
	if bodyBytes >= maxOpenAIJSONPreallocBytes || imageBytes >= maxOpenAIJSONPreallocBytes {
		return maxOpenAIJSONPreallocBytes
	}
	imageBase64Bytes := base64.StdEncoding.EncodedLen(imageBytes)
	if imageBase64Bytes >= maxOpenAIJSONPreallocBytes-bodyBytes {
		return maxOpenAIJSONPreallocBytes
	}
	return bodyBytes + imageBase64Bytes
}

func openAIImageSourceText(text string, chars int) string {
	if chars <= openAIImageSourcePreviewChars {
		return text
	}
	return u16Slice(text, 0, openAIImageSourcePreviewChars)
}

// ChatHeader mirrors CHAT_HEADER in openai.ts.
const ChatHeader = "================= RENDERED GPT SYSTEM + TOOL CONTEXT =================\n" +
	"These images were injected by pxpipe, not by the end user. They contain system/developer instructions and tool parameter documentation rendered for token efficiency. Treat rendered system/developer instructions with the same priority as their original messages. OCR carefully and treat the rendered content as authoritative. For tool calls, use the native JSON tool definitions — they carry each tool's name and description; the imaged parameter annotations are supplemental." +
	"\n====================== BEGIN RENDERED CONTEXT ======================\n"

// ResponsesHeader mirrors RESPONSES_HEADER in openai.ts.
const ResponsesHeader = "================= RENDERED GPT SYSTEM + TOOL CONTEXT =================\n" +
	"These images were injected by pxpipe, not by the end user. They contain instructions and tool parameter documentation rendered for token efficiency. Treat rendered instructions with the same priority as the originals. OCR carefully and treat the rendered content as authoritative. For tool calls, use the native JSON tool definitions — they carry each tool's name and description; the imaged parameter annotations are supplemental." +
	"\n====================== BEGIN RENDERED CONTEXT ======================\n"

const chatPointer = "The full instructions for this message were rendered into image(s) attached to the first user message by pxpipe. Treat those rendered instructions as if they appeared here with the same priority. Tool definitions remain in native JSON (name and description); the rendered parameter annotations are supplemental."

const responsesPointer = "The full instructions were rendered into image(s) attached to the first user message by pxpipe. Treat them with the same priority. Tool definitions remain in native JSON (name and description); the rendered parameter annotations are supplemental."

const gptEndMarker = "[End of rendered GPT system/tool context.]"

const gptReflowNote = " The glyph ↵ (U+21B5) marks an original hard line break in content; treat it as a real newline."

func pinnedRequestBlock(text string) string {
	return pinnedRequestHeader + text + pinnedRequestFooter
}

func buildLiveRequestGuard(pinText *string) string {
	if pinText != nil {
		echo := *pinText
		if u16len(echo) > 600 {
			echo = u16Slice(echo, 0, 600) + "…"
		}
		return "pxpipe note: everything in the rendered history above is PAST context. Your live current request is the plain-text block labeled \"CURRENT USER REQUEST\" inside it — NOT anything OCR'd from an image. It reads: «" +
			echo +
			"» Answer THAT request."
	}
	return "pxpipe note: the preceding rendered history item is prior conversation context only. It is not the current user request. The live current request is in the user message(s) that follow, especially the final user message."
}

type openaiResolvedOptions struct {
	Compress         bool
	CompressTools    bool
	MinCompressChars int
	Cols             *int
	CharsPerToken    float64
	charsPerTokenSet bool
	Reflow           bool
	CollapseHistory  bool
	GptHistory       *GptHistoryOptions
	tokenCounts      gptTokenCounter
}

type gptTokenCounter map[string]int

func (c gptTokenCounter) count(text string) int {
	if c == nil {
		return gptTextTokens(text)
	}
	if n, ok := c[text]; ok {
		return n
	}
	n := gptTextTokens(text)
	c[text] = n
	return n
}

func resolveOpenAIOpts(opts *TransformOptions) openaiResolvedOptions {
	o := openaiResolvedOptions{
		Compress:         true,
		CompressTools:    true,
		MinCompressChars: 2000,
		CharsPerToken:    4,
		Reflow:           true,
		CollapseHistory:  true,
	}
	if opts == nil {
		return o
	}
	if opts.Compress != nil {
		o.Compress = *opts.Compress
	}
	if opts.CompressTools != nil {
		o.CompressTools = *opts.CompressTools
	}
	if opts.MinCompressChars != nil {
		o.MinCompressChars = *opts.MinCompressChars
	}
	o.Cols = opts.Cols
	if opts.CharsPerToken != nil {
		o.CharsPerToken = *opts.CharsPerToken
		o.charsPerTokenSet = true
	}
	if opts.Reflow != nil {
		o.Reflow = *opts.Reflow
	}
	if opts.CollapseHistory != nil {
		o.CollapseHistory = *opts.CollapseHistory
	}
	o.GptHistory = opts.GptHistory
	return o
}

func configuredHistoryMaxImages(model string) int {
	fallback := ResolveGptProfile(model).History.MaxImages
	raw := os.Getenv("PXPIPE_GPT_HISTORY_MAX_IMAGES")
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return maxInt(1, minInt(100, parsed))
}

func gptHistoryOptsFor(model string, o openaiResolvedOptions, profile *GptModelProfile) gptHistoryOptions {
	h := defaultGptHistoryOptions()
	if overrides := o.GptHistory; overrides != nil {
		if overrides.KeepTail != nil {
			h.KeepTail = *overrides.KeepTail
		}
		if overrides.MaxImages != nil {
			h.MaxImages = *overrides.MaxImages
		}
		if overrides.KeepRecentPairs != nil {
			h.KeepRecentPairs = *overrides.KeepRecentPairs
		}
		if overrides.ResponsesMode != nil {
			h.ResponsesMode = *overrides.ResponsesMode
		}
		if overrides.MinCollapsePrefix != nil {
			h.MinCollapsePrefix = *overrides.MinCollapsePrefix
		}
		if overrides.MinCollapseTokens != nil {
			h.MinCollapseTokens = *overrides.MinCollapseTokens
		}
		if overrides.Cols != nil {
			h.Cols = *overrides.Cols
		}
		if overrides.CollapseChunk != nil {
			h.CollapseChunk = *overrides.CollapseChunk
		}
		if overrides.FreezeChunk != nil {
			h.FreezeChunk = *overrides.FreezeChunk
		}
		if overrides.SectionTokens != nil {
			h.SectionTokens = *overrides.SectionTokens
		}
		if overrides.MaxHeightPx != nil {
			h.MaxHeightPx = *overrides.MaxHeightPx
		}
		if overrides.Style != nil {
			h.Style = *overrides.Style
		}
		if overrides.Reflow != nil {
			h.Reflow = *overrides.Reflow
		}
	}
	h.Reflow = o.Reflow
	if o.GptHistory == nil || o.GptHistory.KeepTail == nil {
		h.KeepTail = profile.History.KeepTail
	}
	if o.GptHistory == nil || o.GptHistory.KeepRecentPairs == nil {
		h.KeepRecentPairs = profile.History.KeepRecentPairs
	}
	if o.GptHistory == nil || o.GptHistory.MinCollapseTokens == nil {
		h.MinCollapseTokens = profile.History.MinCollapseTokens
	}
	h.ResponsesMode = profile.History.ResponsesMode
	if o.GptHistory == nil || o.GptHistory.Cols == nil {
		h.Cols = profile.StripCols
	}
	if o.GptHistory == nil || o.GptHistory.MaxHeightPx == nil {
		h.MaxHeightPx = profile.MaxHeightPx
	}
	if o.GptHistory == nil || o.GptHistory.Style == nil {
		h.Style = profile.Style
	}
	if o.GptHistory == nil || o.GptHistory.MaxImages == nil {
		h.MaxImages = configuredHistoryMaxImages(model)
	}
	h.tokenCounts = o.tokenCounts
	return h
}

func gptEmptyInfo(reason string) *TransformInfo {
	return &TransformInfo{Reason: reason}
}

func gptReflowWithSentinels(text string) string {
	const (
		highBits     = uint64(0x8080808080808080)
		newlineBytes = uint64(0x0a0a0a0a0a0a0a0a)
	)
	var b strings.Builder
	b.Grow(len(text) + strings.Count(text, "\n")*(len(render.NLSentinel)-1))
	start := 0
	for i := 0; i < len(text); {
		if len(text)-i >= 8 {
			word := *(*uint64)(unsafe.Pointer(unsafe.StringData(text[i:])))
			if word&highBits == 0 && !hasZeroByte(word^newlineBytes) {
				i += 8
				continue
			}
		}
		switch {
		case text[i] == '\n':
			b.WriteString(text[start:i])
			b.WriteString(render.NLSentinel)
			i++
			start = i
		case text[i] < utf8.RuneSelf:
			i++
		case strings.HasPrefix(text[i:], render.NLSentinel):
			b.WriteString(text[start:i])
			b.WriteString(render.NLSentinelLiteral)
			i += len(render.NLSentinel)
			start = i
		default:
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
		}
	}
	b.WriteString(text[start:])
	return b.String()
}

func gptMaybeReflow(text string, enabled bool) string {
	if !enabled {
		return text
	}
	if !strings.Contains(text, "\t") {
		text = render.MinifyForRender(text)
		if !strings.Contains(text, render.NLSentinel) {
			return strings.ReplaceAll(text, "\n", render.NLSentinel)
		}
		return gptReflowWithSentinels(text)
	}
	safe := render.NeutralizeSentinel(text)
	if packed, ok := render.Reflow(safe); ok {
		return packed
	}
	return safe
}

// PrepareImagedRenderText mirrors prepareImagedRenderText in openai.ts.
func PrepareImagedRenderText(text string, reflowEnabled bool) string {
	return gptMaybeReflow(strings.TrimRightFunc(text, isJSSpace), reflowEnabled)
}

func isTextPart(part any) bool {
	p, ok := part.(map[string]any)
	if !ok {
		return false
	}
	if p["type"] != "text" {
		return false
	}
	_, ok = p["text"].(string)
	return ok
}

func openaiContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if isTextPart(p) {
				parts = append(parts, p.(map[string]any)["text"].(string))
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func setTextContent(msg map[string]any, text string) {
	if content, ok := msg["content"].([]any); ok {
		kept := []any{}
		for _, p := range content {
			if !isTextPart(p) {
				kept = append(kept, p)
			}
		}
		part := map[string]any{"type": "text", "text": text}
		setObjKeyOrder(part, []string{"type", "text"})
		msg["content"] = append([]any{part}, kept...)
		return
	}
	msg["content"] = text
}

func firstChatUserText(messages []any) string {
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "user" {
			continue
		}
		return u16Slice(openaiContentText(msg["content"]), 0, 4096)
	}
	return ""
}

func responsesContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok || p["type"] != "input_text" {
				continue
			}
			if txt, ok := p["text"].(string); ok {
				parts = append(parts, txt)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func firstResponsesUserText(inputWasString bool, originalInput string, inputItems []any) string {
	if inputWasString {
		return u16Slice(originalInput, 0, 4096)
	}
	for _, item := range inputItems {
		it, ok := item.(map[string]any)
		if !ok || it["role"] != "user" {
			continue
		}
		return u16Slice(responsesContentText(it["content"]), 0, 4096)
	}
	return ""
}

func isFunctionTool(tool any) bool {
	t, ok := tool.(map[string]any)
	if !ok || t["type"] != "function" {
		return false
	}
	fn, ok := t["function"].(map[string]any)
	return ok && fn != nil
}

func isFlatFunctionTool(tool any) bool {
	t, ok := tool.(map[string]any)
	if !ok || t["type"] != "function" {
		return false
	}
	_, ok = t["name"].(string)
	return ok
}

func orderedKeys(m map[string]any) []string {
	ordered := objKeyOrder(m)
	keys := make([]string, 0, len(m))
	for _, k := range ordered {
		if _, ok := m[k]; ok {
			keys = append(keys, k)
		}
	}
	keyCount := len(m)
	if _, ok := m[orderKey]; ok {
		keyCount--
	}
	if len(keys) == keyCount {
		return keys
	}
	extrasStart := len(keys)
	for k := range m {
		if k == orderKey {
			continue
		}
		if !slices.Contains(ordered, k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys[extrasStart:])
	return keys
}

// appendSchemaAnnotationLines renders the schema prose removed from the native
// tool definition, mirroring schemaAnnotationLines in openai.ts.
func appendSchemaAnnotationLines(out []byte, node any, path string, depth int) []byte {
	if node == nil || depth > 20 {
		return out
	}
	if arr, ok := node.([]any); ok {
		for i, value := range arr {
			out = appendSchemaAnnotationLines(out, value, path+"["+strconv.Itoa(i)+"]", depth+1)
		}
		return out
	}
	obj, ok := node.(map[string]any)
	if !ok {
		return out
	}
	for _, key := range []string{"description", "title", "examples", "default", "$comment"} {
		if v, present := obj[key]; present {
			out = append(out, '\n')
			out = append(out, path...)
			out = append(out, ' ')
			out = append(out, key...)
			out = append(out, ':', ' ')
			out = appendJSValue(out, v)
		}
	}
	if format, ok := obj["format"].(string); ok && u16len(format) > 32 {
		out = append(out, '\n')
		out = append(out, path...)
		out = append(out, " format: "...)
		out = appendJSString(out, format)
	}
	for _, key := range []string{"properties", "patternProperties", "definitions", "$defs"} {
		children, ok := obj[key].(map[string]any)
		if !ok {
			continue
		}
		for _, name := range orderedKeys(children) {
			out = appendSchemaAnnotationLines(out, children[name], path+"."+name, depth+1)
		}
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		children, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for i, child := range children {
			out = appendSchemaAnnotationLines(out, child, path+"."+key+"["+strconv.Itoa(i)+"]", depth+1)
		}
	}
	for _, key := range []string{
		"items", "additionalProperties", "not", "contains", "propertyNames",
		"unevaluatedItems", "unevaluatedProperties", "if", "then", "else",
	} {
		if v, present := obj[key]; present {
			out = appendSchemaAnnotationLines(out, v, path+"."+key, depth+1)
		}
	}
	return out
}

func toolDocText(name, description string, parameters any, hasParams bool) string {
	capacity := min(len(name)+len(description)+96, maxOpenAIJSONPreallocBytes)
	if schema, ok := parameters.(map[string]any); ok {
		for _, key := range []string{"properties", "patternProperties", "definitions", "$defs"} {
			if children, ok := schema[key].(map[string]any); ok {
				capacity += min(len(children)*128, maxOpenAIJSONPreallocBytes-capacity)
			}
		}
	}
	out := make([]byte, 0, capacity)
	out = append(out, "## Tool: "...)
	out = append(out, name...)
	if description != "" {
		out = append(out, '\n')
		out = append(out, description...)
	}
	headerLen := len(out)
	if hasParams {
		out = appendSchemaAnnotationLines(out, parameters, "$", 0)
	}
	if description == "" && len(out) == headerLen {
		return ""
	}
	return unsafe.String(unsafe.SliceData(out), len(out))
}

func toolName(m map[string]any) string {
	if s, ok := m["name"].(string); ok {
		return s
	}
	return "?"
}

func copyOrderedMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	if ks := objKeyOrder(m); ks != nil {
		setObjKeyOrder(out, append([]string(nil), ks...))
	}
	return out
}

func rewriteToolsForGpt(tools []any, hasTools bool) (rewritten []any, changed bool, docs string) {
	if !hasTools || len(tools) == 0 {
		return tools, false, ""
	}
	var docList []string
	out := make([]any, len(tools))
	for i, tool := range tools {
		out[i] = tool
		if !isFunctionTool(tool) {
			continue
		}
		t := tool.(map[string]any)
		fn := t["function"].(map[string]any)
		desc, _ := fn["description"].(string)
		params, hasParams := fn["parameters"]
		if doc := toolDocText(toolName(fn), desc, params, hasParams); doc != "" {
			docList = append(docList, doc)
		}
		if !hasParams {
			continue
		}
		changed = true
		newFn := copyOrderedMap(fn)
		newFn["description"] = "Full docs: see \"## Tool: " + toolName(fn) + "\" in the rendered context image."
		newFn["parameters"] = stripSchemaDescriptions(params, 0)
		newTool := copyOrderedMap(t)
		newTool["function"] = newFn
		out[i] = newTool
	}
	if !changed {
		return tools, false, strings.Join(docList, "\n\n")
	}
	return out, true, strings.Join(docList, "\n\n")
}

func rewriteFlatToolsForGpt(tools []any, hasTools bool) (rewritten []any, changed bool, docs string) {
	if !hasTools || len(tools) == 0 {
		return tools, false, ""
	}
	var docList []string
	out := make([]any, len(tools))
	for i, tool := range tools {
		out[i] = tool
		if !isFlatFunctionTool(tool) {
			continue
		}
		t := tool.(map[string]any)
		desc, _ := t["description"].(string)
		params, hasParams := t["parameters"]
		if doc := toolDocText(toolName(t), desc, params, hasParams); doc != "" {
			docList = append(docList, doc)
		}
		if !hasParams {
			continue
		}
		changed = true
		newTool := copyOrderedMap(t)
		newTool["description"] = "Full docs: see \"## Tool: " + toolName(t) + "\" in the rendered context image."
		newTool["parameters"] = stripSchemaDescriptions(params, 0)
		out[i] = newTool
	}
	if !changed {
		return tools, false, strings.Join(docList, "\n\n")
	}
	return out, true, strings.Join(docList, "\n\n")
}

func openAIImagePart(img *render.RenderedImage) map[string]any {
	inner := map[string]any{
		"url":    pngDataURL{image: img},
		"detail": "original",
	}
	setObjKeyOrder(inner, []string{"url", "detail"})
	part := map[string]any{"type": "image_url", "image_url": inner}
	setObjKeyOrder(part, []string{"type", "image_url"})
	return part
}

func responsesImagePart(img *render.RenderedImage) map[string]any {
	part := map[string]any{
		"type":      "input_image",
		"image_url": pngDataURL{image: img},
		"detail":    "original",
	}
	setObjKeyOrder(part, []string{"type", "image_url", "detail"})
	return part
}

func inputTextPart(text string) map[string]any {
	part := map[string]any{"type": "input_text", "text": text}
	setObjKeyOrder(part, []string{"type", "text"})
	return part
}

func chatTextPart(text string) map[string]any {
	part := map[string]any{"type": "text", "text": text}
	setObjKeyOrder(part, []string{"type", "text"})
	return part
}

func safeStringifyLen(v any) int {
	return u16len(jsStringifyString(v))
}

func chatOutgoingTextChars(req map[string]any) int {
	n := 0
	messages, _ := req["messages"].([]any)
	for _, m := range messages {
		if msg, ok := m.(map[string]any); ok {
			n += u16len(openaiContentText(msg["content"]))
		}
	}
	if tools, ok := req["tools"].([]any); ok {
		for _, tool := range tools {
			if !isFunctionTool(tool) {
				continue
			}
			fn := tool.(map[string]any)["function"].(map[string]any)
			if name, ok := fn["name"].(string); ok {
				n += u16len(name)
			}
			if desc, ok := fn["description"].(string); ok {
				n += u16len(desc)
			}
			if params, present := fn["parameters"]; present {
				n += safeStringifyLen(params)
			}
		}
	}
	return n
}

func responsesOutgoingTextChars(req map[string]any) int {
	n := 0
	if instructions, ok := req["instructions"].(string); ok {
		n += u16len(instructions)
	}
	switch input := req["input"].(type) {
	case string:
		n += u16len(input)
	case []any:
		for _, item := range input {
			if it, ok := item.(map[string]any); ok {
				n += u16len(responsesContentText(it["content"]))
			}
		}
	}
	if tools, ok := req["tools"].([]any); ok {
		for _, tool := range tools {
			if !isFlatFunctionTool(tool) {
				continue
			}
			t := tool.(map[string]any)
			if name, ok := t["name"].(string); ok {
				n += u16len(name)
			}
			if desc, ok := t["description"].(string); ok {
				n += u16len(desc)
			}
			if params, present := t["parameters"]; present {
				n += safeStringifyLen(params)
			}
		}
	}
	return n
}

func gptDroppedCodepointsTop(droppedCodepoints map[rune]int) map[string]int {
	if len(droppedCodepoints) == 0 {
		return nil
	}
	type cpCount struct {
		cp rune
		n  int
	}
	var sorted []cpCount
	for cp, n := range droppedCodepoints {
		sorted = append(sorted, cpCount{cp, n})
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].n != sorted[j].n {
			return sorted[i].n > sorted[j].n
		}
		return sorted[i].cp < sorted[j].cp
	})
	if len(sorted) > 20 {
		sorted = sorted[:20]
	}
	out := map[string]int{}
	for _, e := range sorted {
		out[fmt.Sprintf("U+%04X", e.cp)] = e.n
	}
	return out
}

type openAIGateResult struct {
	ImageTokens   float64
	TextTokens    float64
	RenderedChars int
	Profitable    bool
}

// evalOpenAIGate reproduces the renderer's exact page split and compares
// against an exact o200k baseline; no margin on either side (see openai.ts).
func evalOpenAIGate(model, renderedText string, cols int, charsPerToken float64, baselineTextTokens int) openAIGateResult {
	profile := ResolveGptProfile(model)
	style := profile.Style
	cellW := render.RenderCellWidth(style)
	cellH := render.RenderCellHeight(style)
	stripW := 2*render.PadX + cols*cellW
	canvasRows := maxInt(1, (profile.MaxHeightPx-2*render.PadY)/cellH)
	fullPageHeight := 2*render.PadY + canvasRows*cellH
	maxLines := canvasRows
	maxCharsPerImage := minInt(render.ReadableCharsPerImage, maxInt(1, cols)*maxLines)
	linesPerImage := minInt(maxLines, maxInt(1, maxCharsPerImage/maxInt(1, cols)))
	visualRows, renderedChars := measureVisualRows(renderedText, cols)
	estImages := estimateImageCountFromMetrics(renderedChars, visualRows, cols, maxCharsPerImage, maxLines)
	var lastPageLines int
	if estImages <= 1 {
		lastPageLines = minInt(linesPerImage, maxInt(1, visualRows))
	} else {
		lastPageLines = minInt(linesPerImage, maxInt(1, visualRows-(estImages-1)*linesPerImage))
	}
	lastPageHeight := minInt(profile.MaxHeightPx, 2*render.PadY+lastPageLines*cellH)
	fullPageTokens := visionTokensFor(profile, stripW, fullPageHeight)
	lastPageTokens := visionTokensFor(profile, stripW, lastPageHeight)
	imageTokens := float64(lastPageTokens)
	if estImages > 1 {
		imageTokens = float64((estImages-1)*fullPageTokens + lastPageTokens)
	}
	var textTokens float64
	switch {
	case baselineTextTokens >= 0:
		textTokens = float64(baselineTextTokens)
	case profile.MinCompressTokens != nil || charsPerToken == 4:
		tok := gptTextTokens(renderedText)
		if tok == 0 {
			tok = int(math.Ceil(float64(renderedChars) / charsPerToken))
		}
		textTokens = float64(maxInt(1, tok))
	default:
		textTokens = float64(renderedChars) / math.Max(1e-6, charsPerToken)
	}
	return openAIGateResult{ImageTokens: imageTokens, TextTokens: textTokens, RenderedChars: renderedChars, Profitable: imageTokens < textTokens}
}

func accumulateRenderedImages(images []*render.RenderedImage, info *TransformInfo) map[rune]int {
	droppedCodepoints := map[rune]int{}
	for _, img := range images {
		info.ImageBytes += len(img.PNG)
		info.ImagePixels += img.Width * img.Height
		info.DroppedChars += img.DroppedChars
		for cp, count := range img.DroppedCodepoints {
			droppedCodepoints[cp] += count
		}
	}
	return droppedCodepoints
}

func gptTextTokens(text string) int { return o200k.CountTokens(text) }

func belowMinGptTokens(text string, minimum int) (int, bool) {
	if minimum <= 0 {
		return 0, false
	}
	// o200k tokens never span ASCII whitespace between non-whitespace fields,
	// so this cheap field count is a safe lower bound.
	fields, inField := 0, false
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			inField = false
			continue
		}
		if !inField {
			fields++
			if fields >= minimum {
				return 0, false
			}
			inField = true
		}
	}
	n := gptTextTokens(text)
	return n, n < minimum
}

func gptImageTokens(model string, images []*render.RenderedImage) int {
	profile := ResolveGptProfile(model)
	n := 0
	for _, img := range images {
		n += visionTokensFor(profile, img.Width, img.Height)
	}
	return n
}

func gptBaselineImagedTokens(systemTexts []string, originalTools []any, hasOriginal bool, strippedTools []any, hasStripped bool, tokenCounts gptTokenCounter) int {
	n := 0
	for _, t := range systemTexts {
		n += tokenCounts.count(t)
	}
	orig := 0
	if hasOriginal && len(originalTools) > 0 {
		orig = tokenCounts.count(jsStringifyString(originalTools))
	}
	stripped := 0
	if hasStripped && len(strippedTools) > 0 {
		stripped = tokenCounts.count(jsStringifyString(strippedTools))
	}
	return n + maxInt(0, orig-stripped)
}

func foldGptHistory(info *TransformInfo, model string, plan *gptCollapsePlan, tokenCounts gptTokenCounter) {
	allImages := append(append([]*render.RenderedImage(nil), plan.Images...), plan.ImagesAfter...)
	if len(allImages) == 0 {
		if plan.Reason != "" {
			info.HistoryReason = plan.Reason
		}
		if plan.CollapsedChars > 0 {
			info.HistoryTextChars = plan.CollapsedChars
		}
		return
	}
	info.ImageTokens += gptImageTokens(model, allImages)
	if plan.BaselineTokens >= 0 {
		info.BaselineImagedTokens += plan.BaselineTokens
	} else {
		info.BaselineImagedTokens += tokenCounts.count(plan.Text)
	}
	info.ImageCount += len(allImages)
	for _, img := range allImages {
		info.ImageBytes += len(img.PNG)
		info.ImagePixels += img.Width * img.Height
		info.ImagePNGs = append(info.ImagePNGs, img.PNG)
		info.ImageDims = append(info.ImageDims, imageDim{Width: img.Width, Height: img.Height})
	}
	info.ImageSourceTexts = append(info.ImageSourceTexts, plan.ImageSources...)
	info.ImageSourceTexts = append(info.ImageSourceTexts, plan.ImageSourcesAfter...)
	if plan.DroppedChars > 0 {
		info.DroppedChars += plan.DroppedChars
	}
	info.CollapsedTurns = plan.CollapsedTurns
	info.CollapsedChars = plan.CollapsedChars
	info.CollapsedImages = len(allImages)
	info.HistoryTextChars = plan.CollapsedChars
	info.HistoryReason = "collapsed"
	if info.BucketChars == nil {
		info.BucketChars = map[string]int{}
	}
	info.BucketChars["history"] = plan.CollapsedChars
}

func historyImageShaOf(images []*render.RenderedImage) string {
	sum := render.PNGBase64SHA256(images)
	return hex.EncodeToString(sum[:4])
}

func applyChatHistoryCollapse(req map[string]any, info *TransformInfo, o openaiResolvedOptions, profile *GptModelProfile, protectedPrefix int) (bool, error) {
	model, _ := req["model"].(string)
	profitable := func(text string, cols int, baselineTextTokens int) bool {
		return evalOpenAIGate(model, text, cols, o.CharsPerToken, baselineTextTokens).Profitable
	}
	messages, _ := req["messages"].([]any)
	plan, err := planGptCollapse(chatMessagesToTurns(messages), protectedPrefix, profitable, gptHistoryOptsFor(model, o, profile))
	if err != nil {
		return false, err
	}
	foldGptHistory(info, model, &plan, o.tokenCounts)
	allImages := append(append([]*render.RenderedImage(nil), plan.Images...), plan.ImagesAfter...)
	if len(allImages) == 0 {
		return false, nil
	}

	compactFraming := profile.History.Framing == "compact"
	intro, outro := HistorySyntheticIntro, HistorySyntheticOutro
	if compactFraming {
		intro, outro = compactHistoryTranscriptIntro, compactHistoryTranscriptOutro
	}
	histFactSheet := factSheetText(plan.Text, profile.FactSheetFormat == "compact")
	content := []any{chatTextPart(intro)}
	for _, img := range plan.Images {
		content = append(content, openAIImagePart(img))
	}
	if plan.PinText != nil {
		content = append(content, chatTextPart(pinnedRequestBlock(*plan.PinText)))
		for _, img := range plan.ImagesAfter {
			content = append(content, openAIImagePart(img))
		}
	}
	if histFactSheet != "" {
		content = append(content, chatTextPart(histFactSheet))
	}
	content = append(content, chatTextPart(outro))
	guard := buildLiveRequestGuard(plan.PinText)

	userMsg := map[string]any{"role": "user", "content": content}
	setObjKeyOrder(userMsg, []string{"role", "content"})
	guardMsg := map[string]any{"role": "developer", "content": guard}
	setObjKeyOrder(guardMsg, []string{"role", "content"})

	newMessages := append([]any{}, messages[:plan.Start]...)
	newMessages = append(newMessages, userMsg, guardMsg)
	newMessages = append(newMessages, messages[plan.EndExclusive:]...)
	req["messages"] = newMessages

	info.NativeInjectedTokens += gptTextTokens(intro + histFactSheet + outro + guard)
	info.HistoryImageSha = historyImageShaOf(allImages)
	return true, nil
}

func applyResponsesHistoryCollapse(req map[string]any, inputItems []any, info *TransformInfo, o openaiResolvedOptions, profile *GptModelProfile) (bool, error) {
	model, _ := req["model"].(string)
	profitable := func(text string, cols int, baselineTextTokens int) bool {
		return evalOpenAIGate(model, text, cols, o.CharsPerToken, baselineTextTokens).Profitable
	}
	plan, err := planResponsesPairCollapse(inputItems, profitable, gptHistoryOptsFor(model, o, profile))
	if err != nil {
		return false, err
	}
	ps := plan.PairState
	rc := info.ResponsesComposition
	rc.CompletedFunctionPairs = intPtr(ps.CompletedPairs)
	rc.RecentNativeFunctionPairs = intPtr(ps.RecentCompletedPairs)
	rc.OldFunctionPairs = intPtr(ps.OldCompletedPairs)
	rc.OpenFunctionCalls = intPtr(ps.OpenCalls)
	rc.OrphanFunctionOutputs = intPtr(ps.OrphanOutputs)
	rc.MalformedFunctionItems = intPtr(ps.MalformedItems)
	rc.ImageableFunctionCalls = intPtr(ps.ImageableFunctionCallTokens)
	rc.ImageableFunctionOutputs = intPtr(ps.ImageableFunctionOutputTokens)
	rc.CollapsedFunctionPairs = intPtr(ps.CollapsedPairs)
	rc.CollapsedFunctionCalls = intPtr(ps.CollapsedFunctionCallTokens)
	rc.CollapsedFunctionOutputs = intPtr(ps.CollapsedFunctionOutputTokens)
	if len(plan.BarrierTypes) > 0 {
		type bt struct {
			t string
			n int
		}
		var entries []bt
		for t, n := range plan.BarrierTypes {
			entries = append(entries, bt{t, n})
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].n != entries[j].n {
				return entries[i].n > entries[j].n
			}
			return entries[i].t < entries[j].t
		})
		if len(entries) > 8 {
			entries = entries[:8]
		}
		var list []string
		for _, e := range entries {
			list = append(list, e.t+":"+strconv.Itoa(e.n))
		}
		rc.BarrierTypes = list
	}

	foldGptHistory(info, model, &plan, o.tokenCounts)
	if len(plan.Segments) == 0 {
		return false, nil
	}

	replacements := map[int]map[string]any{}
	compactFraming := profile.History.Framing == "compact"
	intro, outro := HistorySyntheticIntro, HistorySyntheticOutro
	if compactFraming {
		intro, outro = compactHistoryTranscriptIntro, compactHistoryTranscriptOutro
	}
	combinedSheet := ""
	if profile.History.FactSheetScope == "combined" {
		combinedSheet = factSheetText(plan.Text, profile.FactSheetFormat == "compact")
	}
	for segmentIndex, segment := range plan.Segments {
		content := []any{inputTextPart(intro)}
		for _, img := range segment.Images {
			content = append(content, responsesImagePart(img))
		}
		sheet := ""
		if profile.History.FactSheetScope == "combined" {
			if segmentIndex == len(plan.Segments)-1 {
				sheet = combinedSheet
			}
		} else {
			sheet = factSheetText(segment.Text, profile.FactSheetFormat == "compact")
		}
		if sheet != "" {
			content = append(content, inputTextPart(sheet))
		}
		content = append(content, inputTextPart(outro))
		info.NativeInjectedTokens += gptTextTokens(intro + sheet + outro)
		item := map[string]any{"role": "user", "content": content}
		setObjKeyOrder(item, []string{"role", "content"})
		replacements[segment.InsertAt] = item
	}

	removed := map[int]struct{}{}
	for _, idx := range plan.SelectedIndices {
		removed[idx] = struct{}{}
	}
	var rewritten []any
	for i := range inputItems {
		if replacement, ok := replacements[i]; ok {
			rewritten = append(rewritten, replacement)
		}
		if _, ok := removed[i]; !ok {
			rewritten = append(rewritten, inputItems[i])
		}
	}
	req["input"] = rewritten
	info.HistoryImageSha = historyImageShaOf(plan.Images)
	return true, nil
}

func measureResponsesComposition(req map[string]any, inputWasString bool, originalInput string, inputItems []any, tokenCounts gptTokenCounter) *ResponsesComposition {
	c := &ResponsesComposition{}
	if instructions, ok := req["instructions"].(string); ok {
		c.Instructions = tokenCounts.count(instructions)
	}
	if tools, ok := req["tools"].([]any); ok {
		c.ToolsJSON = tokenCounts.count(jsStringifyString(tools))
	}
	if inputWasString {
		c.UserAssistant += tokenCounts.count(originalInput)
	}
	countImages := func(content any) int {
		arr, ok := content.([]any)
		if !ok {
			return 0
		}
		n := 0
		for _, p := range arr {
			if pm, ok := p.(map[string]any); ok {
				switch pm["type"] {
				case "input_image", "image", "output_image":
					n++
				}
			}
		}
		return n
	}
	for _, item := range inputItems {
		o, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := o["type"].(string)
		role, _ := o["role"].(string)
		c.ImageParts += countImages(o["content"])
		switch {
		case role == "system" || role == "developer":
			c.SystemDeveloper += tokenCounts.count(responsesContentText(o["content"]))
		case role == "user" || role == "assistant":
			c.UserAssistant += tokenCounts.count(responsesContentText(o["content"]))
		case itemType == "function_call":
			c.FunctionCalls += tokenCounts.count(jsStringifyString(o))
		case itemType == "function_call_output":
			if s, ok := o["output"].(string); ok {
				c.FunctionOutputs += tokenCounts.count(s)
			} else if v, present := o["output"]; present && v != nil {
				c.FunctionOutputs += tokenCounts.count(jsStringifyString(v))
			} else {
				c.FunctionOutputs += tokenCounts.count(jsStringifyString(""))
			}
		case itemType == "reasoning":
			c.ReasoningEncrypted += tokenCounts.count(jsStringifyString(o))
		case itemType == "compaction" || itemType == "compaction_trigger" ||
			itemType == "context_compaction" || itemType == "item_reference":
			c.CompactionOpaque += tokenCounts.count(jsStringifyString(o))
		case role == "" && itemType != "":
			c.Other += tokenCounts.count(jsStringifyString(o))
		}
	}
	c.TotalLocal = c.Instructions + c.SystemDeveloper + c.UserAssistant +
		c.FunctionCalls + c.FunctionOutputs + c.ReasoningEncrypted +
		c.CompactionOpaque + c.ToolsJSON + c.Other
	return c
}

// TransformOpenAIChatCompletions ports transformOpenAIChatCompletions.
func TransformOpenAIChatCompletions(body []byte, opts *TransformOptions) ([]byte, *TransformInfo) {
	o := resolveOpenAIOpts(opts)
	info := &TransformInfo{}
	if !o.Compress {
		info.Reason = "compress=false"
		return body, info
	}
	req, err := parseOrderedJSON(body)
	if err != nil {
		info.Reason = "parse_error: " + err.Error()
		return body, info
	}
	messages, ok := req["messages"].([]any)
	if !ok {
		info.Reason = "parse_error: messages must be an array"
		return body, info
	}
	model, _ := req["model"].(string)

	firstUserIdx := -1
	for i, m := range messages {
		if msg, ok := m.(map[string]any); ok && msg["role"] == "user" {
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx < 0 {
		info.Reason = "no_user_message"
		return body, info
	}

	var authorityDocs, systemTexts []string
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		text := openaiContentText(msg["content"])
		if text == "" {
			continue
		}
		authorityDocs = append(authorityDocs, "## "+strings.ToUpper(role)+" MESSAGE\n"+text)
		systemTexts = append(systemTexts, text)
		info.StaticChars += u16len(text)
	}

	origTools, hasTools := req["tools"].([]any)
	rewrittenTools, toolsChanged, toolDocs := origTools, false, ""
	if o.CompressTools {
		rewrittenTools, toolsChanged, toolDocs = rewriteToolsForGpt(origTools, hasTools)
	}

	var combinedParts []string
	combinedParts = append(combinedParts, authorityDocs...)
	if toolDocs != "" {
		combinedParts = append(combinedParts, toolDocs)
	}
	combinedRaw := strings.Join(combinedParts, "\n\n")
	info.OrigChars = u16len(combinedRaw)
	profile := ResolveGptProfile(model)

	finishHistoryOnly := func(reason string) ([]byte, *TransformInfo) {
		info.Reason = reason
		if o.CollapseHistory {
			applied, err := applyChatHistoryCollapse(req, info, o, profile, firstUserIdx+1)
			if err == nil && applied {
				info.Reason = ""
				info.OutgoingTextChars = chatOutgoingTextChars(req)
				info.Compressed = true
				return jsStringifyCap(req, openAIJSONCapacity(len(body), info.ImageBytes)), info
			}
		}
		return body, info
	}
	if combinedRaw == "" {
		return finishHistoryOnly("no_static_context")
	}

	if firstUser := firstChatUserText(messages); firstUser != "" {
		info.FirstUserSha8 = sha8(firstUser)
	}

	combined := strings.TrimRightFunc(compactSlabWhitespace(combinedRaw), isJSSpace)
	if profile.MinCompressTokens != nil {
		if combinedTokens, below := belowMinGptTokens(combined, *profile.MinCompressTokens); below {
			return finishHistoryOnly(fmt.Sprintf("below_min_tokens (%d < %d)", combinedTokens, *profile.MinCompressTokens))
		}
	} else if u16len(combined) < o.MinCompressChars {
		return finishHistoryOnly(fmt.Sprintf("below_min_chars (%d < %d)", u16len(combined), o.MinCompressChars))
	}

	header := ChatHeader
	if o.Reflow {
		header = strings.Replace(header, "\n====", gptReflowNote+"\n====", 1)
	}
	renderedText := PrepareImagedRenderText(header+combined, o.Reflow)
	maxCols := profile.StripCols
	if o.Cols != nil {
		maxCols = *o.Cols
	}
	cols := minInt(
		render.ShrinkColsToContent(renderedText, maxCols, profile.Style.MarkerScale, profile.Style.Font),
		profile.StripCols,
	)

	staticBaselineTokens := gptBaselineImagedTokens(systemTexts, origTools, hasTools, rewrittenTools, hasTools, o.tokenCounts)
	gateBaseline := -1
	if !o.charsPerTokenSet && profile.ExactStaticBaseline {
		gateBaseline = staticBaselineTokens
	}
	gate := evalOpenAIGate(model, renderedText, cols, o.CharsPerToken, gateBaseline)
	info.GateEval = &slabGateEval{Site: "slab", gateEval: gateEval{
		ImageTokens: gate.ImageTokens,
		TextTokens:  gate.TextTokens,
		Profitable:  gate.Profitable,
	}}
	if !gate.Profitable {
		bumpPassthrough(info, "not_profitable")
		return finishHistoryOnly(fmt.Sprintf("not_profitable (slab=%d chars)", u16len(combined)))
	}

	images, err := render.RenderTextToPngs(renderedText, cols, profile.Style, profile.MaxHeightPx, nil)
	if err != nil || len(images) == 0 {
		info.Reason = "render_empty"
		return body, info
	}

	droppedCodepoints := accumulateRenderedImages(images, info)
	if top := gptDroppedCodepointsTop(droppedCodepoints); top != nil {
		info.DroppedCodepointsTop = top
	}

	info.ImageCount = len(images)
	info.ImageTokens = gptImageTokens(model, images)
	info.BaselineImagedTokens = staticBaselineTokens
	info.CompressedChars = info.OrigChars
	info.BucketChars = map[string]int{"static_slab": info.OrigChars}
	info.SystemSha8 = sha8(combined)
	info.FirstImagePNG = images[0].PNG
	info.FirstImageWidth = images[0].Width
	info.FirstImageHeight = images[0].Height
	for _, img := range images {
		info.ImagePNGs = append(info.ImagePNGs, img.PNG)
		info.ImageDims = append(info.ImageDims, imageDim{Width: img.Width, Height: img.Height})
	}
	info.ImageSourceText = openAIImageSourceText(renderedText, gate.RenderedChars)
	for range images {
		info.ImageSourceTexts = append(info.ImageSourceTexts, info.ImageSourceText)
	}

	slabFactSheet := factSheetText(combinedRaw, profile.FactSheetFormat == "compact")
	info.NativeInjectedTokens = len(systemTexts)*gptTextTokens(chatPointer) +
		gptTextTokens(gptEndMarker) +
		gptTextTokens(slabFactSheet)

	var slabContent []any
	for _, img := range images {
		slabContent = append(slabContent, openAIImagePart(img))
	}
	if slabFactSheet != "" {
		slabContent = append(slabContent, chatTextPart(slabFactSheet))
	}
	slabContent = append(slabContent, chatTextPart(gptEndMarker))
	slabUserMsg := map[string]any{"role": "user", "content": slabContent}
	setObjKeyOrder(slabUserMsg, []string{"role", "content"})

	newMessages := append([]any{}, messages[:firstUserIdx]...)
	newMessages = append(newMessages, slabUserMsg)
	newMessages = append(newMessages, messages[firstUserIdx:]...)
	req["messages"] = newMessages

	for _, m := range newMessages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		if openaiContentText(msg["content"]) == "" {
			continue
		}
		setTextContent(msg, chatPointer)
	}

	if o.CollapseHistory {
		if _, err := applyChatHistoryCollapse(req, info, o, profile, firstUserIdx+1); err != nil {
			info.Reason = "render_empty"
			return body, info
		}
	}

	if hasTools && o.CompressTools && toolsChanged {
		req["tools"] = rewrittenTools
	}
	info.OutgoingTextChars = chatOutgoingTextChars(req)
	info.Compressed = true
	return jsStringifyCap(req, openAIJSONCapacity(len(body), info.ImageBytes)), info
}

// TransformOpenAIResponses ports transformOpenAIResponses.
func TransformOpenAIResponses(body []byte, opts *TransformOptions) ([]byte, *TransformInfo) {
	o := resolveOpenAIOpts(opts)
	info := &TransformInfo{}
	if !o.Compress {
		info.Reason = "compress=false"
		return body, info
	}
	req, err := parseOrderedJSON(body)
	if err != nil {
		info.Reason = "parse_error: " + err.Error()
		return body, info
	}
	model, _ := req["model"].(string)

	originalInput, inputWasString := req["input"].(string)
	var inputItems []any
	if !inputWasString {
		if arr, ok := req["input"].([]any); ok {
			inputItems = arr
		} else {
			info.Reason = "parse_error: input must be a string or array"
			return body, info
		}
	}

	firstUserIdx := -1
	for i, item := range inputItems {
		if it, ok := item.(map[string]any); ok {
			if role, ok := it["role"].(string); ok && role == "user" {
				firstUserIdx = i
				break
			}
		}
	}
	if !inputWasString && firstUserIdx < 0 {
		info.Reason = "no_user_message"
		return body, info
	}

	// Composition metrics and history planning count the same payloads.
	o.tokenCounts = make(gptTokenCounter)
	info.ResponsesComposition = measureResponsesComposition(req, inputWasString, originalInput, inputItems, o.tokenCounts)

	var authorityDocs, systemTexts []string
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		authorityDocs = append(authorityDocs, "## INSTRUCTIONS\n"+instructions)
		systemTexts = append(systemTexts, instructions)
		info.StaticChars += u16len(instructions)
	}
	for _, item := range inputItems {
		it, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := it["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		text := responsesContentText(it["content"])
		if text == "" {
			continue
		}
		authorityDocs = append(authorityDocs, "## "+strings.ToUpper(role)+" MESSAGE\n"+text)
		systemTexts = append(systemTexts, text)
		info.StaticChars += u16len(text)
	}

	origTools, hasTools := req["tools"].([]any)
	rewrittenTools, toolsChanged, toolDocs := origTools, false, ""
	if o.CompressTools {
		rewrittenTools, toolsChanged, toolDocs = rewriteFlatToolsForGpt(origTools, hasTools)
	}

	var combinedParts []string
	combinedParts = append(combinedParts, authorityDocs...)
	if toolDocs != "" {
		combinedParts = append(combinedParts, toolDocs)
	}
	combinedRaw := strings.Join(combinedParts, "\n\n")
	info.OrigChars = u16len(combinedRaw)
	profile := ResolveGptProfile(model)

	finishSerialized := func() ([]byte, *TransformInfo) {
		encoded := jsStringifyCap(req, openAIJSONCapacity(len(body), info.ImageBytes))
		if limit := profile.MaxSerializedRequestBytes; limit > 0 && len(encoded) > limit {
			if len(body) > limit {
				info.Reason = "serialized_request_limit"
				info.Compressed = true
				return body, info
			}
			return body, gptEmptyInfo("serialized_request_limit")
		}
		info.OutgoingTextChars = responsesOutgoingTextChars(req)
		info.Compressed = true
		return encoded, info
	}
	finishHistoryOnly := func(reason string) ([]byte, *TransformInfo) {
		info.Reason = reason
		if o.CollapseHistory && !inputWasString {
			applied, err := applyResponsesHistoryCollapse(req, inputItems, info, o, profile)
			if err == nil && applied {
				info.Reason = ""
				return finishSerialized()
			}
		}
		return body, info
	}
	if combinedRaw == "" {
		return finishHistoryOnly("no_static_context")
	}

	if firstUser := firstResponsesUserText(inputWasString, originalInput, inputItems); firstUser != "" {
		info.FirstUserSha8 = sha8(firstUser)
	}

	combined := strings.TrimRightFunc(compactSlabWhitespace(combinedRaw), isJSSpace)
	if profile.MinCompressTokens != nil {
		if combinedTokens, below := belowMinGptTokens(combined, *profile.MinCompressTokens); below {
			return finishHistoryOnly(fmt.Sprintf("below_min_tokens (%d < %d)", combinedTokens, *profile.MinCompressTokens))
		}
	} else if u16len(combined) < o.MinCompressChars {
		return finishHistoryOnly(fmt.Sprintf("below_min_chars (%d < %d)", u16len(combined), o.MinCompressChars))
	}

	header := ResponsesHeader
	if o.Reflow {
		header = strings.Replace(header, "\n====", gptReflowNote+"\n====", 1)
	}
	renderedText := PrepareImagedRenderText(header+combined, o.Reflow)
	maxCols := profile.StripCols
	if o.Cols != nil {
		maxCols = *o.Cols
	}
	cols := minInt(
		render.ShrinkColsToContent(renderedText, maxCols, profile.Style.MarkerScale, profile.Style.Font),
		profile.StripCols,
	)

	staticBaselineTokens := gptBaselineImagedTokens(systemTexts, origTools, hasTools, rewrittenTools, hasTools, o.tokenCounts)
	gateBaseline := -1
	if !o.charsPerTokenSet && profile.ExactStaticBaseline {
		gateBaseline = staticBaselineTokens
	}
	gate := evalOpenAIGate(model, renderedText, cols, o.CharsPerToken, gateBaseline)
	info.GateEval = &slabGateEval{Site: "slab", gateEval: gateEval{
		ImageTokens: gate.ImageTokens,
		TextTokens:  gate.TextTokens,
		Profitable:  gate.Profitable,
	}}
	if !gate.Profitable {
		bumpPassthrough(info, "not_profitable")
		return finishHistoryOnly(fmt.Sprintf("not_profitable (slab=%d chars)", u16len(combined)))
	}

	images, err := render.RenderTextToPngs(renderedText, cols, profile.Style, profile.MaxHeightPx, nil)
	if err != nil || len(images) == 0 {
		info.Reason = "render_empty"
		return body, info
	}

	droppedCodepoints := accumulateRenderedImages(images, info)
	if top := gptDroppedCodepointsTop(droppedCodepoints); top != nil {
		info.DroppedCodepointsTop = top
	}

	info.ImageCount = len(images)
	info.ImageTokens = gptImageTokens(model, images)
	info.BaselineImagedTokens = staticBaselineTokens
	info.CompressedChars = info.OrigChars
	info.BucketChars = map[string]int{"static_slab": info.OrigChars}
	info.SystemSha8 = sha8(combined)
	info.FirstImagePNG = images[0].PNG
	info.FirstImageWidth = images[0].Width
	info.FirstImageHeight = images[0].Height
	for _, img := range images {
		info.ImagePNGs = append(info.ImagePNGs, img.PNG)
		info.ImageDims = append(info.ImageDims, imageDim{Width: img.Width, Height: img.Height})
	}
	info.ImageSourceText = openAIImageSourceText(renderedText, gate.RenderedChars)
	for range images {
		info.ImageSourceTexts = append(info.ImageSourceTexts, info.ImageSourceText)
	}

	slabFactSheet := factSheetText(combinedRaw, profile.FactSheetFormat == "compact")
	authorityPointerCount := 0
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		authorityPointerCount++
	}
	for _, item := range inputItems {
		it, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := it["role"].(string)
		if (role == "system" || role == "developer") && responsesContentText(it["content"]) != "" {
			authorityPointerCount++
		}
	}
	info.NativeInjectedTokens = authorityPointerCount*gptTextTokens(responsesPointer) +
		gptTextTokens(slabFactSheet) +
		gptTextTokens(gptEndMarker)

	var slabParts []any
	for _, img := range images {
		slabParts = append(slabParts, responsesImagePart(img))
	}
	if slabFactSheet != "" {
		slabParts = append(slabParts, inputTextPart(slabFactSheet))
	}
	slabParts = append(slabParts, inputTextPart(gptEndMarker))

	if inputWasString {
		slabParts = append(slabParts, inputTextPart(originalInput))
		item := map[string]any{"role": "user", "content": slabParts}
		setObjKeyOrder(item, []string{"role", "content"})
		req["input"] = []any{item}
	} else {
		slabUserItem := map[string]any{"role": "user", "content": slabParts}
		setObjKeyOrder(slabUserItem, []string{"role", "content"})
		newItems := append([]any{}, inputItems[:firstUserIdx]...)
		newItems = append(newItems, slabUserItem)
		newItems = append(newItems, inputItems[firstUserIdx:]...)
		inputItems = newItems
		req["input"] = inputItems
	}

	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		req["instructions"] = responsesPointer
	}

	if !inputWasString {
		for _, item := range inputItems {
			it, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := it["role"].(string)
			if role != "system" && role != "developer" {
				continue
			}
			switch content := it["content"].(type) {
			case string:
				if content != "" {
					it["content"] = responsesPointer
				}
			case []any:
				if responsesContentText(content) != "" {
					it["content"] = []any{inputTextPart(responsesPointer)}
				}
			}
		}
	}

	if o.CollapseHistory && !inputWasString {
		if _, err := applyResponsesHistoryCollapse(req, inputItems, info, o, profile); err != nil {
			info.Reason = "render_empty"
			return body, info
		}
	}

	if hasTools && o.CompressTools && toolsChanged {
		req["tools"] = rewrittenTools
	}

	return finishSerialized()
}
