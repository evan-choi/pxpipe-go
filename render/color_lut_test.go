package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func referenceInkColor(paper, g int, ink [3]int) [3]byte {
	cov := 0
	if paper > 0 {
		cov = jsRound(float64((paper-g)*255) / float64(paper))
	}
	if cov < 0 {
		cov = 0
	}
	if cov > 255 {
		cov = 255
	}
	return [3]byte{
		byte(jsRound(float64(paper) - float64(cov*(paper-ink[0]))/255)),
		byte(jsRound(float64(paper) - float64(cov*(paper-ink[1]))/255)),
		byte(jsRound(float64(paper) - float64(cov*(paper-ink[2]))/255)),
	}
}

func TestColorLUTMatchesReference(t *testing.T) {
	for _, tc := range []struct {
		name    string
		palette [][3]int
	}{
		{"cycle", glyphPalette[:]},
		{"role", RolePalette},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for paper := range 256 {
				var lut [len(glyphPalette)][256][3]byte
				fillColorLUT(&lut, tc.palette, paper)
				for slot := range lut {
					ink := tc.palette[slot%len(tc.palette)]
					for g := range 256 {
						if got, want := lut[slot][g], referenceInkColor(paper, g, ink); got != want {
							t.Fatalf("paper=%d gray=%d slot=%d: got %v, want %v", paper, g, slot, got, want)
						}
					}
				}
			}
		})
	}
}

func TestColorRenderParity(t *testing.T) {
	paper := 236
	notInverted := false
	roleSlots := SlotMarkUser + SlotMarkUser + SlotMarkUser + SlotMarkAssistant + SlotMarkAssistant + SlotMarkAssistant
	for _, tc := range []struct {
		name  string
		style RenderStyle
		slots *string
		want  string
	}{
		{"aa-cycle", RenderStyle{AA: true, ColorCycle: true}, nil, "08bd594812efc4fbda1a4311799308cc72233363dc4a582457761ed861a4e265"},
		{"bit-cycle-paper", RenderStyle{ColorCycle: true, PaperGray: &paper}, nil, "b37f6039687bcfa4d6a203d8a5fcea91662cf7d8857000b05f23803677089dd8"},
		{"aa-cycle-not-inverted", RenderStyle{AA: true, ColorCycle: true, Invert: &notInverted}, nil, "a6261d6ab5f21762bb7c6fcf5c7d082b8bd526dc51f5cdd45ba3f1563554ac5e"},
		{"aa-role", RenderStyle{AA: true, ColorByRole: true}, &roleSlots, "9968f3f45201b073512659e2bdb61787b315f2abe5ca95cd630f24a9204444bf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var first []byte
			for range 3 {
				pages, err := renderTextToPngsWithCharLimitUncached("abcdef", 8, ReadableCharsPerImage, tc.style, 80, tc.slots, false)
				if err != nil {
					t.Fatal(err)
				}
				if first == nil {
					first = pages[0].PNG
				} else if !bytes.Equal(pages[0].PNG, first) {
					t.Fatal("PNG output is not deterministic")
				}
			}
			got := sha256.Sum256(decodeToRGBA(t, first).Pix)
			if gotString := hex.EncodeToString(got[:]); gotString == tc.want {
				return
			}
			t.Fatalf("RGBA SHA-256 = %x, want %s", got, tc.want)
		})
	}
}

func TestWriteRoleSlotSegmentMatchesString(t *testing.T) {
	const prefix = "prefix:"
	var out strings.Builder
	out.WriteString(prefix)
	WriteRoleSlotSegment(&out, "user", "body\x01text", SlotMarkUser, ` t="12"`)
	want := strings.Repeat(SlotMarkUser, 13) + "\nbody\x03text\n" + strings.Repeat(SlotMarkUser, 7)
	if got := strings.TrimPrefix(out.String(), prefix); got != want {
		t.Fatalf("WriteRoleSlotSegment() = %q, want %q", got, want)
	}
}
