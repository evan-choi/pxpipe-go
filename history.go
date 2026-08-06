package pxpipe

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/evan-choi/pxpipe-go/render"
)

// History-image compression (Variant C): collapses the largest closed
// tool-sequence prefix into one synthetic user message of PNG blocks; the live
// tail stays text. See the TS module docstring for the full design notes.

const HistorySyntheticIntro = `[Earlier turns of THIS conversation, transcribed in the image(s) below. Each turn is wrapped in <user t="N">...</user> or <assistant t="N">...</assistant> tags, where N is an absolute turn index (larger N = more recent); attribute every turn strictly by its tag, and treat the highest-N turns as the most recent prior context, NOT the low-N opening turns. Earlier turns may contain questions or tasks that were already answered later in this same history; do not reopen low-N turns unless the live text after this block asks you to. For exact identifiers, hashes, version strings, and numbers from the transcript, rely on the exact-value factsheet or re-read the source; do not guess an exact value seen only in the image. This is prior context, NOT the current request.]`

const HistorySyntheticOutro = `[End of earlier conversation. The current request is the live text that follows below.]`

const (
	latestCollapsedUserPreviewChars = 300
	latestCollapsedUserVerbatim     = 4000
	verbatimHeadChars               = 2600
	verbatimTailChars               = 1400
	userTextMaxChars                = 2000
	endOfRenderedContext            = "[End of rendered context.]"
)

type profitableFn func(text string, cols int) bool

type historyCollapseOptions struct {
	keepTail          int
	minCollapsePrefix int
	cols              int
	collapseChunk     int
	freezeChunk       int
	protectedPrefix   int
	reflow            bool
	style             render.RenderStyle
	maxHeightPx       int
}

func historyDefaults() historyCollapseOptions {
	return historyCollapseOptions{
		keepTail:          4,
		minCollapsePrefix: 10,
		cols:              100,
		collapseChunk:     50,
		freezeChunk:       10,
		protectedPrefix:   0,
		reflow:            true,
		style:             render.DenseRenderStyle,
		maxHeightPx:       render.MaxHeightPx,
	}
}

type historyCollapseInfo struct {
	collapsedTurns        int
	collapsedChars        int
	collapsedImages       int
	collapsedImageBytes   int
	collapsedImagePixels  int
	collapsedPngs         [][]byte
	collapsedImageDims    []imageDim
	carryOverImageOrdinal int
	hasCarryOver          bool
	reason                string
	droppedChars          int
	droppedCodepoints     map[rune]int
}

type imageDim struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func findClosedPrefixBoundary(messages []any, cutoffExclusive int) int {
	if cutoffExclusive <= 0 {
		return -1
	}
	openSet := map[string]struct{}{}
	lastClosed := -1
	inToolRound := false
	limit := cutoffExclusive
	if limit > len(messages) {
		limit = len(messages)
	}
	assistantToolUses := func(m map[string]any) []map[string]any {
		role, _ := getStr(m, "role")
		if role != "assistant" {
			return nil
		}
		arr, ok := asArr(m["content"])
		if !ok {
			return nil
		}
		var out []map[string]any
		for _, bv := range arr {
			if blockType(bv) == "tool_use" {
				if bm, ok := asMap(bv); ok {
					out = append(out, bm)
				}
			}
		}
		return out
	}
	for i := 0; i < limit; i++ {
		m, ok := asMap(messages[i])
		if !ok {
			continue
		}
		uses := assistantToolUses(m)
		if inToolRound && len(openSet) == 0 && len(uses) == 0 {
			lastClosed = i - 1
			inToolRound = false
		}
		if _, isArr := asArr(m["content"]); !isArr {
			if len(openSet) == 0 && !inToolRound {
				lastClosed = i
			}
			continue
		}
		role, _ := getStr(m, "role")
		if role == "assistant" {
			if len(uses) > 0 {
				inToolRound = true
			}
			for _, blk := range uses {
				if id, ok := getStr(blk, "id"); ok {
					openSet[id] = struct{}{}
				}
			}
		} else if role == "user" {
			arr, _ := asArr(m["content"])
			for _, bv := range arr {
				if blockType(bv) == "tool_result" {
					if bm, ok := asMap(bv); ok {
						if id, ok := getStr(bm, "tool_use_id"); ok {
							delete(openSet, id)
						}
					}
				}
			}
		}
		if len(openSet) == 0 && !inToolRound {
			lastClosed = i
		}
	}
	if inToolRound && len(openSet) == 0 {
		nextContinues := false
		if limit < len(messages) {
			if nm, ok := asMap(messages[limit]); ok {
				if role, _ := getStr(nm, "role"); role == "assistant" {
					if arr, ok := asArr(nm["content"]); ok {
						for _, bv := range arr {
							if blockType(bv) == "tool_use" {
								nextContinues = true
								break
							}
						}
					}
				}
			}
		}
		if !nextContinues {
			lastClosed = limit - 1
		}
	}
	return lastClosed
}

