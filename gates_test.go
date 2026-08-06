package pxpipe

import "testing"

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
