// Package render ports pxpipe's text→PNG renderer: it wraps text onto a
// fixed-pitch glyph grid (Spleen 5x8 or JetBrains Mono atlases) and encodes
// dense grayscale/RGB PNG pages sized for Anthropic's vision tiling.
package render

import (
	"math"
	"strings"

	"github.com/evan-choi/pxpipe-go/internal/atlas"
)

const (
	DefaultRenderFont = "spleen-5x8"

	// MaxHeightPx keeps pages under the API downscale bounds (long edge ≤1568,
	// ~1.15MP): 1568×728 bills exactly ⌈w/28⌉×⌈h/28⌉ patches with no resample.
	MaxHeightPx               = 728
	ReadableCharsPerImage     = 28080
	DenseContentCharsPerImage = 28080
	DenseContentCols          = 312
	AnthropicSlabCols         = DenseContentCols
	defaultCols               = AnthropicSlabCols
	PadX                      = 4
	PadY                      = 4
	DefaultCellWBonus         = 0
	DefaultCellHBonus         = 0
	CellW                     = 5 + DefaultCellWBonus
	CellH                     = 8 + DefaultCellHBonus

	tabWidth = 4

	NLSentinel        = "↵"
	nlSentinelCp      = 0x21b5
	NLSentinelLiteral = "⏎"

	GlyphEscapeOpen  = "[U+"
	GlyphEscapeClose = "]"

	SlotMarkUser      = "\x01"
	SlotMarkAssistant = "\x02"
	slotNeutral       = "\x03"

	roleSlotUser      = 1
	roleSlotAssistant = 2

	gridInk = 25
)

type RenderStyle struct {
	Font          string `json:"font,omitempty"`
	Grid          bool   `json:"grid,omitempty"`
	GridCols      int    `json:"gridCols,omitempty"`
	MarkerScale   int    `json:"markerScale,omitempty"`
	MarkerRed     bool   `json:"markerRed,omitempty"`
	CellHBonus    int    `json:"cellHBonus,omitempty"`
	CellWBonus    int    `json:"cellWBonus,omitempty"`
	AA            bool   `json:"aa,omitempty"`
	ColorCycle    bool   `json:"colorCycle,omitempty"`
	ColorByRole   bool   `json:"colorByRole,omitempty"`
	InkDilate     int    `json:"inkDilate,omitempty"`
	InkDilateAxis string `json:"inkDilateAxis,omitempty"`
	// Invert defaults to true (black ink on white); nil = default.
	Invert *bool `json:"invert,omitempty"`
	// PaperGray defaults to 255 (pure white); nil = default.
	PaperGray *int `json:"paperGray,omitempty"`
}

var DenseRenderStyle = RenderStyle{AA: true}

type RenderedImage struct {
	PNG               []byte
	Width             int
	Height            int
	CharsRendered     int
	DroppedChars      int
	DroppedCodepoints map[rune]int
}

var glyphPalette = [][3]int{
	{20, 20, 20},
	{20, 40, 160},
	{150, 20, 20},
	{20, 110, 40},
}

var RolePalette = [][3]int{
	{150, 20, 20},
	{20, 40, 160},
}

func atlasSet(font string) *atlas.Set { return atlas.ForFont(font) }

func styleAtlas(style RenderStyle) *atlas.Atlas {
	if style.AA {
		return atlasSet(style.Font).Gray
	}
	return atlasSet(style.Font).Bit
}

type glyphRef struct {
	atlas *atlas.Atlas
	rank  int
}

func bitGlyph(cp rune, font string) (glyphRef, bool) {
	selected := atlasSet(font).Bit
	if r := selected.Rank(cp); r >= 0 {
		return glyphRef{selected, r}, true
	}
	if def := atlas.Default().Bit; selected != def {
		if r := def.Rank(cp); r >= 0 {
			return glyphRef{def, r}, true
		}
	}
	return glyphRef{}, false
}

func grayGlyph(cp rune, font string) (glyphRef, bool) {
	selected := atlasSet(font).Gray
	if r := selected.Rank(cp); r >= 0 {
		return glyphRef{selected, r}, true
	}
	if def := atlas.Default().Gray; selected != def {
		if r := def.Rank(cp); r >= 0 {
			return glyphRef{def, r}, true
		}
	}
	return glyphRef{}, false
}

