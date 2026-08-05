package pxpipe

import (
	"sort"
	"strconv"
	"strings"

	"github.com/evan-choi/pxpipe-go/render"
)

// Port of src/core/openai-history.ts: GPT history-image compression planners
// for Chat Completions messages and Responses input items.

const gptHistoryCols = 152

// gptProfitableFn is the break-even gate predicate, injected to avoid a
// circular dependency with openai.go. baselineTextTokens < 0 means "unset".
type gptProfitableFn func(text string, cols int, baselineTextTokens int) bool

type gptHistoryOptions struct {
	KeepTail          int
	MaxImages         int
	KeepRecentPairs   int
	ResponsesMode     string // "pairs" | "mixed"
	MinCollapsePrefix int
	MinCollapseTokens int
	Cols              int
	CollapseChunk     int
	FreezeChunk       int
	SectionTokens     int
	MaxHeightPx       int
	Style             render.RenderStyle
	Reflow            bool
	tokenCounts       gptTokenCounter
}

func defaultGptHistoryOptions() gptHistoryOptions {
	return gptHistoryOptions{
		KeepTail:          6,
		KeepRecentPairs:   6,
		ResponsesMode:     "pairs",
		MinCollapsePrefix: 10,
		MinCollapseTokens: 2000,
		Cols:              gptHistoryCols,
		CollapseChunk:     10,
		FreezeChunk:       10,
		SectionTokens:     2000,
		MaxHeightPx:       GptMaxHeightPx,
		Style:             DefaultGptProfile.Style,
		MaxImages:         DefaultGptProfile.History.MaxImages,
		Reflow:            true,
	}
}

// historyTurn is one conversation item lowered to a renderable unit.
type historyTurn struct {
	Text     string
	OpenIds  []string
	CloseIds []string
	Opaque   bool
	// UserText is set (non-nil) when the item is a real USER request.
	UserText *string
}

type responsesPairState struct {
	CompletedPairs                int
	RecentCompletedPairs          int
	OldCompletedPairs             int
	OpenCalls                     int
	OrphanOutputs                 int
	MalformedItems                int
	ImageableFunctionCallTokens   int
	ImageableFunctionOutputTokens int
	CollapsedPairs                int
	CollapsedFunctionCallTokens   int
	CollapsedFunctionOutputTokens int
}

type responsesPairCollapseSegment struct {
	InsertAt        int
	SelectedIndices []int
	Images          []*render.RenderedImage
	ImageSources    []string
	Text            string
	// BaselineTokens < 0 = unset.
	BaselineTokens int
}

type gptCollapsePlan struct {
	Images            []*render.RenderedImage
	ImagesAfter       []*render.RenderedImage
	ImageSources      []string
	ImageSourcesAfter []string
	// PinText nil = nothing pinned.
	PinText *string
	Text    string
	// BaselineTokens < 0 = unset.
	BaselineTokens    int
	Start             int
	EndExclusive      int
	CollapsedTurns    int
	CollapsedChars    int
	Reason            string // "" = collapsed
	DroppedChars      int
	DroppedCodepoints map[rune]int

	// Responses-plan extras (zero-valued for the chat planner).
	Segments        []responsesPairCollapseSegment
	SelectedIndices []int
	PairState       responsesPairState
	BarrierTypes    map[string]int
}

func emptyGptPlan() gptCollapsePlan {
	return gptCollapsePlan{BaselineTokens: -1, DroppedCodepoints: map[rune]int{}}
}

func safeJSONString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return string(jsStringify(v))
}

// findClosedBoundary returns the last index i in [from, cutoffExclusive) where
// every opened tool-call id has a matching close; from-1 if none.
func findClosedBoundary(turns []historyTurn, cutoffExclusive, from int) int {
	open := map[string]struct{}{}
	lastClosed := from - 1
	limit := minInt(cutoffExclusive, len(turns))
	for i := from; i < limit; i++ {
		t := &turns[i]
		if t.Opaque {
			break
		}
		for _, id := range t.OpenIds {
			open[id] = struct{}{}
		}
		for _, id := range t.CloseIds {
			delete(open, id)
		}
		if len(open) == 0 {
			lastClosed = i
		}
	}
	return lastClosed
}

func isClosedPrefix(turns []historyTurn, from, toExclusive int) bool {
	open := map[string]struct{}{}
	for i := from; i < toExclusive; i++ {
		t := &turns[i]
		if t.Opaque {
			return false
		}
		for _, id := range t.OpenIds {
			open[id] = struct{}{}
		}
		for _, id := range t.CloseIds {
			delete(open, id)
		}
	}
	return len(open) == 0
}

