package pxpipe

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

var (
	envSectionStartReference = regexp.MustCompile(`(?:^|\n)# Environment\b`)
	envSectionEndReference   = regexp.MustCompile(`\n#{1,6}[ \t\n\v\f\r\x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`)
)

func stripMarkdownEnvSectionReference(text string) (kept string, body string) {
	loc := envSectionStartReference.FindStringIndex(text)
	if loc == nil {
		return "", text
	}
	start := loc[0]
	if text[start] == '\n' {
		start++
	}
	end := len(text)
	if loc := envSectionEndReference.FindStringIndex(text[loc[1]:]); loc != nil {
		end = loc[0] + start + len("# Environment")
	}
	return strings.TrimRightFunc(text[start:end], isJSSpace), text[:start] + text[end:]
}

func TestStripMarkdownEnvSection(t *testing.T) {
	tests := []struct {
		name, input, kept, body string
	}{
		{"missing", "prefix\n# Environments\nvalue", "", "prefix\n# Environments\nvalue"},
		{"only heading", "# Environment", "# Environment", ""},
		{"middle", "before\n# Environment\nvalue\n## After\nkept", "# Environment\nvalue", "before\n\n## After\nkept"},
		{"crlf", "before\r\n# Environment\r\nvalue\r\n# After", "# Environment\r\nvalue", "before\r\n\n# After"},
		{"nul boundary", "# Environment\x00value\n# After", "# Environment\x00value", "\n# After"},
		{"unicode boundary and terminator", "# Environment界\n######\u3000After", "# Environment界", "\n######\u3000After"},
		{"seven hashes", "# Environment\nvalue\n####### After", "# Environment\nvalue\n####### After", ""},
		{"ascii word boundary", "# Environment_ignored\n# Environment0_ignored\n# Environment valid", "# Environment valid", "# Environment_ignored\n# Environment0_ignored\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, body := stripMarkdownEnvSection(tt.input)
			if kept != tt.kept || body != tt.body {
				t.Fatalf("got (%q, %q), want (%q, %q)", kept, body, tt.kept, tt.body)
			}
			refKept, refBody := stripMarkdownEnvSectionReference(tt.input)
			if kept != refKept || body != refBody {
				t.Fatalf("reference got (%q, %q), scanner got (%q, %q)", refKept, refBody, kept, body)
			}
		})
	}
}

func TestStripMarkdownEnvSectionRandomParity(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	parts := []string{
		"a", "Z", "0", "_", "#", " ", "\t", "\n", "\r", "\r\n", "\x00",
		"é", "界", "😀", "\u00a0", "\u1680", "\u2007", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000", "\ufeff",
		"# Environment", "\n# Environment", "\n# ", "\n######\t", "\n####### ",
	}
	for i := 0; i < 20_000; i++ {
		var text strings.Builder
		for range rng.Intn(80) {
			text.WriteString(parts[rng.Intn(len(parts))])
		}
		input := text.String()
		gotKept, gotBody := stripMarkdownEnvSection(input)
		wantKept, wantBody := stripMarkdownEnvSectionReference(input)
		if gotKept != wantKept || gotBody != wantBody {
			t.Fatalf("case %d input %q: got (%q, %q), want (%q, %q)", i, input, gotKept, gotBody, wantKept, wantBody)
		}
	}
}