func RenderCellWidth(style RenderStyle) int {
	w := styleAtlas(style).CellW + style.CellWBonus
	if w < 1 {
		return 1
	}
	return w
}

func RenderCellHeight(style RenderStyle) int {
	bonus := style.CellHBonus
	if bonus < 0 {
		bonus = 0
	}
	return styleAtlas(style).CellH + bonus
}

// cellsFor mirrors TS cellsFor: visual width of a codepoint in cells
// (missing codepoints advance 1 so wrap math stays stable).
func cellsFor(cp rune, markerScale int, font string) int {
	if cp == nlSentinelCp && markerScale > 1 {
		return markerScale
	}
	g, ok := bitGlyph(cp, font)
	if !ok {
		return 1
	}
	if g.atlas.Wide[g.rank] == 1 {
		return 2
	}
	return 1
}

func trimLineEnd(line string) string {
	end := len(line)
	for end > 0 {
		c := line[end-1]
		if c != ' ' && c != '\t' {
			break
		}
		end--
	}
	return line[:end]
}

// MinifyForRender strips trailing whitespace per line and collapses 4+
// consecutive newlines to 3.
func MinifyForRender(text string) string {
	newlineRun := 0
	needsRewrite := false
	for i := 0; i < len(text); i++ {
		if text[i] != '\n' {
			newlineRun = 0
			continue
		}
		newlineRun++
		if newlineRun > 3 || i > 0 && (text[i-1] == ' ' || text[i-1] == '\t') {
			needsRewrite = true
			break
		}
	}
	if !needsRewrite && len(text) > 0 {
		last := text[len(text)-1]
		needsRewrite = last == ' ' || last == '\t'
	}
	if !needsRewrite {
		return text
	}

	var b strings.Builder
	b.Grow(len(text))
	start := 0
	newlineRun = 0
	for {
		relEnd := strings.IndexByte(text[start:], '\n')
		if relEnd < 0 {
			b.WriteString(trimLineEnd(text[start:]))
			break
		}
		end := start + relEnd
		line := trimLineEnd(text[start:end])
		if len(line) > 0 {
			b.WriteString(line)
			newlineRun = 0
		}
		newlineRun++
		if newlineRun <= 3 {
			b.WriteByte('\n')
		}
		start = end + 1
	}
	return b.String()
}

func NeutralizeSentinel(text string) string {
	if !strings.Contains(text, NLSentinel) {
		return text
	}
	return strings.ReplaceAll(text, NLSentinel, NLSentinelLiteral)
}

func slotMarkRe(r rune) bool { return r == 0x01 || r == 0x02 }

