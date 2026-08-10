package pxpipe

import (
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

func TestInvalidCharsPerTokenUsesCalibratedFallback(t *testing.T) {
	if got := normCpt(0); got != 3 {
		t.Fatalf("normCpt(0) = %v, want 3", got)
	}
}

func TestCompactSlabWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"unchanged", "a\nb", "a\nb"},
		{"trailing spaces", "a \t\nb\t ", "a\nb"},
		{"blank lines", "a\n \t\n\n\nb", "a\n\nb"},
		{"leading newlines", "\n\n\nx", "\n\nx"},
		{"trailing newlines", "a\n\n\n", "a\n\n"},
		{"non ASCII whitespace", "a\r\nb", "a\r\nb"},
		{"only spaces", " \t", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactSlabWhitespace(tc.input); got != tc.want {
				t.Fatalf("compactSlabWhitespace(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestReflowCompactedWithPrefix(t *testing.T) {
	const prefix = "header\n"
	for _, input := range []string{
		"plain",
		"a\nb",
		"a\tb",
		"a \t\n\n\n\tb",
		"한글\ntext",
		"a" + render.NLSentinel + "b",
	} {
		compacted := compactSlabWhitespace(input)
		want := maybeReflow(compacted, true)
		whole, body := reflowCompactedWithPrefix(compacted, prefix)
		if whole != prefix+want || body != want {
			t.Fatalf("reflowCompactedWithPrefix(%q) = %q, %q; want %q, %q", input, whole, body, prefix+want, want)
		}
	}
}

func TestCollapseNewlineRuns(t *testing.T) {
	for _, tc := range []struct {
		input, want string
	}{
		{"a\nb", "a\nb"},
		{"a\n\nb", "a\n\nb"},
		{"a\n\n\nb", "a\n\nb"},
		{"\n\n\n\n", "\n\n"},
		{"a\n\n\nb\n\n\n\nc", "a\n\nb\n\nc"},
	} {
		if got := collapseNewlineRuns(tc.input); got != tc.want {
			t.Fatalf("collapseNewlineRuns(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
