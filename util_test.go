package pxpipe

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/evan-choi/pxpipe-go/render"
)

func TestIsASCIIWordScan(t *testing.T) {
	for length := range 18 {
		ascii := strings.Repeat("a", length)
		if !isASCII(ascii) {
			t.Fatalf("isASCII(%q) = false", ascii)
		}
		for i := range length {
			text := []byte(ascii)
			text[i] = 0x80
			if isASCII(string(text)) {
				t.Fatalf("isASCII accepted high byte at %d/%d", i, length)
			}
		}
	}
}

func TestU16LenWordScan(t *testing.T) {
	texts := []string{
		"", strings.Repeat("a", 7), strings.Repeat("a", 8), strings.Repeat("a", 9),
		strings.Repeat("a", 7) + "↵" + strings.Repeat("b", 9),
		strings.Repeat("한글😀", 16), string([]byte{'a', 0xff, 'b'}),
	}
	for offset := range 17 {
		texts = append(texts, strings.Repeat("a", offset)+"😀"+strings.Repeat("b", 16-offset))
	}
	for _, text := range texts {
		if got, want := u16len(text), len(utf16.Encode([]rune(text))); got != want {
			t.Fatalf("u16len(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestU16Slice(t *testing.T) {
	input := "a😀b"
	for _, tc := range []struct {
		name       string
		start, end int
		want       string
	}{
		{"whole", 0, 4, input},
		{"aligned astral", 1, 3, "😀"},
		{"high surrogate", 1, 2, "�"},
		{"low surrogate", 2, 3, "�"},
		{"low surrogate suffix", 2, 4, "�b"},
		{"past end", 3, 99, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := u16Slice(input, tc.start, tc.end); got != tc.want {
				t.Fatalf("u16Slice() = %q, want %q", got, tc.want)
			}
		})
	}

	var got string
	if allocs := testing.AllocsPerRun(100, func() { got = u16Slice(input, 1, 3) }); allocs != 0 {
		t.Fatalf("aligned slice allocated %v times: %q", allocs, got)
	}

	for _, text := range []string{
		strings.Repeat("a", 31),
		strings.Repeat("a", 9) + "한글😀" + strings.Repeat("b", 17),
		string([]byte{'a', 0xff, 'b'}),
	} {
		units := utf16.Encode([]rune(text))
		for start := 0; start <= len(units)+1; start++ {
			for end := start + 1; end <= len(units)+2; end++ {
				clampedEnd := minInt(end, len(units))
				want := ""
				if start < clampedEnd {
					want = string(utf16.Decode(units[start:clampedEnd]))
				}
				if got := u16Slice(text, start, end); got != want {
					t.Fatalf("u16Slice(%q, %d, %d) = %q, want %q", text, start, end, got, want)
				}
			}
		}
	}
}

func TestJoinTextPrefixes(t *testing.T) {
	source, ends := joinTextPrefixes([]string{"한글", "", "tail\n"})
	want := []string{"한글", "한글\n\n", "한글\n\n\n\ntail\n"}
	for i, end := range ends {
		if got := source[:end]; got != want[i] {
			t.Fatalf("prefix %d = %q, want %q", i, got, want[i])
		}
	}
	cleanSource, cleanEnds := joinTextPrefixes([]string{"한글", "middle", "tail\n"})
	packed, packedEnds, ok := reflowedTextPrefixes(cleanSource, cleanEnds, 0)
	if !ok {
		t.Fatal("expected reusable reflowed prefixes")
	}
	for i, end := range cleanEnds {
		want, wantOK := render.Reflow(cleanSource[:end])
		if !wantOK || packed[:packedEnds[i]] != want {
			t.Fatalf("reflowed prefix %d = %q, want %q", i, packed[:packedEnds[i]], want)
		}
	}
	const sourceOffset = 17
	absoluteEnds := append([]int(nil), cleanEnds...)
	for i := range absoluteEnds {
		absoluteEnds[i] += sourceOffset
	}
	offsetPacked, offsetEnds, ok := reflowedTextPrefixes(cleanSource, absoluteEnds, sourceOffset)
	if !ok || offsetPacked != packed || !slices.Equal(offsetEnds, packedEnds) {
		t.Fatalf("offset prefixes = %q, %v, %v; want %q, %v, true", offsetPacked, offsetEnds, ok, packed, packedEnds)
	}
}

func TestReflowedTextPrefixesFallback(t *testing.T) {
	for _, text := range []string{"tab\there", "trailing ", "too\n\n\n\nmany", render.NLSentinel} {
		if _, _, ok := reflowedTextPrefixes(text, []int{len(text)}, 0); ok {
			t.Fatalf("reflowedTextPrefixes(%q) unexpectedly reusable", text)
		}
	}
}

func TestClassifyResponsesPairsBuildsSharedText(t *testing.T) {
	items := []any{
		map[string]any{"type": "function_call", "call_id": "a", "name": "first", "arguments": map[string]any{"x": json.Number("1")}},
		map[string]any{"type": "function_call", "call_id": "b", "name": "second", "arguments": "raw-b"},
		map[string]any{"type": "function_call_output", "call_id": "b", "output": []any{"b"}},
		map[string]any{"type": "function_call_output", "call_id": "a", "output": map[string]any{"ok": true}},
	}
	want := "[tool_use second]\nraw-b\n[tool_result]\n[\"b\"]\n\n" +
		"[tool_use first]\n{\"x\":1}\n[tool_result]\n{\"ok\":true}"
	old, text, state := classifyResponsesPairs(items, 0, nil)
	if text != want {
		t.Fatalf("completed text = %q, want %q", text, want)
	}
	if len(old) != 1 || old[0].Text != want || len(old[0].Pairs) != 2 {
		t.Fatalf("completed rounds = %#v, want one two-pair round", old)
	}
	if state.CompletedPairs != 2 || state.OldCompletedPairs != 2 {
		t.Fatalf("pair state = %+v, want two old completed pairs", state)
	}
}

func TestClassifyResponsesPairsCountsInvalidItems(t *testing.T) {
	items := []any{
		map[string]any{"type": "function_call", "call_id": "duplicate"},
		map[string]any{"type": "function_call", "call_id": "duplicate"},
		map[string]any{"type": "function_call_output", "call_id": "duplicate"},
		map[string]any{"type": "function_call", "call_id": "open"},
		map[string]any{"type": "function_call_output", "call_id": "orphan"},
		map[string]any{"type": "function_call"},
	}
	old, text, state := classifyResponsesPairs(items, 0, nil)
	if len(old) != 0 || text != "" {
		t.Fatalf("invalid pairs produced completed output: %#v, %q", old, text)
	}
	if state.OpenCalls != 1 || state.OrphanOutputs != 1 || state.MalformedItems != 4 {
		t.Fatalf("invalid pair state = %+v", state)
	}
}

func TestHistoryImageSha8ConcatSemantics(t *testing.T) {
	imageBlock := func(data string) map[string]any {
		return map[string]any{"type": "image", "source": map[string]any{"data": data}}
	}
	content := []any{
		textBlock(HistorySyntheticIntro),
		imageBlock("ab"),
		map[string]any{"type": "image", "source": map[string]any{}},
		imageBlock("c"),
	}
	messages := []any{
		map[string]any{"role": "user", "content": content},
		map[string]any{"role": "user", "content": []any{imageBlock("ignored")}},
	}
	if got := historyImageSha8(messages); got != "ba7816bf" {
		t.Fatalf("historyImageSha8() = %q, want SHA-256 prefix of delimiter-free abc", got)
	}

	content[1] = imageBlock("a")
	content[3] = imageBlock("bc")
	if got := historyImageSha8(messages); got != "ba7816bf" {
		t.Fatalf("historyImageSha8() changed across equivalent concatenation: %q", got)
	}

	if got := historyImageSha8([]any{map[string]any{"content": []any{imageBlock("")}}}); got != "" {
		t.Fatalf("historyImageSha8(empty data) = %q, want empty", got)
	}
}

func TestHistoryImageSha8RawChunkBoundary(t *testing.T) {
	first := bytes.Repeat([]byte{0xfb}, (12<<10)+5)
	second := []byte{0, 1, 2, 3}
	messages := []any{map[string]any{"content": []any{
		makeImageBlock(first), makeImageBlock(second),
	}}}
	want := sha8(base64.StdEncoding.EncodeToString(first) + base64.StdEncoding.EncodeToString(second))
	if got := historyImageSha8(messages); got != want {
		t.Fatalf("historyImageSha8(chunked raw PNGs) = %q, want %q", got, want)
	}
}