func SlotCopyBody(body string) string {
	if !strings.ContainsAny(body, SlotMarkUser+SlotMarkAssistant) {
		return body
	}
	var b strings.Builder
	b.Grow(len(body))
	for _, r := range body {
		if slotMarkRe(r) {
			b.WriteString(slotNeutral)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RoleSlotSegment builds the width-preserving slot-string segment for one
// role-wrapped turn `<tag attr>\nbody\n</tag>`; marker runs must cover the
// attr too since coloring is positional.
func RoleSlotSegment(tag, body, mark, attr string) string {
	open := "<" + tag + attr + ">"
	close := "</" + tag + ">"
	return strings.Repeat(mark, utf16Len(open)) + "\n" + SlotCopyBody(body) + "\n" + strings.Repeat(mark, utf16Len(close))
}

func slotForMarkCp(cp rune, ok bool) int {
	if !ok {
		return 0
	}
	switch cp {
	case 0x0001:
		return roleSlotUser
	case 0x0002:
		return roleSlotAssistant
	}
	return 0
}

// Reflow minifies, expands tabs, then joins hard newlines with the ↵ sentinel.
// Returns ok=false when the text already contains ↵ (caller renders raw).
func Reflow(text string) (string, bool) {
	if strings.Contains(text, NLSentinel) {
		return "", false
	}
	lines := strings.Split(MinifyForRender(text), "\n")
	for i, l := range lines {
		lines[i] = ExpandTabsInLine(l)
	}
	return strings.Join(lines, NLSentinel), true
}

func Dereflow(reflowed string) string {
	return strings.ReplaceAll(reflowed, NLSentinel, "\n")
}

func isEscapeExempt(cp rune) bool {
	switch {
	case cp < 0x20:
		return true
	case cp >= 0x7f && cp <= 0x9f:
		return true
	case cp >= 0x0300 && cp <= 0x036f:
		return true
	case cp == 0x200b || cp == 0x200c || cp == 0x200d || cp == 0x2060 || cp == 0xfeff:
		return true
	case cp >= 0xfe00 && cp <= 0xfe0f:
		return true
	case cp >= 0xe0100 && cp <= 0xe01ef:
		return true
	}
	return false
}

// EscapeMissingGlyphs replaces atlas-missing codepoints with "[U+HEX]"
// (always ranked against the default Spleen atlas, matching TS).
func EscapeMissingGlyphs(line string) string {
	def := atlas.Default().Bit
	miss := false
	for _, r := range line {
		if def.Rank(r) < 0 && !isEscapeExempt(r) {
			miss = true
			break
		}
	}
	if !miss {
		return line
	}
	var b strings.Builder
	b.Grow(len(line) + 16)
	for _, r := range line {
		if def.Rank(r) < 0 && !isEscapeExempt(r) {
			b.WriteString(GlyphEscapeOpen)
			b.WriteString(strings.ToUpper(strconvHex(r)))
			b.WriteString(GlyphEscapeClose)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func strconvHex(r rune) string {
	const digits = "0123456789abcdef"
	if r == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	v := uint32(r)
	for v > 0 {
		i--
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[i:])
}

// ExpandTabsInLine expands \t to a visible → marker padded to the next
// 4-column stop (wide CJK counts 2 cols).
func ExpandTabsInLine(line string) string {
	if !strings.Contains(line, "\t") {
		return line
	}
	var b strings.Builder
	b.Grow(len(line) + 8)
	col := 0
	for _, r := range line {
		if r == '\t' {
			span := tabWidth - (col % tabWidth)
			b.WriteRune('→')
			if span > 1 {
				b.WriteString(strings.Repeat(" ", span-1))
			}
			col += span
		} else {
			b.WriteRune(r)
			col += cellsFor(r, 1, DefaultRenderFont)
		}
	}
	return b.String()
}

func MeasureLineCols(line string, markerScale int, font string) int {
	w := 0
	for _, r := range line {
		w += cellsFor(r, markerScale, font)
	}
	return w
}

func ShrinkColsToContent(text string, cols, markerScale int, font string) int {
	return MeasureContentCols(text, cols, markerScale, font)
}

// MeasureContentCols returns the display width in cols of the widest line,
// capped at maxCols.
func MeasureContentCols(text string, maxCols, markerScale int, font string) int {
	cap_ := maxCols
	if cap_ < 1 {
		cap_ = 1
	}
	widest := 1
	for _, line := range strings.Split(text, "\n") {
		w := MeasureLineCols(EscapeMissingGlyphs(ExpandTabsInLine(line)), markerScale, font)
		if w > widest {
			widest = w
		}
		if widest >= cap_ {
			return cap_
		}
	}
	if widest > cap_ {
		return cap_
	}
	return widest
}

func WrapLines(text string, cols, markerScale int, font string) []string {
	var out []string
	for _, rawWithTabs := range strings.Split(MinifyForRender(text), "\n") {
		raw := EscapeMissingGlyphs(ExpandTabsInLine(rawWithTabs))
		if len(raw) == 0 {
			out = append(out, "")
			continue
		}
		start := 0
		curCols := 0
		for i, r := range raw {
			w := cellsFor(r, markerScale, font)
			if curCols+w > cols {
				out = append(out, raw[start:i])
				start = i
				curCols = w
			} else {
				curCols += w
			}
		}
		if start < len(raw) {
			out = append(out, raw[start:])
		}
	}
	return out
}

// utf16Len counts UTF-16 code units, matching TS String.prototype.length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func splitWrappedLinesIntoReadablePages(lines []string, maxLines, maxChars int) [][]string {
	var pages [][]string
	var cur []string
	curChars := 0
	lineLimit := maxLines
	if lineLimit < 1 {
		lineLimit = 1
	}
	charLimit := maxChars
	if charLimit < 1 {
		charLimit = 1
	}
	for _, line := range lines {
		lineChars := utf16Len(line)
		if len(cur) > 0 {
			lineChars++
		}
		if len(cur) > 0 && (len(cur) >= lineLimit || curChars+lineChars > charLimit) {
			pages = append(pages, cur)
			cur = nil
			curChars = 0
		}
		cur = append(cur, line)
		if len(cur) > 1 {
			curChars += utf16Len(line) + 1
		} else {
			curChars += utf16Len(line)
		}
	}
	if len(cur) > 0 {
		pages = append(pages, cur)
	}
	if len(pages) == 0 {
		pages = [][]string{{}}
	}
	return pages
}

func fbSet(fb []byte, idx int, v byte) {
	if idx >= 0 && idx < len(fb) {
		fb[idx] = v
	}
}

func blitGlyph(fb []byte, fbW, x, y int, cp rune, font string, markerMask []byte) int {
	g, ok := bitGlyph(cp, font)
	if !ok {
		return 0
	}
	a := g.atlas
	wide := a.Wide[g.rank] == 1
	srcW := a.CellW
	if wide {
		srcW *= 2
	}
	srcOff := int(a.Offsets[g.rank])
	yOffset := atlasSet(font).Bit.Ascent - a.Ascent
	for gy := 0; gy < a.CellH; gy++ {
		dstRow := (y+yOffset+gy)*fbW + x
		bitRowStart := srcOff + gy*srcW
		for gx := 0; gx < srcW; gx++ {
			bitIdx := bitRowStart + gx
			if (a.Pixels[bitIdx>>3]>>(7-(bitIdx&7)))&1 == 1 {
				fbSet(fb, dstRow+gx, 255)
				if markerMask != nil {
					fbSet(markerMask, dstRow+gx, 1)
				}
			}
		}
	}
	if wide {
		return 2
	}
	return 1
}

func blitGlyphGray(fb []byte, fbW, x, y int, cp rune, font string) int {
	g, ok := grayGlyph(cp, font)
	if !ok {
		return 0
	}
	a := g.atlas
	wide := a.Wide[g.rank] == 1
	srcW := a.CellW
	if wide {
		srcW *= 2
	}
	srcOff := int(a.Offsets[g.rank])
	yOffset := atlasSet(font).Gray.Ascent - a.Ascent
	for gy := 0; gy < a.CellH; gy++ {
		dstRow := (y+yOffset+gy)*fbW + x
		srcRow := srcOff + gy*srcW
		for gx := 0; gx < srcW; gx++ {
			coverage := a.Pixels[srcRow+gx]
			if coverage > 0 {
				idx := dstRow + gx
				if idx >= 0 && idx < len(fb) && coverage > fb[idx] {
					fb[idx] = coverage
				}
			}
		}
	}
	if wide {
		return 2
	}
	return 1
}

func blitGlyphScaled(fb, markerMask []byte, fbW, fbH, x, y int, cp rune, scaleX int, font string) int {
	g, ok := bitGlyph(cp, font)
	if !ok {
		return 0
	}
	a := g.atlas
	wide := a.Wide[g.rank] == 1
	srcW := a.CellW
	if wide {
		srcW *= 2
	}
	srcOff := int(a.Offsets[g.rank])
	yOffset := atlasSet(font).Bit.Ascent - a.Ascent
	for gy := 0; gy < a.CellH; gy++ {
		py := y + yOffset + gy
		if py >= fbH {
			break
		}
		bitRowStart := srcOff + gy*srcW
		for gx := 0; gx < srcW; gx++ {
			bitIdx := bitRowStart + gx
			if (a.Pixels[bitIdx>>3]>>(7-(bitIdx&7)))&1 == 0 {
				continue
			}
			for sx := 0; sx < scaleX; sx++ {
				px := x + gx*scaleX + sx
				if px >= fbW {
					break
				}
				idx := py*fbW + px
				fbSet(fb, idx, 255)
				if markerMask != nil {
					fbSet(markerMask, idx, 1)
				}
			}
		}
	}
	if wide {
		return 2 * scaleX
	}
	return scaleX
}

func dilateInk(fb []byte, fbW, fbH, radius int, axis string) {
	if radius <= 0 {
		return
	}
	doX := axis == "both" || axis == "x"
	doY := axis == "both" || axis == "y"
	src := fb
	for pass := 0; pass < radius; pass++ {
		out := make([]byte, len(src))
		copy(out, src)
		for y := 0; y < fbH; y++ {
			for x := 0; x < fbW; x++ {
				i := y*fbW + x
				if src[i] > 0 {
					continue
				}
				if (doX && x > 0 && src[i-1] > 0) ||
					(doX && x+1 < fbW && src[i+1] > 0) ||
					(doY && y > 0 && src[i-fbW] > 0) ||
					(doY && y+1 < fbH && src[i+fbW] > 0) {
					out[i] = 255
				}
			}
		}
		src = out
	}
	if len(src) > 0 {
		copy(fb, src)
	}
}

func drawGrid(fb []byte, fbW, fbH, rows, gridCols, cellH, cellW, glyphH int) {
	for row := 0; row < rows; row++ {
		y := PadY + row*cellH + (glyphH - 1)
		if y >= fbH {
			break
		}
		rowStart := y * fbW
		for x := 0; x < fbW; x++ {
			if fb[rowStart+x] == 0 {
				fb[rowStart+x] = gridInk
			}
		}
	}
	if gridCols > 0 {
		for col := gridCols; ; col += gridCols {
			x := PadX + col*cellW
			if x >= fbW-PadX {
				break
			}
			for y := 0; y < fbH; y++ {
				idx := y*fbW + x
				if fb[idx] == 0 {
					fb[idx] = gridInk
				}
			}
		}
	}
}

func jsRound(f float64) int { return int(math.Floor(f + 0.5)) }

func wrappedLinesRuneCount(lines []string) int {
	n := 0
	for _, line := range lines {
		for range line {
			n++
		}
	}
	if len(lines) > 1 {
		n += len(lines) - 1
	}
	return n
}

// RenderChunkToPng renders text to a single PNG page (≤ maxHeightPx tall).
func RenderChunkToPng(text string, cols int, style RenderStyle, maxHeightPx int, slotText *string) (*RenderedImage, error) {
	markerScale := style.MarkerScale
	if markerScale < 1 {
		markerScale = 1
	}
	cellH := RenderCellHeight(style)
	lines := WrapLines(text, cols, markerScale, style.Font)
	var slotLines []string
	if style.ColorByRole && slotText != nil {
		slotLines = WrapLines(*slotText, cols, markerScale, style.Font)
	}

	maxLines := (maxHeightPx - 2*PadY) / cellH
	if maxLines < 1 {
		maxLines = 1
	}
	fitLines := lines
	if len(fitLines) > maxLines {
		fitLines = fitLines[:maxLines]
	}
	var fitSlotLines []string
	if slotLines != nil {
		fitSlotLines = slotLines
		if len(fitSlotLines) > maxLines {
			fitSlotLines = fitSlotLines[:maxLines]
		}
	}

	var charsRendered int
	if len(fitLines) == len(lines) {
		for range text {
			charsRendered++
		}
	} else {
		charsRendered = wrappedLinesRuneCount(fitLines)
	}
	return renderWrappedLinesToPNG(fitLines, fitSlotLines, charsRendered, cols, style)
}

func renderWrappedLinesToPNG(fitLines, fitSlotLines []string, charsRendered, cols int, style RenderStyle) (*RenderedImage, error) {
	useAA := style.AA
	selected := atlasSet(style.Font)
	atlasH := selected.Bit.CellH
	atlasW := selected.Bit.CellW
	if useAA {
		atlasH = selected.Gray.CellH
		atlasW = selected.Gray.CellW
	}
	markerScale := style.MarkerScale
	if markerScale < 1 {
		markerScale = 1
	}
	cellH := RenderCellHeight(style)
	cellW := RenderCellWidth(style)
	overhang := atlasW - cellW
	if overhang < 0 {
		overhang = 0
	}
	width := 2*PadX + cols*cellW + overhang
	height := 2*PadY + len(fitLines)*cellH

	fb := getPixelBuffer(width * height)
	var markerMask []byte
	if style.MarkerRed {
		markerMask = make([]byte, width*height)
	}
	useColorCycle := style.ColorCycle
	useColorByRole := style.ColorByRole
	var colorMask []byte
	if useColorCycle || useColorByRole {
		colorMask = make([]byte, width*height)
	}
	inkPalette := glyphPalette
	if useColorByRole {
		inkPalette = RolePalette
	}

	stampColorMask := func(baseX, baseY, spanW, colorSlot int) {
		if colorMask == nil {
			return
		}
		for gy := 0; gy < atlasH; gy++ {
			py := baseY + gy
			if py >= height {
				break
			}
			rowBase := py * width
			for gx := 0; gx < spanW; gx++ {
				px := baseX + gx
				if px >= width {
					break
				}
				idx := rowBase + px
				if fb[idx] > 0 {
					colorMask[idx] = byte(colorSlot)
				}
			}
		}
	}

	droppedChars := 0
	droppedCodepoints := map[rune]int{}
	glyphIndex := 0
	for row := 0; row < len(fitLines); row++ {
		line := fitLines[row]
		baseY := PadY + row*cellH
		col := 0
		var slotRow []rune
		if fitSlotLines != nil {
			if row < len(fitSlotLines) {
				slotRow = []rune(fitSlotLines[row])
			} else {
				slotRow = []rune{}
			}
		}
		charIdx := 0
		for _, cp := range line {
			if col >= cols {
				break
			}
			baseX := PadX + col*cellW
			isMarker := cp == nlSentinelCp
			var colorSlot int
			if useColorByRole {
				if slotRow != nil && charIdx < len(slotRow) {
					colorSlot = slotForMarkCp(slotRow[charIdx], true)
				}
			} else {
				colorSlot = (glyphIndex % len(glyphPalette)) + 1
			}
			var advance int
			if isMarker && markerScale > 1 {
				advance = blitGlyphScaled(fb, markerMask, width, height, baseX, baseY, cp, markerScale, style.Font)
				if colorMask != nil {
					stampColorMask(baseX, baseY, advance*cellW, colorSlot)
				}
			} else {
				if useAA {
					advance = blitGlyphGray(fb, width, baseX, baseY, cp, style.Font)
				} else {
					var mm []byte
					if isMarker {
						mm = markerMask
					}
					advance = blitGlyph(fb, width, baseX, baseY, cp, style.Font, mm)
				}
				if colorMask != nil && advance > 0 {
					stampColorMask(baseX, baseY, advance*atlasW, colorSlot)
				}
			}
			glyphIndex++
			charIdx++
			if advance == 0 {
				droppedChars++
				droppedCodepoints[cp]++
				col++
			} else {
				col += advance
			}
		}
	}

	if style.Grid {
		gc := style.GridCols
		if gc < 0 {
			gc = 0
		}
		drawGrid(fb, width, height, len(fitLines), gc, cellH, cellW, atlasH)
	}

	if style.InkDilate > 0 {
		axis := "both"
		if style.InkDilateAxis == "x" || style.InkDilateAxis == "y" {
			axis = style.InkDilateAxis
		}
		dilateInk(fb, width, height, style.InkDilate, axis)
	}

	if style.Invert == nil || *style.Invert {
		for i := range fb {
			fb[i] = 255 - fb[i]
		}
	}

	paper := 255
	if style.PaperGray != nil {
		paper = *style.PaperGray
		if paper < 0 {
			paper = 0
		}
		if paper > 255 {
			paper = 255
		}
	}
	if paper < 255 {
		for i := range fb {
			fb[i] = byte(jsRound(float64(paper) * float64(fb[i]) / 255))
		}
	}

	var png []byte
	var err error
	switch {
	case colorMask != nil:
		rgb := make([]byte, width*height*3)
		for i := range fb {
			g := int(fb[i])
			slot := int(colorMask[i])
			if slot > 0 {
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
				p := inkPalette[(slot-1)%len(inkPalette)]
				rgb[i*3] = byte(jsRound(float64(paper) - float64(cov*(paper-p[0]))/255))
				rgb[i*3+1] = byte(jsRound(float64(paper) - float64(cov*(paper-p[1]))/255))
				rgb[i*3+2] = byte(jsRound(float64(paper) - float64(cov*(paper-p[2]))/255))
			} else {
				rgb[i*3] = byte(g)
				rgb[i*3+1] = byte(g)
				rgb[i*3+2] = byte(g)
			}
		}
		png, err = EncodeRGBPNG(rgb, width, height)
	case markerMask != nil:
		rgb := make([]byte, width*height*3)
		for i := range fb {
			g := fb[i]
			if markerMask[i] == 1 && g < 128 {
				rgb[i*3] = 220
				rgb[i*3+1] = 0
				rgb[i*3+2] = 0
			} else {
				rgb[i*3] = g
				rgb[i*3+1] = g
				rgb[i*3+2] = g
			}
		}
		png, err = EncodeRGBPNG(rgb, width, height)
	default:
		png, err = EncodeGrayPNG(fb, width, height)
	}
	putPixelBuffer(fb)
	if err != nil {
		return nil, err
	}
	return &RenderedImage{
		PNG:               png,
		Width:             width,
		Height:            height,
		CharsRendered:     charsRendered,
		DroppedChars:      droppedChars,
		DroppedCodepoints: droppedCodepoints,
	}, nil
}

func RenderTextToPngsReflow(text string, cols int, style RenderStyle) ([]*RenderedImage, error) {
	if packed, ok := Reflow(text); ok {
		return RenderTextToPngs(packed, cols, style, MaxHeightPx, nil)
	}
	return RenderTextToPngs(text, cols, style, MaxHeightPx, nil)
}

// RenderTextToPngsWithCharLimit splits text into pages each ≤ maxHeightPx
// tall, respecting the per-image char budget.
func RenderTextToPngsWithCharLimit(text string, cols, maxCharsPerImage int, style RenderStyle, maxHeightPx int, slotText *string) ([]*RenderedImage, error) {
	markerScale := style.MarkerScale
	if markerScale < 1 {
		markerScale = 1
	}
	cellH := RenderCellHeight(style)
	lines := WrapLines(text, cols, markerScale, style.Font)
	var slotLines []string
	if style.ColorByRole && slotText != nil {
		slotLines = WrapLines(*slotText, cols, markerScale, style.Font)
	}
	hardLinesPerImg := (maxHeightPx - 2*PadY) / cellH
	if hardLinesPerImg < 1 {
		hardLinesPerImg = 1
	}
	byChars := maxCharsPerImage / cols
	if byChars < 1 {
		byChars = 1
	}
	linesPerImg := hardLinesPerImg
	if byChars < linesPerImg {
		linesPerImg = byChars
	}

	var images []*RenderedImage
	slotCursor := 0
	for _, page := range splitWrappedLinesIntoReadablePages(lines, linesPerImg, maxCharsPerImage) {
		var pageSlotLines []string
		if slotLines != nil {
			end := slotCursor + len(page)
			if end > len(slotLines) {
				end = len(slotLines)
			}
			if slotCursor < len(slotLines) {
				pageSlotLines = slotLines[slotCursor:end]
			} else {
				pageSlotLines = slotLines[:0]
			}
		}
		slotCursor += len(page)
		img, err := renderWrappedLinesToPNG(page, pageSlotLines, wrappedLinesRuneCount(page), cols, style)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, nil
}

func RenderTextToPngs(text string, cols int, style RenderStyle, maxHeightPx int, slotText *string) ([]*RenderedImage, error) {
	return RenderTextToPngsWithCharLimit(text, cols, ReadableCharsPerImage, style, maxHeightPx, slotText)
}
