package pxpipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// User pin folding: `@pxpipe pin <text>` / `@pxpipe unpin` commands are folded
// from the transcript, stripped from the outbound copy, and re-emitted as a
// single block at the request tail (after every cache breakpoint).

const (
	pinMaxChars      = 300
	pinTotalMaxChars = 2000
	pinReplyMark     = "@pxpipe "
	reminderOpen     = "<system-reminder>"
	reminderClose    = "</system-reminder>"
)

var (
	pinCmdRe          = regexp.MustCompile(`^@pxpipe[ \t]+(pin|unpin)\b(.*)$`)
	leadingReminderRe = regexp.MustCompile(`(?s)^\s*<system-reminder>.*?</system-reminder>`)
	filePathOfRe      = regexp.MustCompile(`(?m)^Contents of (.+?)(?: \(|:\s*$)`)
)

type pinSource string

const (
	pinSourceFile    pinSource = "file"
	pinSourceSession pinSource = "session"
)

type pin struct {
	text   string
	source pinSource
	path   string
}

type pinLine struct {
	line   string
	source pinSource
	path   string
}

func matchPinCmd(line string) (verb, rest string, ok bool) {
	m := pinCmdRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

func stripReminders(text string) string {
	var b strings.Builder
	at := 0
	for {
		open := strings.Index(text[at:], reminderOpen)
		if open < 0 {
			break
		}
		open += at
		close_ := strings.Index(text[open+len(reminderOpen):], reminderClose)
		if close_ < 0 {
			break
		}
		close_ += open + len(reminderOpen)
		b.WriteString(text[at:open])
		at = close_ + len(reminderClose)
	}
	return b.String() + text[at:]
}

func foldPins(messages []any, system any) []pin {
	var pins []pin
	for _, entry := range systemPinLines(system) {
		applyPinLine(&pins, entry)
	}
	for idx, mv := range messages {
		m, ok := asMap(mv)
		if !ok {
			continue
		}
		if role, _ := getStr(m, "role"); role != "user" {
			continue
		}
		for _, entry := range messagePinLines(m, idx) {
			applyPinLine(&pins, entry)
		}
	}
	return pins
}

func applyPinLine(pins *[]pin, entry pinLine) {
	verb, rest, ok := matchPinCmd(entry.line)
	if !ok {
		return
	}
	if verb == "unpin" {
		applyUnpin(pins, rest)
		return
	}
	text := rest
	if u16len(text) > pinMaxChars {
		text = u16Slice(text, 0, pinMaxChars) + "… [pxpipe: pin truncated]"
	}
	if entry.source == pinSourceFile {
		if text == "" {
			if len(*pins) == 0 || (*pins)[len(*pins)-1].text == "" {
				return
			}
		}
		*pins = append(*pins, pin{text: text, source: entry.source, path: entry.path})
		return
	}
	if text == "" {
		return
	}
	for _, p := range *pins {
		if p.text == text {
			return
		}
	}
	*pins = append(*pins, pin{text: text, source: pinSourceSession})
}

func applyUnpin(pins *[]pin, arg string) {
	if arg == "" {
		return
	}
	if arg == "all" {
		out := (*pins)[:0]
		for _, p := range *pins {
			if p.source != pinSourceSession {
				out = append(out, p)
			}
		}
		*pins = out
		return
	}
	var session []int
	for i, p := range *pins {
		if p.source == pinSourceSession {
			session = append(session, i)
		}
	}
	target := -1
	if n, err := strconv.Atoi(arg); err == nil && regexp.MustCompile(`^\d+$`).MatchString(arg) {
		if n >= 1 && n <= len(session) {
			target = session[n-1]
		}
	} else {
		lower := strings.ToLower(arg)
		for _, i := range session {
			if (*pins)[i].text == arg {
				target = i
				break
			}
		}
		if target < 0 {
			for _, i := range session {
				if strings.HasPrefix(strings.ToLower((*pins)[i].text), lower) {
					target = i
					break
				}
			}
		}
	}
	if target >= 0 {
		*pins = append((*pins)[:target], (*pins)[target+1:]...)
	}
}

func contentBlocks(content any) []any {
	if s, ok := content.(string); ok {
		return []any{textBlock(s)}
	}
	if a, ok := asArr(content); ok {
		return a
	}
	return nil
}

func messagePinLines(m map[string]any, idx int) []pinLine {
	var out []pinLine
	leadingRun := idx == 0
	for _, bv := range contentBlocks(m["content"]) {
		blk, ok := asMap(bv)
		if !ok || blockType(bv) != "text" {
			continue
		}
		text, ok := getStr(blk, "text")
		if !ok {
			continue
		}
		if leadingReminderRe.MatchString(text) {
			source := pinSourceSession
			if leadingRun {
				source = pinSourceFile
			}
			consumed := 0
			for _, block := range reminderBlocks(text) {
				consumed += len(block)
				path := ""
				if source == pinSourceFile {
					path = filePathOf(block)
				}
				for _, line := range strings.Split(block, "\n") {
					out = append(out, pinLine{line: line, source: source, path: path})
				}
			}
			rest := text[consumed:]
			if strings.TrimSpace(rest) != "" {
				leadingRun = false
				for _, line := range strings.Split(rest, "\n") {
					out = append(out, pinLine{line: line, source: pinSourceSession})
				}
			}
			continue
		}
		leadingRun = false
		for _, line := range strings.Split(text, "\n") {
			out = append(out, pinLine{line: line, source: pinSourceSession})
		}
	}
	return out
}

func systemPinLines(sys any) []pinLine {
	if sys == nil {
		return nil
	}
	var out []pinLine
	for _, bv := range contentBlocks(sys) {
		blk, ok := asMap(bv)
		if !ok || blockType(bv) != "text" {
			continue
		}
		text, ok := getStr(blk, "text")
		if !ok {
			continue
		}
		path := filePathOf(text)
		for _, line := range strings.Split(text, "\n") {
			out = append(out, pinLine{line: line, source: pinSourceFile, path: path})
		}
	}
	return out
}

func reminderBlocks(text string) []string {
	var out []string
	rest := text
	for {
		m := leadingReminderRe.FindString(rest)
		if m == "" {
			break
		}
		out = append(out, m)
		rest = rest[len(m):]
	}
	return out
}

func filePathOf(block string) string {
	if !strings.Contains(block, "Contents of ") {
		return ""
	}
	m := filePathOfRe.FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func isCommandOnlyTurn(m map[string]any, live bool) bool {
	if role, _ := getStr(m, "role"); role != "user" {
		return false
	}
	blocks := contentBlocks(m["content"])
	if blocks == nil {
		return false
	}
	sawCommand := false
	for _, bv := range blocks {
		blk, ok := asMap(bv)
		if !ok {
			return false
		}
		if blockType(bv) != "text" {
			return false
		}
		raw, ok := getStr(blk, "text")
		if !ok {
			return false
		}
		if !live && leadingReminderRe.MatchString(raw) {
			return false
		}
		text := stripReminders(raw)
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if _, _, isCmd := matchPinCmd(line); !isCmd {
				return false
			}
			sawCommand = true
		}
	}
	return sawCommand
}

func isPinReply(mv any) bool {
	m, ok := asMap(mv)
	if !ok {
		return false
	}
	if role, _ := getStr(m, "role"); role != "assistant" {
		return false
	}
	if s, ok := m["content"].(string); ok {
		return strings.HasPrefix(s, pinReplyMark)
	}
	blocks, ok := asArr(m["content"])
	if !ok || len(blocks) != 1 {
		return false
	}
	blk, ok := asMap(blocks[0])
	if !ok || blockType(blocks[0]) != "text" {
		return false
	}
	t, ok := getStr(blk, "text")
	return ok && strings.HasPrefix(t, pinReplyMark)
}

func stripPinCommands(messages []any) ([]any, bool) {
	var out []any
	changed := false
	for i := 0; i < len(messages); i++ {
		mv := messages[i]
		m, isMap := asMap(mv)
		if isMap && isCommandOnlyTurn(m, false) {
			var next any
			if i+1 < len(messages) {
				next = messages[i+1]
			}
			if isPinReply(next) {
				i++
				changed = true
				continue
			}
		}
		if !isMap {
			out = append(out, mv)
			continue
		}
		if role, _ := getStr(m, "role"); role != "user" {
			out = append(out, mv)
			continue
		}
		stripped, state := stripFromMessage(m)
		switch state {
		case stripDroppedAll:
			var nextRole string
			if i+1 < len(messages) {
				if nm, ok := asMap(messages[i+1]); ok {
					nextRole, _ = getStr(nm, "role")
				}
			}
			if nextRole == "assistant" {
				i++
				changed = true
				continue
			}
			out = append(out, mv)
		case stripChanged:
			out = append(out, stripped)
			changed = true
		default:
			out = append(out, mv)
		}
	}
	return out, changed
}

type stripState int

const (
	stripUnchanged stripState = iota
	stripChanged
	stripDroppedAll
)

func stripFromMessage(m map[string]any) (map[string]any, stripState) {
	if s, ok := m["content"].(string); ok {
		text := stripLines(s)
		if text == s {
			return nil, stripUnchanged
		}
		if strings.TrimSpace(text) == "" {
			return nil, stripDroppedAll
		}
		out := cloneMap(m)
		out["content"] = text
		return out, stripChanged
	}
	arr, ok := asArr(m["content"])
	if !ok {
		return nil, stripUnchanged
	}
	changed := false
	var blocks []any
	for _, bv := range arr {
		blk, isMap := asMap(bv)
		if isMap && blockType(bv) == "text" {
			if tbText, isStr := getStr(blk, "text"); isStr {
				text := stripLines(tbText)
				if text != tbText {
					changed = true
					if strings.TrimSpace(text) == "" {
						cc, hasCC := blk["cache_control"]
						if hasCC && len(blocks) > 0 {
							if prev, prevOK := asMap(blocks[len(blocks)-1]); prevOK {
								np := cloneMap(prev)
								np["cache_control"] = cc
								blocks[len(blocks)-1] = np
							} else {
								blocks = append(blocks, bv)
							}
						} else if hasCC {
							blocks = append(blocks, bv)
						}
						continue
					}
					nb := cloneMap(blk)
					nb["text"] = text
					blocks = append(blocks, nb)
					continue
				}
			}
		}
		blocks = append(blocks, bv)
	}
	if !changed {
		return nil, stripUnchanged
	}
	if len(blocks) == 0 {
		return nil, stripDroppedAll
	}
	out := cloneMap(m)
	out["content"] = blocks
	return out, stripChanged
}

// stripPinCommandsFromSystem returns (newSystem, changed).
func stripPinCommandsFromSystem(sys any) (any, bool) {
	if sys == nil {
		return nil, false
	}
	if s, ok := sys.(string); ok {
		text := stripLines(s)
		if text == s {
			return nil, false
		}
		return text, true
	}
	arr, ok := asArr(sys)
	if !ok {
		return nil, false
	}
	changed := false
	var out []any
	for _, bv := range arr {
		blk, isMap := asMap(bv)
		if isMap && blockType(bv) == "text" {
			if tbText, isStr := getStr(blk, "text"); isStr {
				text := stripLines(tbText)
				if text != tbText {
					changed = true
					if strings.TrimSpace(text) == "" {
						cc, hasCC := blk["cache_control"]
						if hasCC && len(out) > 0 && blockType(out[len(out)-1]) == "text" {
							if prev, prevOK := asMap(out[len(out)-1]); prevOK {
								np := cloneMap(prev)
								np["cache_control"] = cc
								out[len(out)-1] = np
							}
						} else if hasCC {
							out = append(out, bv)
						}
						continue
					}
					nb := cloneMap(blk)
					nb["text"] = text
					out = append(out, nb)
					continue
				}
			}
		}
		out = append(out, bv)
	}
	if !changed {
		return nil, false
	}
	return out, true
}

func stripLines(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	changed := false
	for _, line := range lines {
		if _, _, isCmd := matchPinCmd(line); isCmd {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		return text
	}
	return strings.Join(kept, "\n")
}

func pinBlockText(pins []pin) string {
	if len(pins) == 0 {
		return ""
	}
	budget := pinTotalMaxChars
	var lines []string
	for _, p := range pins {
		tl := u16len(p.text)
		if budget-tl < 0 {
			continue
		}
		budget -= tl
		if p.source == pinSourceFile {
			lines = append(lines, p.text)
		} else {
			lines = append(lines, "- "+p.text)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	parts := []string{
		"<system-reminder>",
		"[pxpipe pin] The user pinned these instructions and pxpipe relocated them here,",
		"last in the request, because rules stated far above get read as background. They",
		"are the user's own words and they govern this reply; on conflict they win.",
	}
	parts = append(parts, lines...)
	parts = append(parts, "</system-reminder>")
	return strings.Join(parts, "\n")
}

func canAppendPinBlock(messages []any) bool {
	if len(messages) == 0 {
		return false
	}
	last, ok := asMap(messages[len(messages)-1])
	if !ok {
		return false
	}
	if role, _ := getStr(last, "role"); role != "user" {
		return false
	}
	switch last["content"].(type) {
	case string, []any:
		return true
	}
	return false
}

func appendPinBlock(messages []any, pins []pin) int {
	text := pinBlockText(pins)
	if text == "" {
		return 0
	}
	last, ok := asMap(messages[len(messages)-1])
	if !ok {
		return 0
	}
	if role, _ := getStr(last, "role"); role != "user" {
		return 0
	}
	if s, isStr := last["content"].(string); isStr {
		last["content"] = []any{textBlock(s)}
	}
	arr, ok := asArr(last["content"])
	if !ok {
		return 0
	}
	last["content"] = append(arr, textBlock(text))
	return u16len(text)
}

type pinVerb string

const (
	pinVerbPin   pinVerb = "pin"
	pinVerbUnpin pinVerb = "unpin"
)

type pinFileGroup struct {
	path string
	pins []pin
}

func pinFileGroups(pins []pin) []pinFileGroup {
	var groups []pinFileGroup
	for _, p := range pins {
		if p.source != pinSourceFile {
			continue
		}
		path := p.path
		if path == "" {
			path = "system instructions"
		}
		if len(groups) > 0 && groups[len(groups)-1].path == path {
			groups[len(groups)-1].pins = append(groups[len(groups)-1].pins, p)
		} else {
			groups = append(groups, pinFileGroup{path: path, pins: []pin{p}})
		}
	}
	return groups
}

func pinReplyText(pins []pin, verb pinVerb) string {
	if verb == pinVerbUnpin {
		var session []pin
		for _, p := range pins {
			if p.source == pinSourceSession {
				session = append(session, p)
			}
		}
		if len(session) == 0 {
			fromFile := ""
			if len(pins) > 0 {
				groups := pinFileGroups(pins)
				where := fmt.Sprintf("%d files", len(groups))
				if len(groups) == 1 {
					where = groups[0].path
				}
				line := "lines"
				come := "come"
				if len(pins) == 1 {
					line = "line"
					come = "comes"
				}
				fromFile = fmt.Sprintf("\n  %d pinned %s %s from %s   (edit the file to change these)", len(pins), line, come, where)
			}
			return pinReplyMark + "nothing to unpin" + fromFile
		}
		lines := []string{"session   (@pxpipe unpin <n>, or unpin all)", ""}
		for i, p := range session {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, p.text))
		}
		return fmt.Sprintf("%s%d removable\n%s", pinReplyMark, len(session), strings.Join(lines, "\n"))
	}
	if len(pins) == 0 {
		return pinReplyMark + "nothing pinned\n  @pxpipe pin <instruction>"
	}
	var out []string
	for _, group := range pinFileGroups(pins) {
		out = append(out, "", group.path+"   (edit the file to change these)", "")
		for _, p := range group.pins {
			out = append(out, p.text)
		}
	}
	var session []pin
	for _, p := range pins {
		if p.source == pinSourceSession {
			session = append(session, p)
		}
	}
	if len(session) > 0 {
		out = append(out, "", "session   (@pxpipe unpin <n>, or unpin all)", "")
		for i, p := range session {
			out = append(out, fmt.Sprintf("%d. %s", i+1, p.text))
		}
	}
	return fmt.Sprintf("%s%d pinned\n%s", pinReplyMark, len(pins), strings.Join(out[1:], "\n"))
}

func livePinTurn(messages []any) (map[string]any, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := asMap(messages[i])
		if !ok {
			return nil, false
		}
		if role, _ := getStr(m, "role"); role == "system" {
			continue
		}
		return m, true
	}
	return nil, false
}

func isPinOnlyRequest(messages []any) bool {
	live, ok := livePinTurn(messages)
	return ok && isCommandOnlyTurn(live, true)
}

func livePinVerb(m map[string]any) pinVerb {
	verb := pinVerbPin
	for _, bv := range contentBlocks(m["content"]) {
		blk, ok := asMap(bv)
		if !ok {
			continue
		}
		text, ok := getStr(blk, "text")
		if !ok {
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			cmd, _, ok := matchPinCmd(line)
			if ok {
				verb = pinVerb(cmd)
			}
		}
	}
	return verb
}

type pinCommandReply struct {
	body        []byte
	contentType string
}

func pinCommandResponse(body []byte) *pinCommandReply {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	messages, ok := asArr(req["messages"])
	if !ok || !isPinOnlyRequest(messages) {
		return nil
	}
	live, _ := livePinTurn(messages)
	model, ok := getStr(req, "model")
	if !ok {
		model = "pxpipe"
	}
	stream, _ := req["stream"].(bool)
	return synthesizePinReply(pinReplyText(foldPins(messages, req["system"]), livePinVerb(live)), model, stream)
}

func openAITextBlocks(content any) []any {
	if text, ok := content.(string); ok {
		return []any{textBlock(text)}
	}
	parts, ok := asArr(content)
	if !ok {
		return nil
	}
	var blocks []any
	for _, raw := range parts {
		part, ok := asMap(raw)
		if !ok {
			continue
		}
		text, ok := getStr(part, "text")
		if !ok {
			continue
		}
		type_, hasType := getStr(part, "type")
		if !hasType || type_ == "text" || type_ == "input_text" || type_ == "output_text" {
			blocks = append(blocks, textBlock(text))
		}
	}
	return blocks
}

func normalizeOpenAIRequest(req map[string]any) ([]any, any, bool) {
	var items []any
	if input, ok := asArr(req["input"]); ok {
		items = input
	} else if messages, ok := asArr(req["messages"]); ok {
		items = messages
	} else if input, ok := req["input"].(string); ok {
		items = []any{map[string]any{"role": "user", "content": input}}
	} else {
		return nil, nil, false
	}
	var system []any
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		system = append(system, textBlock(instructions))
	}
	var messages []any
	for _, raw := range items {
		item, ok := asMap(raw)
		if !ok {
			return nil, nil, false
		}
		if value, hasType := item["type"]; hasType {
			type_, ok := value.(string)
			if !ok || type_ != "message" {
				messages = append(messages, map[string]any{"role": "assistant", "content": []any{}})
				continue
			}
		}
		blocks := openAITextBlocks(item["content"])
		role, _ := getStr(item, "role")
		if role == "system" || role == "developer" {
			system = append(system, blocks...)
			continue
		}
		if role != "user" {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}
	if len(system) == 0 {
		return messages, nil, true
	}
	return messages, system, true
}

func pinCommandResponseOpenAI(body []byte, wire Protocol) *pinCommandReply {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	messages, system, ok := normalizeOpenAIRequest(req)
	if !ok || !isPinOnlyRequest(messages) {
		return nil
	}
	model, ok := getStr(req, "model")
	if !ok {
		model = "pxpipe"
	}
	stream, _ := req["stream"].(bool)
	text := pinReplyText(foldPins(messages, system), livePinVerb(messages[len(messages)-1].(map[string]any)))
	switch wire {
	case ProtocolOpenAIChat:
		return synthesizeChatPinReply(text, model, stream)
	case ProtocolOpenAIResponses:
		return synthesizeResponsesPinReply(text, model, stream)
	default:
		return nil
	}
}

func marshalPinReply(v any) []byte {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(body.Bytes(), []byte{'\n'})
}

func appendPinSSEEvent(b *strings.Builder, event string, data any) {
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteString("\ndata: ")
	b.Write(marshalPinReply(data))
	b.WriteString("\n\n")
}

func synthesizePinReply(text, model string, stream bool) *pinCommandReply {
	id := "msg_pxpipe_pin_" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if !stream {
		return &pinCommandReply{
			contentType: "application/json",
			body: marshalPinReply(map[string]any{
				"id": id, "type": "message", "role": "assistant", "model": model,
				"content":     []any{map[string]any{"type": "text", "text": text}},
				"stop_reason": "end_turn", "stop_sequence": nil, "usage": usage,
			}),
		}
	}
	var body strings.Builder
	appendPinSSEEvent(&body, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage,
		},
	})
	appendPinSSEEvent(&body, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	appendPinSSEEvent(&body, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	appendPinSSEEvent(&body, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	appendPinSSEEvent(&body, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0},
	})
	appendPinSSEEvent(&body, "message_stop", map[string]any{"type": "message_stop"})
	return &pinCommandReply{body: []byte(body.String()), contentType: "text/event-stream"}
}