var freshnessHintRe = regexp.MustCompile(`\(file state is current in your\s+context — no need to Read it back\)`)

const staleFreshnessNote = "(state as of this PRIOR turn — the file may have changed since; Read it again before editing)"

func staleFreshnessHints(text string) string {
	return freshnessHintRe.ReplaceAllString(text, staleFreshnessNote)
}

func blocksToText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := asArr(content)
	if !ok {
		return ""
	}
	var parts []string
	for _, bv := range arr {
		blk, ok := asMap(bv)
		if !ok {
			continue
		}
		switch blockType(bv) {
		case "text":
			if t, ok := getStr(blk, "text"); ok {
				parts = append(parts, t)
			}
		case "tool_use":
			name, _ := getStr(blk, "name")
			parts = append(parts, "[tool_use "+name+"]\n"+jsStringifyString(blk["input"]))
		case "tool_result":
			inner := blk["content"]
			var innerText string
			if s, ok := inner.(string); ok {
				innerText = s
			} else if ia, ok := asArr(inner); ok {
				var sub []string
				for _, sv := range ia {
					sm, ok := asMap(sv)
					if !ok {
						continue
					}
					switch blockType(sv) {
					case "text":
						if t, ok := getStr(sm, "text"); ok {
							sub = append(sub, t)
						}
					case "image":
						sub = append(sub, "[image]")
					}
				}
				innerText = strings.Join(sub, "\n")
			}
			errMark := ""
			if isErr, ok := blk["is_error"].(bool); ok && isErr {
				errMark = " (error)"
			}
			parts = append(parts, "[tool_result"+errMark+"]\n"+staleFreshnessHints(innerText))
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n\n")
}

func messageCacheControl(m map[string]any) (any, bool) {
	arr, ok := asArr(m["content"])
	if !ok {
		return nil, false
	}
	for i := len(arr) - 1; i >= 0; i-- {
		if bm, ok := asMap(arr[i]); ok {
			if cc, has := bm["cache_control"]; has && cc != nil {
				return cc, true
			}
		}
	}
	return nil, false
}

func messagesToHistorySegments(messages []any, upToExclusive, fromInclusive int) (text, slotText string) {
	var textOut, slotOut []string
	for i := fromInclusive; i < upToExclusive; i++ {
		m, ok := asMap(messages[i])
		if !ok {
			continue
		}
		role, _ := getStr(m, "role")
		content := m["content"]
		if role == "user" {
			content = withoutTypedUserText(content)
		}
		body := blocksToText(content)
		if strings.TrimSpace(body) == "" {
			continue
		}
		tag := "user"
		mark := render.SlotMarkUser
		if role == "assistant" {
			tag = "assistant"
			mark = render.SlotMarkAssistant
		}
		attr := ` t="` + strconv.Itoa(i) + `"`
		textOut = append(textOut, "<"+tag+attr+">\n"+body+"\n</"+tag+">")
		slotOut = append(slotOut, render.RoleSlotSegment(tag, body, mark, attr))
	}
	return strings.Join(textOut, "\n\n"), strings.Join(slotOut, "\n\n")
}

func compactPreview(text string) string {
	compact := strings.TrimSpace(jsWSReplace(text))
	if u16len(compact) <= latestCollapsedUserPreviewChars {
		return compact
	}
	return strings.TrimRightFunc(u16Slice(compact, 0, latestCollapsedUserPreviewChars), isJSSpace) + "..."
}

// jsWSReplace mirrors /\s+/g with the JS \s class (includes NBSP etc.).
func jsWSReplace(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	inRun := false
	for _, r := range text {
		if isJSSpace(r) {
			if !inRun {
				b.WriteByte(' ')
				inRun = true
			}
			continue
		}
		inRun = false
		b.WriteRune(r)
	}
	return b.String()
}

func verbatimTaskText(text string) string {
	t := strings.TrimSpace(text)
	tl := u16len(t)
	if tl <= latestCollapsedUserVerbatim {
		return t
	}
	elided := tl - verbatimHeadChars - verbatimTailChars
	return u16Slice(t, 0, verbatimHeadChars) +
		"\n[… middle elided (" + strconv.Itoa(elided) + " chars) …]\n" +
		u16Slice(t, tl-verbatimTailChars, tl)
}

func withoutTypedUserText(content any) any {
	if _, ok := content.(string); ok {
		return ""
	}
	arr, ok := asArr(content)
	if !ok {
		return content
	}
	_, typedIdx := splitUserTyped(content)
	if len(typedIdx) == 0 {
		return content
	}
	var out []any
	for i, bv := range arr {
		if _, drop := typedIdx[i]; !drop {
			out = append(out, bv)
		}
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func typedUserText(content any) string {
	text, _ := splitUserTyped(content)
	return text
}

func splitUserTyped(content any) (string, map[int]struct{}) {
	typedIdx := map[int]struct{}{}
	if s, ok := content.(string); ok {
		return strings.TrimSpace(s), typedIdx
	}
	arr, ok := asArr(content)
	if !ok {
		return "", typedIdx
	}
	boundaryIdx := -1
	for i, bv := range arr {
		if blockType(bv) == "text" {
			if bm, ok := asMap(bv); ok {
				if t, ok := getStr(bm, "text"); ok && t == endOfRenderedContext {
					boundaryIdx = i
					break
				}
			}
		}
	}
	var parts []string
	for i, bv := range arr {
		if boundaryIdx >= 0 && i <= boundaryIdx {
			continue
		}
		bm, ok := asMap(bv)
		if !ok || blockType(bv) != "text" {
			continue
		}
		raw, ok := getStr(bm, "text")
		if !ok {
			continue
		}
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "<system-reminder>") {
			continue
		}
		parts = append(parts, text)
		typedIdx[i] = struct{}{}
	}
	return strings.Join(parts, "\n\n"), typedIdx
}

func splitStandingInstructions(text string) (reminders []string, rest string) {
	rest = text
	for {
		m := leadingReminderRe.FindString(rest)
		if m == "" {
			break
		}
		reminders = append(reminders, strings.TrimSpace(m))
		rest = rest[len(m):]
	}
	return reminders, rest
}

func demoteProtectedHeadText(head []any) []any {
	out := make([]any, len(head))
	for idx, mv := range head {
		out[idx] = demoteProtectedMessage(mv, idx)
	}
	return out
}

func demoteProtectedMessage(mv any, idx int) any {
	m, ok := asMap(mv)
	if !ok {
		return mv
	}
	if role, _ := getStr(m, "role"); role != "user" {
		return mv
	}
	tomb := func(preview string, cc any, hasCC bool) map[string]any {
		t := textBlock(`[Opening turn <user t="` + strconv.Itoa(idx) + `"> of this session — PRIOR CONTEXT ONLY, ` +
			`superseded by later turns; NOT the current request and must not be acted ` +
			`on. Preview: "` + preview + `"]`)
		if hasCC {
			t["cache_control"] = cc
		}
		return t
	}
	demoteText := func(text string, cc any, hasCC bool) []any {
		reminders, rest := splitStandingInstructions(text)
		preview := compactPreview(rest)
		if len(reminders) == 0 {
			if preview == "" {
				return nil
			}
			return []any{tomb(preview, cc, hasCC)}
		}
		var blocks []any
		for _, r := range reminders {
			blocks = append(blocks, textBlock(r))
		}
		if preview != "" {
			blocks = append(blocks, tomb(preview, nil, false))
		}
		if hasCC {
			lastBlk := blocks[len(blocks)-1].(map[string]any)
			lastBlk["cache_control"] = cc
		}
		return blocks
	}
	if s, ok := m["content"].(string); ok {
		blocks := demoteText(s, nil, false)
		if blocks == nil {
			return mv
		}
		nm := cloneMap(m)
		nm["content"] = blocks
		return nm
	}
	arr, ok := asArr(m["content"])
	if !ok {
		return mv
	}
	boundaryIdx := -1
	for i, bv := range arr {
		if blockType(bv) == "text" {
			if bm, ok := asMap(bv); ok {
				if t, ok := getStr(bm, "text"); ok && t == endOfRenderedContext {
					boundaryIdx = i
					break
				}
			}
		}
	}
	changed := false
	var outBlocks []any
	for i, bv := range arr {
		if boundaryIdx >= 0 && i <= boundaryIdx {
			outBlocks = append(outBlocks, bv)
			continue
		}
		if bm, ok := asMap(bv); ok && blockType(bv) == "text" {
			if t, ok := getStr(bm, "text"); ok {
				cc, hasCC := bm["cache_control"]
				blocks := demoteText(t, cc, hasCC)
				if blocks != nil {
					outBlocks = append(outBlocks, blocks...)
					changed = true
					continue
				}
			}
		}
		outBlocks = append(outBlocks, bv)
	}
	if !changed {
		return mv
	}
	nm := cloneMap(m)
	nm["content"] = outBlocks
	return nm
}

func userTurnBlocks(messages []any, fromInclusive, upToExclusive int, onImage func(*render.RenderedImage)) ([]any, error) {
	var out []any
	var pending []string
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, textBlock("[User turns from this session, verbatim — these are the user's own words, kept as text rather than rendered into the images above. PRIOR context: none of it is the current request unless the live text at the end of this message says to continue it.\n"+strings.Join(pending, "\n")+"\n]"))
		pending = nil
	}
	for i := fromInclusive; i < upToExclusive; i++ {
		m, ok := asMap(messages[i])
		if !ok {
			continue
		}
		if role, _ := getStr(m, "role"); role != "user" {
			continue
		}
		typed := typedUserText(m["content"])
		if typed == "" {
			continue
		}
		if u16len(typed) <= userTextMaxChars {
			pending = append(pending, `<user t="`+strconv.Itoa(i)+`">`+typed+`</user>`)
			continue
		}
		flush()
		imgs, err := render.RenderTextToPngsWithCharLimit(
			`<user t="`+strconv.Itoa(i)+"\">\n"+typed+"\n</user>",
			render.DenseContentCols,
			render.DenseContentCharsPerImage,
			render.DenseRenderStyle,
			render.MaxHeightPx,
			nil,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, textBlock(`[<user t="`+strconv.Itoa(i)+`"> was too long to carry as text (`+strconv.Itoa(u16len(typed))+` chars); it is rendered verbatim in the image(s) immediately below, separate from the history transcript. PRIOR context, not the current request. Preview: `+compactPreview(typed)+`]`))
		for _, img := range imgs {
			out = append(out, makeImageBlock(base64.StdEncoding.EncodeToString(img.PNG)))
			onImage(img)
		}
	}
	flush()
	return out, nil
}

