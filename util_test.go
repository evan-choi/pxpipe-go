package pxpipe

import "testing"

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