func appendPinSSEData(b *strings.Builder, data any) {
	b.WriteString("data: ")
	b.Write(marshalPinReply(data))
	b.WriteString("\n\n")
}

func synthesizeChatPinReply(text, model string, stream bool) *pinCommandReply {
	now := time.Now()
	id := "chatcmpl_pxpipe_pin_" + strconv.FormatInt(now.UnixMilli(), 36)
	created := now.Unix()
	if !stream {
		return &pinCommandReply{
			contentType: "application/json",
			body: marshalPinReply(map[string]any{
				"id": id, "object": "chat.completion", "created": created, "model": model,
				"choices": []any{map[string]any{
					"index": 0, "message": map[string]any{"role": "assistant", "content": text},
					"finish_reason": "stop", "logprobs": nil,
				}},
				"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
			}),
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	var body strings.Builder
	appendPinSSEData(&body, chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	appendPinSSEData(&body, chunk(map[string]any{"content": text}, nil))
	appendPinSSEData(&body, chunk(map[string]any{}, "stop"))
	body.WriteString("data: [DONE]\n\n")
	return &pinCommandReply{body: []byte(body.String()), contentType: "text/event-stream"}
}

func synthesizeResponsesPinReply(text, model string, stream bool) *pinCommandReply {
	now := time.Now()
	stamp := strconv.FormatInt(now.UnixMilli(), 36)
	id := "resp_pxpipe_pin_" + stamp
	itemID := "msg_pxpipe_pin_" + stamp
	created := now.Unix()
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	item := map[string]any{
		"id": itemID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	response := func(status string, output []any) map[string]any {
		var responseUsage any
		if status == "completed" {
			responseUsage = usage
		}
		return map[string]any{
			"id": id, "object": "response", "created_at": created, "status": status, "model": model,
			"output": output, "error": nil, "incomplete_details": nil, "usage": responseUsage,
		}
	}
	if !stream {
		return &pinCommandReply{
			body:        marshalPinReply(response("completed", []any{item})),
			contentType: "application/json",
		}
	}
	sequence := 0
	var body strings.Builder
	event := func(type_ string, data map[string]any) {
		payload := map[string]any{"type": type_, "sequence_number": sequence}
		sequence++
		for key, value := range data {
			payload[key] = value
		}
		appendPinSSEEvent(&body, type_, payload)
	}
	part := func(text string) map[string]any {
		return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	}
	event("response.created", map[string]any{"response": response("in_progress", []any{})})
	event("response.in_progress", map[string]any{"response": response("in_progress", []any{})})
	event("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{},
		},
	})
	event("response.content_part.added", map[string]any{
		"item_id": itemID, "output_index": 0, "content_index": 0, "part": part(""),
	})
	event("response.output_text.delta", map[string]any{
		"item_id": itemID, "output_index": 0, "content_index": 0, "delta": text,
	})
	event("response.output_text.done", map[string]any{
		"item_id": itemID, "output_index": 0, "content_index": 0, "text": text,
	})
	event("response.content_part.done", map[string]any{
		"item_id": itemID, "output_index": 0, "content_index": 0, "part": part(text),
	})
	event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	event("response.completed", map[string]any{"response": response("completed", []any{item})})
	return &pinCommandReply{body: []byte(body.String()), contentType: "text/event-stream"}
}