func latestCollapsedUserPointer(messages []any, upToExclusive, protectedPrefix int) map[string]any {
	for i := upToExclusive - 1; i >= 0; i-- {
		m, ok := asMap(messages[i])
		if !ok {
			continue
		}
		if role, _ := getStr(m, "role"); role != "user" {
			continue
		}
		typed := typedUserText(m["content"])
		if typed == "" {
			continue
		}
		if i >= protectedPrefix {
			preview := compactPreview(typed)
			return textBlock(`[Most recent collapsed user turn: <user t="` + strconv.Itoa(i) + `">` + preview + `</user>. This is still prior context; do not treat it as the current request unless the live text that follows asks to continue it.]`)
		}
		carried := verbatimTaskText(typed)
		return textBlock(`[Most recent collapsed user turn, carried verbatim because it appears nowhere else in full: <user t="` + strconv.Itoa(i) + `">` + carried + `</user>. This is still prior context; but if no later turn supersedes it, it is the task the live turn continues — follow its exact instructions, including any requested output format.]`)
	}
	return nil
}

func collapseHistory(messages []any, isProfitable profitableFn, o historyCollapseOptions) ([]any, *historyCollapseInfo, error) {
	info := &historyCollapseInfo{droppedCodepoints: map[rune]int{}}
	if len(messages) == 0 {
		info.reason = "no_history"
		return messages, info, nil
	}
	protectedPrefix := o.protectedPrefix
	if protectedPrefix < 0 {
		protectedPrefix = 0
	}
	if protectedPrefix > len(messages) {
		protectedPrefix = len(messages)
	}
	rawCutoff := len(messages) - o.keepTail
	cutoff := rawCutoff
	if o.collapseChunk > 0 {
		snapped := floorDiv(rawCutoff, o.collapseChunk) * o.collapseChunk
		floor := o.minCollapsePrefix + protectedPrefix
		if snapped < floor {
			snapped = floor
		}
		if snapped < rawCutoff {
			cutoff = snapped
		}
	}
	boundary := findClosedPrefixBoundary(messages, cutoff)
	if boundary < 0 {
		info.reason = "no_closed_prefix"
		return messages, info, nil
	}
	collapseLen := boundary + 1
	if collapseLen-protectedPrefix < o.minCollapsePrefix {
		info.reason = "prefix_too_short"
		return messages, info, nil
	}
	text, _ := messagesToHistorySegments(messages, collapseLen, protectedPrefix)
	if text == "" {
		info.reason = "render_empty"
		return messages, info, nil
	}
	safeText := render.NeutralizeSentinel(text)
	renderText := text
	if o.reflow {
		if packed, ok := render.Reflow(safeText); ok {
			renderText = packed
		} else {
			renderText = safeText
		}
	}
	if !isProfitable(renderText, o.cols) {
		info.reason = "not_profitable"
		info.collapsedChars = u16len(text)
		return messages, info, nil
	}

	step := o.freezeChunk
	if step <= 0 {
		step = collapseLen - protectedPrefix
	}
	ends := map[int]struct{}{}
	for e := protectedPrefix + step; e < collapseLen; e += step {
		ends[e] = struct{}{}
	}
	markerByEnd := map[int]any{}
	for i := protectedPrefix; i < collapseLen; i++ {
		if m, ok := asMap(messages[i]); ok {
			if cc, has := messageCacheControl(m); has {
				ends[i+1] = struct{}{}
				markerByEnd[i+1] = cc
			}
		}
	}
	ends[collapseLen] = struct{}{}
	var sortedEnds []int
	for e := range ends {
		if e > protectedPrefix && e <= collapseLen {
			sortedEnds = append(sortedEnds, e)
		}
	}
	sort.Ints(sortedEnds)

	carryOverEnd := -1
	for e := protectedPrefix + step; e < collapseLen; e += step {
		carryOverEnd = e
	}
	carryOverOrdinal := -1

	var blocks []any
	imageCount := 0
	countImage := func(img *render.RenderedImage) {
		imageCount++
		info.collapsedImageBytes += len(img.PNG)
		info.collapsedImagePixels += img.Width * img.Height
		info.collapsedPngs = append(info.collapsedPngs, img.PNG)
		info.collapsedImageDims = append(info.collapsedImageDims, imageDim{img.Width, img.Height})
		info.droppedChars += img.DroppedChars
		for cp, n := range img.DroppedCodepoints {
			info.droppedCodepoints[cp] += n
		}
	}
	chunkStart := protectedPrefix
	for _, chunkEnd := range sortedEnds {
		segText, segSlot := messagesToHistorySegments(messages, chunkEnd, chunkStart)
		userFrom := chunkStart
		chunkStart = chunkEnd
		if segText == "" {
			ub, err := userTurnBlocks(messages, userFrom, chunkEnd, countImage)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, ub...)
			continue
		}
		chunkRender, chunkSlot := segText, segSlot
		if o.reflow {
			st := render.NeutralizeSentinel(segText)
			ss := render.NeutralizeSentinel(segSlot)
			rt, okT := render.Reflow(st)
			rs, okS := render.Reflow(ss)
			if okT && okS {
				chunkRender, chunkSlot = rt, rs
			} else {
				chunkRender, chunkSlot = st, ss
			}
		}
		style := o.style
		style.ColorByRole = true
		imgs, err := render.RenderTextToPngsWithCharLimit(
			chunkRender, o.cols, render.DenseContentCharsPerImage, style, o.maxHeightPx, &chunkSlot,
		)
		if err != nil {
			return nil, nil, err
		}
		markerCC, hasMarker := markerByEnd[chunkEnd]
		for k, img := range imgs {
			block := makeImageBlock(base64.StdEncoding.EncodeToString(img.PNG))
			if hasMarker && k == len(imgs)-1 {
				block["cache_control"] = markerCC
			}
			blocks = append(blocks, block)
			countImage(img)
		}
		if chunkEnd == carryOverEnd {
			carryOverOrdinal = imageCount - 1
		}
		ub, err := userTurnBlocks(messages, userFrom, chunkEnd, countImage)
		if err != nil {
			return nil, nil, err
		}
		blocks = append(blocks, ub...)
	}
	if imageCount == 0 {
		info.reason = "render_empty"
		return messages, info, nil
	}
	syntheticContent := []any{textBlock(HistorySyntheticIntro)}
	syntheticContent = append(syntheticContent, blocks...)
	if ptr := latestCollapsedUserPointer(messages, collapseLen, protectedPrefix); ptr != nil {
		syntheticContent = append(syntheticContent, ptr)
	}
	if fs := FactSheetText(text); fs != "" {
		syntheticContent = append(syntheticContent, textBlock(fs))
	}
	syntheticContent = append(syntheticContent, textBlock(HistorySyntheticOutro))
	syntheticUser := map[string]any{"role": "user", "content": syntheticContent}

	head := demoteProtectedHeadText(messages[:protectedPrefix])
	tail := messages[collapseLen:]
	info.collapsedTurns = collapseLen - protectedPrefix
	info.collapsedChars = u16len(text)
	info.collapsedImages = imageCount
	if carryOverOrdinal >= 0 {
		info.carryOverImageOrdinal = carryOverOrdinal
		info.hasCarryOver = true
	}
	out := make([]any, 0, len(head)+1+len(tail))
	out = append(out, head...)
	out = append(out, syntheticUser)
	out = append(out, tail...)
	return out, info, nil
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func makeImageBlock(pngB64 string) map[string]any {
	src := map[string]any{
		"type":       "base64",
		"media_type": "image/png",
		"data":       pngB64,
	}
	setObjKeyOrder(src, []string{"type", "media_type", "data"})
	m := map[string]any{"type": "image", "source": src}
	setObjKeyOrder(m, []string{"type", "source"})
	return m
}
