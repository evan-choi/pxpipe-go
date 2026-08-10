// Package render ports pxpipe's text→PNG renderer: it wraps text onto a
// fixed-pitch glyph grid (Spleen 5x8 or JetBrains Mono atlases) and encodes
// dense grayscale/RGB PNG pages sized for Anthropic's vision tiling.
package render

import (
	"encoding/binary"
	"math"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

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
	LinesPerImage             = (MaxHeightPx - 2*PadY) / CellH

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

// MaxCharsPerImage returns the real char capacity of one page at cols.
// DenseContentCharsPerImage is only correct at DenseContentCols.
func MaxCharsPerImage(cols int) int {
	if cols < 1 {
		cols = 1
	}
	return min(cols*LinesPerImage, ReadableCharsPerImage)
}

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

var (
	pageRenderOnce sync.Once
	pageRenderJobs chan pageRenderJob
)

type pageRenderJob struct {
	batch *pageRenderBatch
	index int
}

type pageRenderBatch struct {
	pages         [][]string
	pageSlotLines [][]string
	images        []*RenderedImage
	errs          []error
	cols          int
	style         RenderStyle
	wg            sync.WaitGroup
}

func pageRenderQueue() chan pageRenderJob {
	pageRenderOnce.Do(func() {
		workers := runtime.GOMAXPROCS(0)
		pageRenderJobs = make(chan pageRenderJob, workers)
		for range workers {
			go func() {
				for job := range pageRenderJobs {
					job.batch.render(job.index)
				}
			}()
		}
	})
	return pageRenderJobs
}

func (b *pageRenderBatch) render(i int) {
	var slots []string
	if b.pageSlotLines != nil {
		slots = b.pageSlotLines[i]
	}
	b.images[i], b.errs[i] = renderWrappedLinesToPNG(
		b.pages[i], slots, wrappedLinesRuneCount(b.pages[i]), b.cols, b.style,
	)
	b.wg.Done()
}

type RenderedImage struct {
	PNG               []byte
	Width             int
	Height            int
	CharsRendered     int
	DroppedChars      int
	DroppedCodepoints map[rune]int
	base64            *renderedImageBase64
	sequence          *renderedImageSequence
	sequenceIndex     int
}

var glyphPalette = [...][3]int{
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

func bitGlyph(cp rune, selected *atlas.Set) (glyphRef, bool) {
	if r := selected.Bit.Rank(cp); r >= 0 {
		return glyphRef{selected.Bit, r}, true
	}
	if def := atlas.Default().Bit; selected.Bit != def {
		if r := def.Rank(cp); r >= 0 {
			return glyphRef{def, r}, true
		}
	}
	return glyphRef{}, false
}

func grayGlyph(cp rune, selected *atlas.Set) (glyphRef, bool) {
	if r := selected.Gray.Rank(cp); r >= 0 {
		return glyphRef{selected.Gray, r}, true
	}
	if def := atlas.Default().Gray; selected.Gray != def {
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
func cellsFor(cp rune, markerScale int, selected *atlas.Set) int {
	if cp == nlSentinelCp {
		return max(1, markerScale)
	}
	if cp >= 0 && cp < 128 {
		return 1
	}
	return cellsForSlow(cp, selected)
}

func cellsForSlow(cp rune, selected *atlas.Set) int {
	g, ok := bitGlyph(cp, selected)
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
	text = MinifyForRender(text)
	if !strings.Contains(text, "\t") {
		return strings.ReplaceAll(text, "\n", NLSentinel), true
	}
	lines := strings.Split(text, "\n")
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
	known := true
	for i := 0; i < len(line); {
		if line[i] < 0x80 {
			i++
			continue
		}
		if strings.HasPrefix(line[i:], NLSentinel) {
			i += len(NLSentinel)
			continue
		}
		known = false
		break
	}
	if known {
		return line
	}
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
	selected := atlasSet(DefaultRenderFont)
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
			col += cellsFor(r, 1, selected)
		}
	}
	return b.String()
}

func MeasureLineCols(line string, markerScale int, font string) int {
	w := 0
	selected := atlasSet(font)
	for _, r := range line {
		w += cellsFor(r, markerScale, selected)
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
	for line := range strings.SplitSeq(text, "\n") {
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
	selected := atlasSet(font)
	for rawWithTabs := range strings.SplitSeq(MinifyForRender(text), "\n") {
		raw := EscapeMissingGlyphs(ExpandTabsInLine(rawWithTabs))
		if len(raw) == 0 {
			out = append(out, "")
			continue
		}
		start := 0
		curCols := 0
		for i, r := range raw {
			w := cellsFor(r, markerScale, selected)
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

func wrapReflowedLines(text string, cols, markerScale int, font string) []string {
	raw := EscapeMissingGlyphs(text)
	if len(raw) == 0 {
		return []string{""}
	}
	var out []string
	selected := atlasSet(font)
	start := 0
	curCols := 0
	for i, r := range raw {
		w := cellsFor(r, markerScale, selected)
		if curCols+w > cols {
			out = append(out, raw[start:i])
			start = i
			curCols = w
		} else {
			curCols += w
		}
	}
	return append(out, raw[start:])
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
	start := 0
	curChars := 0
	lineLimit := maxLines
	if lineLimit < 1 {
		lineLimit = 1
	}
	charLimit := maxChars
	if charLimit < 1 {
		charLimit = 1
	}
	for i, line := range lines {
		lineChars := utf16Len(line)
		if i > start {
			lineChars++
		}
		if i > start && (i-start >= lineLimit || curChars+lineChars > charLimit) {
			pages = append(pages, lines[start:i])
			start = i
			curChars = 0
		}
		if i > start {
			curChars += utf16Len(line) + 1
		} else {
			curChars += utf16Len(line)
		}
	}
	if start < len(lines) {
		pages = append(pages, lines[start:])
	}
	if len(pages) == 0 {
		pages = [][]string{{}}
	}
	return pages
}

func textPages(text string, cols, maxCharsPerImage int, style RenderStyle, maxHeightPx int) [][]string {
	markerScale := style.MarkerScale
	if markerScale < 1 {
		markerScale = 1
	}
	lines := WrapLines(text, cols, markerScale, style.Font)
	hardLinesPerImg := (maxHeightPx - 2*PadY) / RenderCellHeight(style)
	if hardLinesPerImg < 1 {
		hardLinesPerImg = 1
	}
	byChars := maxCharsPerImage / cols
	if byChars < 1 {
		byChars = 1
	}
	return splitWrappedLinesIntoReadablePages(lines, min(hardLinesPerImg, byChars), maxCharsPerImage)
}

func reflowedTextPages(text string, cols, maxCharsPerImage int, style RenderStyle, maxHeightPx int) [][]string {
	markerScale := style.MarkerScale
	if markerScale < 1 {
		markerScale = 1
	}
	lines := wrapReflowedLines(text, cols, markerScale, style.Font)
	hardLinesPerImg := (maxHeightPx - 2*PadY) / RenderCellHeight(style)
	if hardLinesPerImg < 1 {
		hardLinesPerImg = 1
	}
	byChars := maxCharsPerImage / cols
	if byChars < 1 {
		byChars = 1
	}
	return splitWrappedLinesIntoReadablePages(lines, min(hardLinesPerImg, byChars), maxCharsPerImage)
}

type textPageCounter struct {
	pages, lines, chars int
	maxLines, maxChars  int
	stopAfter           int
}

func (c *textPageCounter) addLine(chars int) bool {
	nextChars := chars
	if c.lines > 0 {
		nextChars++
	}
	if c.lines > 0 && (c.lines >= c.maxLines || c.chars+nextChars > c.maxChars) {
		c.pages++
		if c.stopAfter > 0 && c.pages >= c.stopAfter {
			return false
		}
		c.lines = 0
		c.chars = 0
		nextChars = chars
	}
	c.lines++
	c.chars += nextChars
	return true
}

func countTextPages(text string, cols, maxCharsPerImage int, style RenderStyle, maxHeightPx, stopAfter int, reflowed bool) int {
	markerScale := style.MarkerScale
	if markerScale < 1 {
		markerScale = 1
	}
	hardLinesPerImg := (maxHeightPx - 2*PadY) / RenderCellHeight(style)
	if hardLinesPerImg < 1 {
		hardLinesPerImg = 1
	}
	byChars := maxCharsPerImage / cols
	if byChars < 1 {
		byChars = 1
	}
	counter := textPageCounter{
		maxLines:  min(hardLinesPerImg, byChars),
		maxChars:  max(1, maxCharsPerImage),
		stopAfter: stopAfter,
	}
	selected := atlasSet(style.Font)
	defaultAtlas := atlas.Default().Bit
	countLine := func(raw string) bool {
		if len(raw) == 0 {
			return counter.addLine(0)
		}
		lineCols := 0
		lineChars := 0
		for i := 0; i < len(raw); {
			if raw[i] < utf8.RuneSelf {
				start := i
				for i < len(raw) && raw[i] < utf8.RuneSelf {
					i++
				}
				n := i - start
				if lineCols >= cols {
					if !counter.addLine(lineChars) {
						return false
					}
					lineCols = 0
					lineChars = 0
				}
				room := cols - lineCols
				if n <= room {
					lineCols += n
					lineChars += n
					continue
				}
				lineChars += room
				n -= room
				if !counter.addLine(lineChars) {
					return false
				}
				for n > cols {
					if !counter.addLine(cols) {
						return false
					}
					n -= cols
				}
				lineCols = n
				lineChars = n
				continue
			}
			r, size := utf8.DecodeRuneInString(raw[i:])
			i += size
			if r >= utf8.RuneSelf && r != nlSentinelCp && defaultAtlas.Rank(r) < 0 && !isEscapeExempt(r) {
				for range len(GlyphEscapeOpen) + len(strconvHex(r)) + len(GlyphEscapeClose) {
					if lineCols+1 > cols {
						if !counter.addLine(lineChars) {
							return false
						}
						lineCols = 1
						lineChars = 0
					} else {
						lineCols++
					}
					lineChars++
				}
				continue
			}
			w := cellsFor(r, markerScale, selected)
			if lineCols+w > cols {
				if !counter.addLine(lineChars) {
					return false
				}
				lineCols = w
				lineChars = 0
			} else {
				lineCols += w
			}
			lineChars++
			if r >= 0x10000 {
				lineChars++
			}
		}
		return counter.addLine(lineChars)
	}
	if reflowed {
		if !countLine(text) {
			return stopAfter + 1
		}
	} else {
		for rawWithTabs := range strings.SplitSeq(MinifyForRender(text), "\n") {
			if !countLine(ExpandTabsInLine(rawWithTabs)) {
				return stopAfter + 1
			}
		}
	}
	return counter.pages + 1
}

// CountTextPages returns the exact number of pages RenderTextToPngs would
// produce without rasterizing or encoding them.
func CountTextPages(text string, cols int, style RenderStyle, maxHeightPx int) int {
	return countTextPages(text, cols, ReadableCharsPerImage, style, maxHeightPx, 0, false)
}

// FitsTextPages reports whether the exact render fits within maxPages.
func FitsTextPages(text string, cols int, style RenderStyle, maxHeightPx, maxPages int) bool {
	return maxPages > 0 && countTextPages(text, cols, ReadableCharsPerImage, style, maxHeightPx, maxPages, false) <= maxPages
}

// FitsReflowedTextPages is FitsTextPages for output already produced by Reflow.
func FitsReflowedTextPages(text string, cols int, style RenderStyle, maxHeightPx, maxPages int) bool {
	return maxPages > 0 && countTextPages(text, cols, ReadableCharsPerImage, style, maxHeightPx, maxPages, true) <= maxPages
}

func fbSet(fb []byte, idx int, v byte) {
	if idx >= 0 && idx < len(fb) {
		fb[idx] = v
	}
}

func invertBytes(buf []byte) {
	for len(buf) >= 8 {
		binary.LittleEndian.PutUint64(buf, ^binary.LittleEndian.Uint64(buf))
		buf = buf[8:]
	}
	for i := range buf {
		buf[i] = ^buf[i]
	}
}

func blitGlyph(fb []byte, fbW, x, y int, cp rune, selected *atlas.Set, markerMask []byte) int {
	g, ok := bitGlyph(cp, selected)
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
	yOffset := selected.Bit.Ascent - a.Ascent
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

func blitGlyphGray(fb []byte, fbW, fbH, x, y int, cp rune, selected *atlas.Set, overwrite bool) int {
	g, ok := grayGlyph(cp, selected)
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
	yOffset := selected.Gray.Ascent - a.Ascent
	dstY := y + yOffset
	if overwrite && x >= 0 && x+srcW <= fbW && dstY >= 0 && dstY+a.CellH <= fbH {
		dstRow := dstY*fbW + x
		srcRow := srcOff
		switch srcW {
		case 5:
			for gy := 0; gy < a.CellH; gy++ {
				copy(fb[dstRow:dstRow+5], a.Pixels[srcRow:srcRow+5])
				dstRow += fbW
				srcRow += 5
			}
		case 10:
			for gy := 0; gy < a.CellH; gy++ {
				copy(fb[dstRow:dstRow+10], a.Pixels[srcRow:srcRow+10])
				dstRow += fbW
				srcRow += 10
			}
		default:
			for gy := 0; gy < a.CellH; gy++ {
				copy(fb[dstRow:dstRow+srcW], a.Pixels[srcRow:srcRow+srcW])
				dstRow += fbW
				srcRow += srcW
			}
		}
		if wide {
			return 2
		}
		return 1
	}
	for gy := 0; gy < a.CellH; gy++ {
		dstRow := (dstY+gy)*fbW + x
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

func blitGlyphScaled(fb, markerMask []byte, fbW, fbH, x, y int, cp rune, scaleX int, selected *atlas.Set) int {
	g, ok := bitGlyph(cp, selected)
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
	yOffset := selected.Bit.Ascent - a.Ascent
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

func fillColorLUT(lut *[len(glyphPalette)][256][3]byte, palette [][3]int, paper int) {
	for slot := range lut {
		p := palette[slot%len(palette)]
		for g := range 256 {
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
			lut[slot][g][0] = byte(jsRound(float64(paper) - float64(cov*(paper-p[0]))/255))
			lut[slot][g][1] = byte(jsRound(float64(paper) - float64(cov*(paper-p[1]))/255))
			lut[slot][g][2] = byte(jsRound(float64(paper) - float64(cov*(paper-p[2]))/255))
		}
	}
}

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

	pixelCount := width * height
	useColorCycle := style.ColorCycle
	useColorByRole := style.ColorByRole
	useColor := useColorCycle || useColorByRole
	scratchLen := pixelCount
	if style.MarkerRed {
		scratchLen += pixelCount
	}
	if useColor {
		scratchLen += pixelCount
	}
	if style.MarkerRed || useColor {
		scratchLen += 3 * pixelCount
	}
	scratch := getPixelBuffer(scratchLen)
	fb := scratch[:pixelCount]
	nextScratch := pixelCount
	var markerMask []byte
	if style.MarkerRed {
		markerMask = scratch[nextScratch : nextScratch+pixelCount]
		nextScratch += pixelCount
	}
	var colorMask []byte
	if useColor {
		colorMask = scratch[nextScratch : nextScratch+pixelCount]
		nextScratch += pixelCount
	}
	var rgb []byte
	if markerMask != nil || colorMask != nil {
		rgb = scratch[nextScratch : nextScratch+3*pixelCount]
	}
	inkPalette := glyphPalette[:]
	if useColorByRole {
		inkPalette = RolePalette
	}
	solidColorCells := cellW >= atlasW && !style.Grid && style.InkDilate <= 0 && (style.Invert == nil || *style.Invert)

	stampColorMask := func(baseX, baseY, spanW, colorSlot int) {
		if colorMask == nil {
			return
		}
		if solidColorCells && baseX >= 0 && baseX+spanW <= width && baseY >= 0 && baseY+atlasH <= height {
			if colorSlot == 0 {
				return
			}
			fill := uint64(byte(colorSlot)) * 0x0101010101010101
			row := baseY*width + baseX
			for range atlasH {
				switch spanW {
				case 5:
					binary.LittleEndian.PutUint32(colorMask[row:], uint32(fill))
					colorMask[row+4] = byte(fill)
				case 10:
					binary.LittleEndian.PutUint64(colorMask[row:], fill)
					binary.LittleEndian.PutUint16(colorMask[row+8:], uint16(fill))
				default:
					for i := range spanW {
						colorMask[row+i] = byte(fill)
					}
				}
				row += width
			}
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
				advance = blitGlyphScaled(fb, markerMask, width, height, baseX, baseY, cp, markerScale, selected)
				if colorMask != nil {
					stampColorMask(baseX, baseY, advance*cellW, colorSlot)
				}
			} else {
				if useAA {
					advance = blitGlyphGray(fb, width, height, baseX, baseY, cp, selected, cellW >= atlasW)
				} else {
					var mm []byte
					if isMarker {
						mm = markerMask
					}
					advance = blitGlyph(fb, width, baseX, baseY, cp, selected, mm)
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
		invertBytes(fb)
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
		var colorLUT [len(glyphPalette)][256][3]byte
		fillColorLUT(&colorLUT, inkPalette, paper)
		for i := range fb {
			g := int(fb[i])
			slot := int(colorMask[i])
			if slot > 0 {
				color := colorLUT[slot-1][g]
				rgb[i*3] = color[0]
				rgb[i*3+1] = color[1]
				rgb[i*3+2] = color[2]
			} else {
				rgb[i*3] = byte(g)
				rgb[i*3+1] = byte(g)
				rgb[i*3+2] = byte(g)
			}
		}
		png, err = EncodeRGBPNG(rgb, width, height)
	case markerMask != nil:
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
	putPixelBuffer(scratch)
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
func renderTextToPngsWithCharLimitUncached(text string, cols, maxCharsPerImage int, style RenderStyle, maxHeightPx int, slotText *string, reflowed bool) ([]*RenderedImage, error) {
	markerScale := style.MarkerScale
	if markerScale < 1 {
		markerScale = 1
	}
	var slotLines []string
	if style.ColorByRole && slotText != nil {
		slotLines = WrapLines(*slotText, cols, markerScale, style.Font)
	}
	var pages [][]string
	if reflowed {
		pages = reflowedTextPages(text, cols, maxCharsPerImage, style, maxHeightPx)
	} else {
		pages = textPages(text, cols, maxCharsPerImage, style, maxHeightPx)
	}
	var pageSlotLines [][]string
	if slotLines != nil {
		pageSlotLines = make([][]string, len(pages))
	}
	slotCursor := 0
	for i, page := range pages {
		if slotLines != nil {
			end := slotCursor + len(page)
			if end > len(slotLines) {
				end = len(slotLines)
			}
			if slotCursor < len(slotLines) {
				pageSlotLines[i] = slotLines[slotCursor:end]
			} else {
				pageSlotLines[i] = slotLines[:0]
			}
		}
		slotCursor += len(page)
	}

	images := make([]*RenderedImage, len(pages))
	renderPage := func(i int) (*RenderedImage, error) {
		var slots []string
		if pageSlotLines != nil {
			slots = pageSlotLines[i]
		}
		return renderWrappedLinesToPNG(pages[i], slots, wrappedLinesRuneCount(pages[i]), cols, style)
	}
	workers := min(len(pages), runtime.GOMAXPROCS(0))
	if workers == 1 {
		for i := range pages {
			var err error
			images[i], err = renderPage(i)
			if err != nil {
				return nil, err
			}
		}
		return images, nil
	}

	batch := pageRenderBatch{
		pages:         pages,
		pageSlotLines: pageSlotLines,
		images:        images,
		errs:          make([]error, len(pages)),
		cols:          cols,
		style:         style,
	}
	batch.wg.Add(len(pages))
	jobs := pageRenderQueue()
	for i := range pages {
		jobs <- pageRenderJob{&batch, i}
	}
	batch.wg.Wait()
	for _, err := range batch.errs {
		if err != nil {
			return nil, err
		}
	}
	return images, nil
}

func RenderTextToPngsWithCharLimit(text string, cols, maxCharsPerImage int, style RenderStyle, maxHeightPx int, slotText *string) ([]*RenderedImage, error) {
	return renderTextToPngsCached(renderResultCache, text, cols, maxCharsPerImage, style, maxHeightPx, slotText, false)
}

// RenderReflowedTextToPngs renders output already produced by Reflow.
func RenderReflowedTextToPngs(text string, cols int, style RenderStyle, maxHeightPx int) ([]*RenderedImage, error) {
	return renderTextToPngsCached(renderResultCache, text, cols, ReadableCharsPerImage, style, maxHeightPx, nil, true)
}

func RenderTextToPngs(text string, cols int, style RenderStyle, maxHeightPx int, slotText *string) ([]*RenderedImage, error) {
	return RenderTextToPngsWithCharLimit(text, cols, ReadableCharsPerImage, style, maxHeightPx, slotText)
}
