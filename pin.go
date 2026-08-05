package pxpipe

import (
	"regexp"
	"strconv"
	"strings"
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