func joinTurns(turns []historyTurn, from, toExclusive, skip int) string {
	var parts []string
	for i := from; i < toExclusive; i++ {
		if i == skip {
			continue
		}
		if s := turns[i].Text; len(s) > 0 {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func historyMaybeReflow(text string, enabled bool) string {
	safe := render.NeutralizeSentinel(text)
	if !enabled {
		// TS: `let renderText = o.reflow ? reflow(safeText) ?? safeText : text`
		return text
	}
	if packed, ok := render.Reflow(safe); ok {
		return packed
	}
	return safe
}

// planGptCollapse plans + renders a history collapse over pre-lowered turns.
func planGptCollapse(turns []historyTurn, protectedPrefix int, isProfitable gptProfitableFn, o gptHistoryOptions) (gptCollapsePlan, error) {
	base := emptyGptPlan()
	pp := maxInt(0, minInt(protectedPrefix, len(turns)))
	rawCutoff := len(turns) - o.KeepTail
	if rawCutoff-pp < o.MinCollapsePrefix {
		base.Reason = "prefix_too_short"
		return base, nil
	}
	cutoff := rawCutoff
	if o.CollapseChunk > 0 {
		cutoff = minInt(rawCutoff, maxInt(
			pp+o.MinCollapsePrefix,
			pp+(rawCutoff-pp)/o.CollapseChunk*o.CollapseChunk,
		))
	}
	boundary := findClosedBoundary(turns, cutoff, pp)
	if boundary < pp {
		base.Reason = "no_closed_prefix"
		return base, nil
	}
	if boundary+1-pp < o.MinCollapsePrefix {
		base.Reason = "prefix_too_short"
		return base, nil
	}
	rawEnd := boundary + 1

	// Pin the most-recent user turn overall when it falls inside the range.
	pinIdx := -1
	for i := len(turns) - 1; i >= pp; i-- {
		if turns[i].UserText != nil {
			pinIdx = i
			break
		}
	}
	if pinIdx >= rawEnd {
		pinIdx = -1
	}
	if pinIdx >= 0 && !isClosedPrefix(turns, pp, pinIdx) {
		pinIdx = -1
	}

	text := joinTurns(turns, pp, rawEnd, pinIdx)
	if text == "" || o.tokenCounts.count(text) < o.MinCollapseTokens {
		base.Reason = "below_min_tokens"
		base.CollapsedChars = u16len(text)
		return base, nil
	}
	renderText := historyMaybeReflow(text, o.Reflow)
	if !isProfitable(renderText, o.Cols, -1) {
		base.Reason = "not_profitable"
		base.CollapsedChars = u16len(text)
		return base, nil
	}

	// Append-only, token-length sectioning.
	type section struct{ s, e int }
	var sections []section
	if o.FreezeChunk <= 0 {
		if pinIdx > pp {
			sections = append(sections, section{pp, pinIdx})
		}
		afterStart := pp
		if pinIdx >= pp {
			afterStart = pinIdx + 1
		}
		if afterStart < rawEnd {
			sections = append(sections, section{afterStart, rawEnd})
		}
	} else {
		secStart := pp
		acc := 0
		open := map[string]struct{}{}
		for i := pp; i < rawEnd; i++ {
			if i == pinIdx {
				if secStart < i {
					if len(sections) > 0 && acc < o.SectionTokens && sections[len(sections)-1].e == secStart {
						sections[len(sections)-1].e = i
					} else {
						sections = append(sections, section{secStart, i})
					}
				}
				secStart = i + 1
				acc = 0
				continue
			}
			acc += o.tokenCounts.count(turns[i].Text)
			for _, id := range turns[i].OpenIds {
				open[id] = struct{}{}
			}
			for _, id := range turns[i].CloseIds {
				delete(open, id)
			}
			if acc >= o.SectionTokens && len(open) == 0 {
				sections = append(sections, section{secStart, i + 1})
				secStart = i + 1
				acc = 0
			}
		}
	}
	if len(sections) == 0 {
		base.Reason = "below_min_tokens"
		base.CollapsedChars = u16len(text)
		return base, nil
	}

	maxImages := maxInt(0, o.MaxImages)
	type renderedSection struct {
		s, e int
		imgs []*render.RenderedImage
	}
	var rendered []renderedSection
	imgCount := 0
	collapseEnd := pp
	for _, sec := range sections {
		sectionText := joinTurns(turns, sec.s, sec.e, -1)
		if sectionText == "" {
			continue
		}
		sectionRender := historyMaybeReflow(sectionText, o.Reflow)
		sectionImgs, err := render.RenderTextToPngs(sectionRender, o.Cols, o.Style, o.MaxHeightPx, nil)
		if err != nil {
			return base, err
		}
		if imgCount+len(sectionImgs) > maxImages {
			break
		}
		rendered = append(rendered, renderedSection{sec.s, sec.e, sectionImgs})
		imgCount += len(sectionImgs)
		collapseEnd = sec.e
	}

	pinConsumed := pinIdx >= pp && collapseEnd > pinIdx
	var imagesBefore, imagesAfter []*render.RenderedImage
	var imageSources, imageSourcesAfter []string
	for _, r := range rendered {
		source := joinTurns(turns, r.s, r.e, -1)
		if pinConsumed && r.s >= pinIdx+1 {
			imagesAfter = append(imagesAfter, r.imgs...)
			for range r.imgs {
				imageSourcesAfter = append(imageSourcesAfter, source)
			}
		} else {
			imagesBefore = append(imagesBefore, r.imgs...)
			for range r.imgs {
				imageSources = append(imageSources, source)
			}
		}
	}
	if len(imagesBefore) == 0 && len(imagesAfter) == 0 {
		base.Reason = "too_many_images"
		base.CollapsedChars = u16len(text)
		return base, nil
	}
	var pinText *string
	if pinConsumed {
		pinText = turns[pinIdx].UserText
	}
	skip := -1
	if pinConsumed {
		skip = pinIdx
	}
	collapsedText := joinTurns(turns, pp, collapseEnd, skip)
	droppedCodepoints := map[rune]int{}
	droppedChars := 0
	for _, img := range imagesBefore {
		droppedChars += img.DroppedChars
		for cp, n := range img.DroppedCodepoints {
			droppedCodepoints[cp] += n
		}
	}
	for _, img := range imagesAfter {
		droppedChars += img.DroppedChars
		for cp, n := range img.DroppedCodepoints {
			droppedCodepoints[cp] += n
		}
	}
	collapsedTurns := collapseEnd - pp
	if pinConsumed {
		collapsedTurns--
	}
	return gptCollapsePlan{
		Images:            imagesBefore,
		ImagesAfter:       imagesAfter,
		ImageSources:      imageSources,
		ImageSourcesAfter: imageSourcesAfter,
		PinText:           pinText,
		Text:              collapsedText,
		BaselineTokens:    -1,
		Start:             pp,
		EndExclusive:      collapseEnd,
		CollapsedTurns:    collapsedTurns,
		CollapsedChars:    u16len(collapsedText),
		DroppedChars:      droppedChars,
		DroppedCodepoints: droppedCodepoints,
	}, nil
}

// ---- Responses completed-pair planning -------------------------------------

// safeJSONKey mirrors TS safeJson(obj.key): explicit null stringifies to
// "null" while a missing key (undefined) becomes "".
func safeJSONKey(item map[string]any, key string) string {
	v, present := item[key]
	if !present {
		return ""
	}
	return safeJSONString(v)
}

func responseCallText(item map[string]any) string {
	name := "tool"
	if s, ok := item["name"].(string); ok {
		name = s
	}
	return "[tool_use " + name + "]\n" + safeJSONKey(item, "arguments")
}

func responseOutputText(item map[string]any) string {
	return "[tool_result]\n" + safeJSONKey(item, "output")
}

type responsesCompletedPair struct {
	CallIndex    int
	OutputIndex  int
	Text         string
	CallTokens   int
	OutputTokens int
}

type responsesCompletedRound struct {
	Pairs        []responsesCompletedPair
	Indices      []int
	StartIndex   int
	EndIndex     int
	Text         string
	CallTokens   int
	OutputTokens int
}

func responseItemType(item any) string {
	if o, ok := item.(map[string]any); ok {
		if t, ok := o["type"].(string); ok {
			return t
		}
	}
	return ""
}

func responseCallID(item any) string {
	if o, ok := item.(map[string]any); ok {
		if id, ok := o["call_id"].(string); ok {
			return id
		}
	}
	return ""
}

type responsesMessageText struct {
	Role string
	Text string
}

// responseMessageText returns a lossless textual Responses message, or nil.
func responseMessageText(item any) *responsesMessageText {
	o, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	role, _ := o["role"].(string)
	if role != "user" && role != "assistant" {
		return nil
	}
	if it, ok := o["type"].(string); ok && it != "" && it != "message" {
		return nil
	}
	var body string
	switch content := o["content"].(type) {
	case string:
		body = content
	case []any:
		var parts []string
		for _, part := range content {
			p, ok := part.(map[string]any)
			if !ok || p == nil {
				return nil
			}
			ptype, _ := p["type"].(string)
			if ptype == "refusal" {
				if refusal, ok := p["refusal"].(string); ok {
					parts = append(parts, refusal)
					continue
				}
			}
			txt, ok := p["text"].(string)
			if !ok {
				return nil
			}
			parts = append(parts, txt)
		}
		body = strings.Join(parts, "\n\n")
	default:
		return nil
	}
	if strings.TrimSpace(body) == "" {
		return nil
	}
	return &responsesMessageText{Role: role, Text: body}
}

// isContentlessMessage is true for a message item with no renderable text.
func isContentlessMessage(item any) bool {
	o, ok := item.(map[string]any)
	if !ok {
		return false
	}
	role, _ := o["role"].(string)
	if role != "user" && role != "assistant" {
		return false
	}
	if it, ok := o["type"].(string); ok && it != "" && it != "message" {
		return false
	}
	switch content := o["content"].(type) {
	case string:
		return strings.TrimSpace(content) == ""
	case []any:
		for _, part := range content {
			p, ok := part.(map[string]any)
			if !ok || p == nil {
				return false
			}
			ptype, _ := p["type"].(string)
			if ptype == "refusal" {
				if refusal, ok := p["refusal"].(string); ok {
					if strings.TrimSpace(refusal) != "" {
						return false
					}
					continue
				}
			}
			txt, ok := p["text"].(string)
			if !ok {
				return false
			}
			if strings.TrimSpace(txt) != "" {
				return false
			}
		}
		return true
	}
	return false
}

func responseMessageTranscript(item any, index int) string {
	msg := responseMessageText(item)
	if msg == nil {
		return ""
	}
	return "<" + msg.Role + " t=\"" + strconv.Itoa(index) + "\">\n" + msg.Text + "\n</" + msg.Role + ">"
}

func responseReferencedIds(items []any) map[string]struct{} {
	refs := map[string]struct{}{}
	for _, item := range items {
		o, ok := item.(map[string]any)
		if !ok || o["type"] != "item_reference" {
			continue
		}
		for _, key := range []string{"id", "item_id", "ref_id"} {
			if s, ok := o[key].(string); ok && s != "" {
				refs[s] = struct{}{}
			}
		}
	}
	return refs
}

// classifyResponsesPairs classifies tool state and returns old (imageable)
// rounds plus counters.
func classifyResponsesPairs(items []any, keepRecentPairs int, tokenCounts gptTokenCounter) ([]responsesCompletedRound, responsesPairState) {
	calls := map[string][]int{}
	outputs := map[string][]int{}
	missingIDItems := 0
	// Track id first-seen order for deterministic iteration (TS Set order).
	var idOrder []string
	seen := map[string]struct{}{}
	for i, item := range items {
		t := responseItemType(item)
		if t != "function_call" && t != "function_call_output" {
			continue
		}
		id := responseCallID(item)
		if id == "" {
			missingIDItems++
			continue
		}
		if t == "function_call" {
			calls[id] = append(calls[id], i)
		} else {
			outputs[id] = append(outputs[id], i)
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			idOrder = append(idOrder, id)
		}
	}

	pairByCallIndex := map[int]responsesCompletedPair{}
	openCalls, orphanOutputs, malformedItems := 0, 0, missingIDItems
	for _, id := range idOrder {
		cs := calls[id]
		os := outputs[id]
		switch {
		case len(cs) == 1 && len(os) == 1 && cs[0] < os[0]:
			callIndex, outputIndex := cs[0], os[0]
			call := items[callIndex].(map[string]any)
			output := items[outputIndex].(map[string]any)
			callJSON := jsStringify(call)
			outTokens := 0
			if s, ok := output["output"].(string); ok {
				outTokens = tokenCounts.count(s)
			} else {
				outTokens = tokenCounts.count(safeJSONString(output["output"]))
			}
			pairByCallIndex[callIndex] = responsesCompletedPair{
				CallIndex:    callIndex,
				OutputIndex:  outputIndex,
				Text:         responseCallText(call) + "\n" + responseOutputText(output),
				CallTokens:   tokenCounts.count(string(callJSON)),
				OutputTokens: outTokens,
			}
		case len(cs) > 0 && len(os) == 0:
			openCalls += len(cs)
		case len(os) > 0 && len(cs) == 0:
			orphanOutputs += len(os)
		default:
			malformedItems += len(cs) + len(os)
		}
	}

	var completed []responsesCompletedRound
	acceptedCallIndices := map[int]struct{}{}
	for i := 0; i < len(items); {
		if responseItemType(items[i]) != "function_call" {
			i++
			continue
		}
		if _, ok := pairByCallIndex[i]; !ok {
			i++
			continue
		}
		var roundCalls []responsesCompletedPair
		j := i
		for j < len(items) && responseItemType(items[j]) == "function_call" {
			if p, ok := pairByCallIndex[j]; ok {
				roundCalls = append(roundCalls, p)
				j++
			} else {
				break
			}
		}
		roundOutputIndices := map[int]struct{}{}
		for _, pair := range roundCalls {
			roundOutputIndices[pair.OutputIndex] = struct{}{}
		}
		var outputIdxs []int
		for j < len(items) && responseItemType(items[j]) == "function_call_output" {
			if _, ok := roundOutputIndices[j]; !ok {
				break
			}
			outputIdxs = append(outputIdxs, j)
			j++
		}
		outputSet := map[int]struct{}{}
		for _, oi := range outputIdxs {
			outputSet[oi] = struct{}{}
		}
		valid := len(roundCalls) > 0 && len(outputIdxs) == len(roundCalls)
		if valid {
			for _, pair := range roundCalls {
				if _, ok := outputSet[pair.OutputIndex]; !ok {
					valid = false
					break
				}
			}
		}
		if !valid {
			i++
			continue
		}
		byOutput := append([]responsesCompletedPair(nil), roundCalls...)
		sort.Slice(byOutput, func(a, b int) bool { return byOutput[a].OutputIndex < byOutput[b].OutputIndex })
		var indices []int
		for _, pair := range roundCalls {
			indices = append(indices, pair.CallIndex)
		}
		for _, pair := range byOutput {
			indices = append(indices, pair.OutputIndex)
		}
		var texts []string
		callTokens, outputTokens := 0, 0
		for _, pair := range byOutput {
			texts = append(texts, pair.Text)
		}
		for _, pair := range roundCalls {
			callTokens += pair.CallTokens
			outputTokens += pair.OutputTokens
		}
		completed = append(completed, responsesCompletedRound{
			Pairs:        roundCalls,
			Indices:      indices,
			StartIndex:   i,
			EndIndex:     j - 1,
			Text:         strings.Join(texts, "\n\n"),
			CallTokens:   callTokens,
			OutputTokens: outputTokens,
		})
		for _, pair := range roundCalls {
			acceptedCallIndices[pair.CallIndex] = struct{}{}
		}
		i = j
	}
	for callIndex := range pairByCallIndex {
		if _, ok := acceptedCallIndices[callIndex]; !ok {
			malformedItems += 2
		}
	}

	keep := maxInt(0, keepRecentPairs)
	recentStart := len(completed)
	recentPairs := 0
	for recentStart > 0 && recentPairs < keep {
		recentStart--
		recentPairs += len(completed[recentStart].Pairs)
	}
	old := completed[:recentStart]
	completedPairs, oldPairs := 0, 0
	imageableCallTokens, imageableOutputTokens := 0, 0
	for _, round := range completed {
		completedPairs += len(round.Pairs)
	}
	for _, round := range old {
		oldPairs += len(round.Pairs)
		imageableCallTokens += round.CallTokens
		imageableOutputTokens += round.OutputTokens
	}
	return old, responsesPairState{
		CompletedPairs:                completedPairs,
		RecentCompletedPairs:          completedPairs - oldPairs,
		OldCompletedPairs:             oldPairs,
		OpenCalls:                     openCalls,
		OrphanOutputs:                 orphanOutputs,
		MalformedItems:                malformedItems,
		ImageableFunctionCallTokens:   imageableCallTokens,
		ImageableFunctionOutputTokens: imageableOutputTokens,
	}
}

func emptyResponsesPairPlan(state responsesPairState) gptCollapsePlan {
	p := emptyGptPlan()
	p.PairState = state
	return p
}

type responsesMixedUnit struct {
	Indices        []int
	Text           string
	BaselineTokens int
}

type renderedUnits struct {
	Source       string
	RenderedText string
	Images       []*render.RenderedImage
}

func renderUnitTexts(texts []string, o gptHistoryOptions) (renderedUnits, error) {
	source := strings.Join(texts, "\n\n")
	safe := render.NeutralizeSentinel(source)
	renderedText := safe
	if o.Reflow {
		if packed, ok := render.Reflow(safe); ok {
			renderedText = packed
		}
	}
	images, err := render.RenderTextToPngs(renderedText, o.Cols, o.Style, o.MaxHeightPx, nil)
	if err != nil {
		return renderedUnits{}, err
	}
	return renderedUnits{Source: source, RenderedText: renderedText, Images: images}, nil
}

// planResponsesMixedCollapse is the profile-gated broad Responses planner.
func planResponsesMixedCollapse(items []any, old []responsesCompletedRound, state responsesPairState, isProfitable gptProfitableFn, o gptHistoryOptions) (gptCollapsePlan, error) {
	base := emptyResponsesPairPlan(state)
	oldByCall := map[int]*responsesCompletedRound{}
	for i := range old {
		oldByCall[old[i].StartIndex] = &old[i]
	}
	var messageIndices []int
	referencedIds := responseReferencedIds(items)
	latestUserIndex := -1
	for i, item := range items {
		msg := responseMessageText(item)
		if msg == nil {
			continue
		}
		messageIndices = append(messageIndices, i)
		if msg.Role == "user" {
			latestUserIndex = i
		}
	}
	tail := maxInt(0, o.KeepTail)
	protectedMessages := map[int]struct{}{}
	if tail > 0 {
		start := maxInt(0, len(messageIndices)-tail)
		for _, idx := range messageIndices[start:] {
			protectedMessages[idx] = struct{}{}
		}
	}
	if latestUserIndex >= 0 {
		protectedMessages[latestUserIndex] = struct{}{}
	}

	var runs [][]responsesMixedUnit
	var current []responsesMixedUnit
	barrierTypes := map[string]int{}
	noteBarrier := func(index int) {
		if len(current) == 0 {
			return
		}
		t := responseItemType(items[index])
		if t == "" {
			t = "untyped"
		}
		barrierTypes[t]++
	}
	flush := func() {
		if len(current) > 0 {
			runs = append(runs, current)
		}
		current = nil
	}
	for i := 0; i < len(items); i++ {
		round := oldByCall[i]
		roundReferenced := false
		if round != nil {
			for _, index := range round.Indices {
				if it, ok := items[index].(map[string]any); ok {
					if id, ok := it["id"].(string); ok {
						if _, ref := referencedIds[id]; ref {
							roundReferenced = true
							break
						}
					}
				}
			}
		}
		if round != nil && !roundReferenced {
			current = append(current, responsesMixedUnit{
				Indices:        round.Indices,
				Text:           round.Text,
				BaselineTokens: round.CallTokens + round.OutputTokens,
			})
			i = round.EndIndex
			continue
		}
		referenced := false
		if it, ok := items[i].(map[string]any); ok {
			if id, ok := it["id"].(string); ok {
				_, referenced = referencedIds[id]
			}
		}
		_, isProtected := protectedMessages[i]
		text := ""
		if !isProtected && !referenced {
			text = responseMessageTranscript(items[i], i)
		}
		if text != "" {
			current = append(current, responsesMixedUnit{
				Indices:        []int{i},
				Text:           text,
				BaselineTokens: o.tokenCounts.count(responseMessageText(items[i]).Text),
			})
			continue
		}
		if !isProtected && !referenced && isContentlessMessage(items[i]) {
			continue
		}
		noteBarrier(i)
		flush()
	}
	flush()

	var eligible []responsesMixedUnit
	for _, run := range runs {
		eligible = append(eligible, run...)
	}
	var allTextParts []string
	allBaselineTokens := 0
	for _, unit := range eligible {
		allTextParts = append(allTextParts, unit.Text)
		allBaselineTokens += unit.BaselineTokens
	}
	allText := strings.Join(allTextParts, "\n\n")
	if len(eligible) == 0 {
		base.Reason = "no_closed_prefix"
		return base, nil
	}
	if allBaselineTokens < o.MinCollapseTokens {
		base.Reason = "below_min_tokens"
		base.CollapsedChars = u16len(allText)
		return base, nil
	}
	maxImages := maxInt(0, o.MaxImages)
	if maxImages == 0 {
		base.Reason = "too_many_images"
		base.CollapsedChars = u16len(allText)
		return base, nil
	}

	var segments []responsesPairCollapseSegment
	remainingImages := maxImages
	hitImageCap := false
	for _, run := range runs {
		if remainingImages == 0 {
			hitImageCap = true
			break
		}
		low, high := 0, len(run)+1
		var best *renderedUnits
		for low+1 < high {
			count := (low + high) / 2
			var texts []string
			for _, u := range run[:count] {
				texts = append(texts, u.Text)
			}
			rendered, err := renderUnitTexts(texts, o)
			if err != nil {
				return base, err
			}
			if len(rendered.Images) > 0 && len(rendered.Images) <= remainingImages {
				low = count
				best = &rendered
			} else {
				high = count
			}
		}
		if best == nil || low == 0 {
			hitImageCap = true
			break
		}
		selected := run[:low]
		selectedBaselineTokens := 0
		for _, unit := range selected {
			selectedBaselineTokens += unit.BaselineTokens
		}
		if !isProfitable(best.RenderedText, o.Cols, selectedBaselineTokens) {
			continue
		}
		var selectedIndices []int
		for _, unit := range selected {
			selectedIndices = append(selectedIndices, unit.Indices...)
		}
		sort.Ints(selectedIndices)
		sources := make([]string, len(best.Images))
		for i := range sources {
			sources[i] = best.Source
		}
		segments = append(segments, responsesPairCollapseSegment{
			InsertAt:        selectedIndices[0],
			SelectedIndices: selectedIndices,
			Images:          best.Images,
			ImageSources:    sources,
			Text:            best.Source,
			BaselineTokens:  selectedBaselineTokens,
		})
		remainingImages -= len(best.Images)
		if low < len(run) {
			hitImageCap = true
			break
		}
	}

	if len(segments) == 0 {
		if hitImageCap {
			base.Reason = "too_many_images"
		} else {
			base.Reason = "not_profitable"
		}
		base.CollapsedChars = u16len(allText)
		base.BarrierTypes = barrierTypes
		return base, nil
	}
	return finalizeResponsesPlan(base, segments, old, state, barrierTypes, true), nil
}

// finalizeResponsesPlan assembles the shared tail of both Responses planners.
func finalizeResponsesPlan(base gptCollapsePlan, segments []responsesPairCollapseSegment, old []responsesCompletedRound, state responsesPairState, barrierTypes map[string]int, withBaseline bool) gptCollapsePlan {
	var selectedIndices []int
	for _, segment := range segments {
		selectedIndices = append(selectedIndices, segment.SelectedIndices...)
	}
	sort.Ints(selectedIndices)
	selectedIds := map[int]struct{}{}
	for _, idx := range selectedIndices {
		selectedIds[idx] = struct{}{}
	}
	for _, round := range old {
		all := true
		for _, index := range round.Indices {
			if _, ok := selectedIds[index]; !ok {
				all = false
				break
			}
		}
		if all {
			state.CollapsedPairs += len(round.Pairs)
			state.CollapsedFunctionCallTokens += round.CallTokens
			state.CollapsedFunctionOutputTokens += round.OutputTokens
		}
	}
	var images []*render.RenderedImage
	var imageSources []string
	var textParts []string
	baselineTokens := 0
	for _, segment := range segments {
		images = append(images, segment.Images...)
		imageSources = append(imageSources, segment.ImageSources...)
		textParts = append(textParts, segment.Text)
		if segment.BaselineTokens > 0 {
			baselineTokens += segment.BaselineTokens
		}
	}
	text := strings.Join(textParts, "\n\n")
	droppedCodepoints := map[rune]int{}
	droppedChars := 0
	for _, image := range images {
		droppedChars += image.DroppedChars
		for cp, n := range image.DroppedCodepoints {
			droppedCodepoints[cp] += n
		}
	}
	out := base
	out.BarrierTypes = barrierTypes
	out.Segments = segments
	out.Images = images
	out.ImageSources = imageSources
	out.Text = text
	if withBaseline {
		out.BaselineTokens = baselineTokens
	}
	if len(selectedIndices) > 0 {
		out.Start = selectedIndices[0]
		out.EndExclusive = selectedIndices[len(selectedIndices)-1] + 1
	}
	out.CollapsedTurns = len(selectedIndices)
	out.CollapsedChars = u16len(text)
	out.DroppedChars = droppedChars
	out.DroppedCodepoints = droppedCodepoints
	out.SelectedIndices = selectedIndices
	out.PairState = state
	return out
}

// planResponsesPairCollapse renders only old, unambiguously completed
// Responses call/output rounds (or defers to the mixed planner per profile).
func planResponsesPairCollapse(items []any, isProfitable gptProfitableFn, o gptHistoryOptions) (gptCollapsePlan, error) {
	old, state := classifyResponsesPairs(items, o.KeepRecentPairs, o.tokenCounts)
	if o.ResponsesMode == "mixed" {
		return planResponsesMixedCollapse(items, old, state, isProfitable, o)
	}
	base := emptyResponsesPairPlan(state)
	if len(old) == 0 {
		base.Reason = "no_closed_prefix"
		return base, nil
	}

	var allTextParts []string
	for _, round := range old {
		allTextParts = append(allTextParts, round.Text)
	}
	allText := strings.Join(allTextParts, "\n\n")
	if allText == "" || o.tokenCounts.count(allText) < o.MinCollapseTokens {
		base.Reason = "below_min_tokens"
		base.CollapsedChars = u16len(allText)
		return base, nil
	}

	maxImages := maxInt(0, o.MaxImages)
	if maxImages == 0 {
		base.Reason = "too_many_images"
		base.CollapsedChars = u16len(allText)
		return base, nil
	}

	var runs [][]responsesCompletedRound
	for _, round := range old {
		if n := len(runs); n > 0 {
			run := runs[n-1]
			if round.StartIndex == run[len(run)-1].EndIndex+1 {
				runs[n-1] = append(run, round)
				continue
			}
		}
		runs = append(runs, []responsesCompletedRound{round})
	}

	var segments []responsesPairCollapseSegment
	remainingImages := maxImages
	hitImageCap := false
	for _, run := range runs {
		if remainingImages == 0 {
			hitImageCap = true
			break
		}
		low, high := 0, len(run)+1
		var best *renderedUnits
		for low+1 < high {
			count := (low + high) / 2
			var texts []string
			for _, round := range run[:count] {
				texts = append(texts, round.Text)
			}
			rendered, err := renderUnitTexts(texts, o)
			if err != nil {
				return base, err
			}
			if len(rendered.Images) > 0 && len(rendered.Images) <= remainingImages {
				low = count
				best = &rendered
			} else {
				high = count
			}
		}
		if best == nil || low == 0 {
			hitImageCap = true
			break
		}
		if !isProfitable(best.RenderedText, o.Cols, -1) {
			continue
		}
		selected := run[:low]
		var selectedIndices []int
		for _, round := range selected {
			selectedIndices = append(selectedIndices, round.Indices...)
		}
		sort.Ints(selectedIndices)
		sources := make([]string, len(best.Images))
		for i := range sources {
			sources[i] = best.Source
		}
		segments = append(segments, responsesPairCollapseSegment{
			InsertAt:        selected[0].StartIndex,
			SelectedIndices: selectedIndices,
			Images:          best.Images,
			ImageSources:    sources,
			Text:            best.Source,
			BaselineTokens:  -1,
		})
		remainingImages -= len(best.Images)
		if low < len(run) {
			hitImageCap = true
			break
		}
	}

	if len(segments) == 0 {
		if hitImageCap {
			base.Reason = "too_many_images"
		} else {
			base.Reason = "not_profitable"
		}
		base.CollapsedChars = u16len(allText)
		return base, nil
	}
	return finalizeResponsesPlan(base, segments, old, state, nil, false), nil
}

// ---- Chat Completions lowering ----------------------------------------------

func chatContentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch p["type"] {
			case "text":
				if txt, ok := p["text"].(string); ok {
					parts = append(parts, txt)
				}
			case "image_url", "input_image", "image":
				parts = append(parts, "[image]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func chatMessageToTurn(msg any, idx int) historyTurn {
	o, _ := msg.(map[string]any)
	if o == nil {
		o = map[string]any{}
	}
	role, _ := o["role"].(string)
	body := chatContentToText(o["content"])
	if role == "tool" {
		var closeIds []string
		if id, ok := o["tool_call_id"].(string); ok && id != "" {
			closeIds = []string{id}
		}
		return historyTurn{Text: "[tool_result]\n" + body, CloseIds: closeIds}
	}
	if role == "assistant" {
		var openIds []string
		var parts []string
		if strings.TrimSpace(body) != "" {
			parts = append(parts, body)
		}
		if tc, ok := o["tool_calls"].([]any); ok {
			for _, call := range tc {
				c, _ := call.(map[string]any)
				if c == nil {
					c = map[string]any{}
				}
				if id, ok := c["id"].(string); ok && id != "" {
					openIds = append(openIds, id)
				}
				name := "tool"
				args := ""
				if fn, ok := c["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok {
						name = n
					}
					if a, ok := fn["arguments"].(string); ok {
						args = a
					} else {
						args = safeJSONString(fn["arguments"])
					}
				} else {
					args = safeJSONString(nil)
				}
				parts = append(parts, "[tool_use "+name+"]\n"+args)
			}
		}
		text := strings.Join(parts, "\n")
		full := ""
		if strings.TrimSpace(text) != "" {
			full = "<assistant t=\"" + strconv.Itoa(idx) + "\">\n" + text + "\n</assistant>"
		}
		return historyTurn{Text: full, OpenIds: openIds}
	}
	if strings.TrimSpace(body) == "" {
		return historyTurn{}
	}
	tag := role
	if role == "user" || role == "" {
		tag = "user"
	}
	turn := historyTurn{Text: "<" + tag + " t=\"" + strconv.Itoa(idx) + "\">\n" + body + "\n</" + tag + ">"}
	if role == "user" {
		b := body
		turn.UserText = &b
	}
	return turn
}

func chatMessagesToTurns(messages []any) []historyTurn {
	turns := make([]historyTurn, len(messages))
	for i, msg := range messages {
		turns[i] = chatMessageToTurn(msg, i)
	}
	return turns
}
