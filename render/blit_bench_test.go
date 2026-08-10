package render

import "testing"

var benchmarkGlyphAdvance int

func BenchmarkBlitGlyphGray(b *testing.B) {
	selected := atlasSet(DefaultRenderFont)
	for _, tc := range []struct {
		name string
		cp   rune
	}{
		{"ascii", 'A'},
		{"wide", '한'},
	} {
		b.Run(tc.name, func(b *testing.B) {
			fb := make([]byte, 64*64)
			b.ReportAllocs()
			for range b.N {
				benchmarkGlyphAdvance = blitGlyphGray(fb, 64, 64, 4, 4, tc.cp, selected, true)
			}
		})
	}
}
