package pxpipe

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/evan-choi/pxpipe-go/render"
)

// Break-even gates: image token cost (28-px patch pricing + GATE_MARGIN) vs a
// chars-per-token text estimate. Constants bias conservative — mispredictions
// pass through as text, never generate net-loss images.

const (
	charsPerTokenDefault = 4.0
	SlabCharsPerToken    = 2.0
	HistoryCharsPerToken = 2.0
	ReportCharsPerToken  = 3.7
	gateMargin           = 1.10
)

var LinesPerImage = maxInt(1, (render.MaxHeightPx-2*render.PadY)/render.CellH)

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type gateGeometry struct {
	cols        int
	maxHeightPx int
	maxChars    int
	style       render.RenderStyle
	tier        visionTier
}

func defaultGateGeometry() gateGeometry {
	return gateGeometry{
		cols:        render.DenseContentCols,
		maxHeightPx: render.MaxHeightPx,
		maxChars:    render.DenseContentCharsPerImage,
		style:       render.DenseRenderStyle,
		tier:        tierHighRes,
	}
}

func denseGateGeometry(o *resolvedOptions) gateGeometry {
	g := defaultGateGeometry()
	profile := resolveProfile(o.Model)
	g.cols = o.Cols
	if profile != nil {
		g.maxHeightPx = profile.maxHeightPx
		g.style = profile.style
		g.tier = profile.tier
	}
	return g
}

func singleColWidthPx(cols int, style render.RenderStyle) int {
	return 2*render.PadX + cols*render.RenderCellWidth(style)
}

func imageTokensForRows(visualRows, cols, imageCountCap, maxCharsPerImage int, g gateGeometry) float64 {
	if visualRows <= 0 {
		return 0
	}
	widthPx := singleColWidthPx(cols, g.style)
	cellH := render.RenderCellHeight(g.style)
	hardLinesPerImg := maxInt(1, (g.maxHeightPx-2*render.PadY)/cellH)
	readableLinesPerCol := maxInt(1, maxCharsPerImage/maxInt(1, cols))
	linesPerImage := minInt(hardLinesPerImg, readableLinesPerCol)
	rowsPerImage := linesPerImage
	imagesNeeded := ceilDiv(visualRows, linesPerImage)
	if imageCountCap > 0 && imagesNeeded > imageCountCap {
		imagesNeeded = imageCountCap
	}
	fullImages := maxInt(0, imagesNeeded-1)
	linesInLast := visualRows - fullImages*linesPerImage
	rowsInLast := minInt(maxInt(1, linesInLast), rowsPerImage)
	fullImageHeight := 2*render.PadY + rowsPerImage*cellH
	lastImageHeight := 2*render.PadY + rowsInLast*cellH
	imageSum := fullImages*visionTokens(g.tier, widthPx, fullImageHeight) +
		visionTokens(g.tier, widthPx, lastImageHeight)
	return math.Ceil(float64(imageSum) * gateMargin)
}

func imageTokensCost(text string, cols, imageCountCap int, shrinkWidth bool, maxCharsPerImage int, g gateGeometry) float64 {
	effectiveCols := cols
	if shrinkWidth {
		effectiveCols = render.ShrinkColsToContent(text, cols, 1, g.style.Font)
	}
	rows := countVisualRows(text, effectiveCols)
	return imageTokensForRows(rows, effectiveCols, imageCountCap, maxCharsPerImage, g)
}

// countVisualRows counts soft-wrapped rows: only hard \n breaks; the ↵
// sentinel is inline. Lengths are UTF-16 units, matching TS.
func countVisualRows(text string, cols int) int {
	rows := 0
	c := maxInt(1, cols)
	for _, line := range strings.Split(text, "\n") {
		l := u16len(line)
		if l == 0 {
			rows++
		} else {
			rows += ceilDiv(l, c)
		}
	}
	return rows
}

func lineRows(line string, cols int) int {
	return maxInt(1, ceilDiv(u16len(line), cols))
}

func estimateImageCount(text string, cols, maxCharsPerImage, maxLinesPerColumn int) int {
	readableLinesPerCol := maxInt(1, maxCharsPerImage/maxInt(1, cols))
	hardLinesPerCol := maxInt(1, maxLinesPerColumn)
	linesPerImage := minInt(hardLinesPerCol, readableLinesPerCol)
	charBudget := maxInt(1, maxCharsPerImage)
	rows := countVisualRows(text, cols)
	return maxInt(maxInt(1, ceilDiv(rows, linesPerImage)), ceilDiv(u16len(text), charBudget))
}

type gateEval struct {
	ImageTokens   float64 `json:"imageTokens"`
	TextTokens    float64 `json:"textTokens"`
	BurnImageSide float64 `json:"burnImageSide"`
	BurnTextSide  float64 `json:"burnTextSide"`
	Profitable    bool    `json:"profitable"`
}

func burnSides(priorWarmTokens, priorWarmImageTokens float64) (imageSide, textSide float64) {
	if priorWarmTokens > 0 {
		imageSide = priorWarmTokens * (cacheCreateRate - cacheReadRate)
	}
	if priorWarmImageTokens > 0 {
		textSide = priorWarmImageTokens * (cacheCreateRate - cacheReadRate)
	}
	return
}

func normCpt(charsPerToken float64) float64 {
	if charsPerToken > 0 && !math.IsInf(charsPerToken, 0) && !math.IsNaN(charsPerToken) {
		return charsPerToken
	}
	return charsPerTokenDefault
}

func evalCompressionProfitability(text string, cols, imageCountCap int, charsPerToken, priorWarmTokens, priorWarmImageTokens float64, shrinkWidth bool, g gateGeometry) *gateEval {
	if text == "" {
		return nil
	}
	cpt := normCpt(charsPerToken)
	imageTokens := imageTokensCost(text, cols, imageCountCap, shrinkWidth, render.ReadableCharsPerImage, g)
	textTokens := float64(u16len(text)) / cpt
	burnImg, burnText := burnSides(priorWarmTokens, priorWarmImageTokens)
	return &gateEval{
		ImageTokens:   imageTokens,
		TextTokens:    textTokens,
		BurnImageSide: burnImg,
		BurnTextSide:  burnText,
		Profitable:    imageTokens+burnImg < textTokens+burnText,
	}
}

func isCompressionProfitable(text string, cols, imageCountCap int, charsPerToken, priorWarmTokens, priorWarmImageTokens float64, shrinkWidth bool, maxCharsPerImage int, g gateGeometry) bool {
	if text == "" {
		return false
	}
	cpt := normCpt(charsPerToken)
	cost := imageTokensCost(text, cols, imageCountCap, shrinkWidth, maxCharsPerImage, g)
	textTokens := float64(u16len(text)) / cpt
	burnImg, burnText := burnSides(priorWarmTokens, priorWarmImageTokens)
	return cost+burnImg < textTokens+burnText
}

func isCompressionProfitableAmortized(text string, cols, imageCountCap int, charsPerToken float64, horizon int, priorWarmTokens, priorWarmImageTokens float64, shrinkWidth bool, maxCharsPerImage int, g gateGeometry) bool {
	if horizon <= 1 {
		return isCompressionProfitable(text, cols, imageCountCap, charsPerToken, priorWarmTokens, priorWarmImageTokens, shrinkWidth, maxCharsPerImage, g)
	}
	n := maxInt(2, horizon)
	if text == "" {
		return false
	}
	cpt := normCpt(charsPerToken)
	imageTokens := imageTokensCost(text, cols, imageCountCap, shrinkWidth, maxCharsPerImage, g)
	textTokens := float64(u16len(text)) / cpt
	imageLifetime := imageTokens * (cacheCreateRate + cacheReadRate*float64(n-1))
	textLifetime := textTokens * cacheReadRate * float64(n)
	burnImg, burnText := burnSides(priorWarmTokens, priorWarmImageTokens)
	return imageLifetime+burnImg < textLifetime+burnText
}

// --- whitespace compaction + reflow ---------------------------------------

var newlineRun3Re = regexp.MustCompile(`\n{3,}`)

func compactSlabWhitespace(text string) string {
	if text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		end := len(l)
		for end > 0 {
			c := l[end-1]
			if c != ' ' && c != '\t' {
				break
			}
			end--
		}
		lines[i] = l[:end]
	}
	return newlineRun3Re.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
}

func maybeReflow(text string, enabled bool) string {
	if !enabled {
		return text
	}
	safe := render.NeutralizeSentinel(text)
	if packed, ok := render.Reflow(safe); ok {
		return packed
	}
	return safe
}

// --- content classification + paging truncation ---------------------------

var (
	jsonObjectStartRe = regexp.MustCompile(`^\{\s*("|\})`)
	jsonArrayStartRe  = regexp.MustCompile(`^\[\s*("|\{|\[|-?\d|true\b|false\b|null\b|\])`)
	diffHeaderRe      = regexp.MustCompile(`^---\s+\S`)
	logLineRe         = regexp.MustCompile(`^(\[?(DEBUG|INFO|WARN|WARNING|ERROR|TRACE|FATAL)\]?\b|\d{4}-\d{2}-\d{2}[T ]?|\d{2}:\d{2}:\d{2}\b)`)
)

func classifyContent(text string) string {
	head := u16Slice(text, 0, 4096)
	trimmed := strings.TrimLeftFunc(head, isJSSpace)
	if strings.HasPrefix(trimmed, "{") && jsonObjectStartRe.MatchString(trimmed) {
		return "structured"
	}
	if strings.HasPrefix(trimmed, "[") && jsonArrayStartRe.MatchString(trimmed) {
		return "structured"
	}
	if strings.HasPrefix(trimmed, "---\n") || strings.HasPrefix(trimmed, "---\r\n") {
		return "structured"
	}
	if strings.HasPrefix(trimmed, "diff --git ") || diffHeaderRe.MatchString(trimmed) {
		return "structured"
	}
	split := strings.Split(head, "\n")
	if len(split) > 40 {
		split = split[:40]
	}
	var lines []string
	for _, l := range split {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 4 {
		return "other"
	}
	logHits := 0
	for _, l := range lines {
		if logLineRe.MatchString(l) {
			logHits++
		}
	}
	if float64(logHits)/float64(len(lines)) >= 0.3 {
		return "log"
	}
	return "other"
}

func groupThousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		return "-" + out
	}
	return out
}

type pagingMarkerArgs struct {
	originalChars   int
	originalLines   int
	originalEstImgs int
	shownHeadLines  int
	shownTailLines  int
	omittedLines    int
	omittedChars    int
}

func buildPagingMarker(a pagingMarkerArgs) string {
	tailNote := " Showing first " + strconv.Itoa(a.shownHeadLines) + " lines (tail elided)."
	if a.shownTailLines > 0 {
		tailNote = " Showing first " + strconv.Itoa(a.shownHeadLines) + " lines and last " + strconv.Itoa(a.shownTailLines) + " lines."
	}
	return "\n\n[ pxpipe paging: omitted " + groupThousands(a.omittedLines) + " lines " +
		"(" + groupThousands(a.omittedChars) + " chars) of content here. " +
		"Original length: " + groupThousands(a.originalChars) + " chars " +
		"(" + groupThousands(a.originalLines) + " lines, ~" + strconv.Itoa(a.originalEstImgs) + " images)." +
		tailNote + " ]\n\n"
}

type truncateResult struct {
	text         string
	omittedChars int
	truncated    bool
}

func truncateForBudget(text string, maxImages, cols, maxCharsPerImage, linesPerImage int) truncateResult {
	estImages := estimateImageCount(text, cols, maxCharsPerImage, linesPerImage)
	if estImages <= maxImages {
		return truncateResult{text: text}
	}
	readableLinesPerCol := maxInt(1, maxCharsPerImage/maxInt(1, cols))
	totalRowBudget := maxInt(8, maxImages*minInt(linesPerImage, readableLinesPerCol)-6)
	totalCharBudget := maxInt(128, maxImages*maxCharsPerImage-512)
	shape := classifyContent(text)
	nlChar := render.NLSentinel
	if strings.Contains(text, "\n") {
		nlChar = "\n"
	}
	lines := strings.Split(text, nlChar)
	originalLines := len(lines)
	originalChars := u16len(text)

	if shape == "structured" {
		rows, chars, cut := 0, 0, 0
		for i, line := range lines {
			r := lineRows(line, cols)
			c := u16len(line)
			if i > 0 {
				c++
			}
			if rows+r > totalRowBudget || chars+c > totalCharBudget {
				break
			}
			rows += r
			chars += c
			cut = i + 1
		}
		if cut == 0 {
			cut = 1
		}
		head := strings.Join(lines[:cut], nlChar)
		omitted := originalChars - u16len(head)
		return truncateResult{
			text: head + buildPagingMarker(pagingMarkerArgs{
				originalChars:   originalChars,
				originalLines:   originalLines,
				originalEstImgs: estImages,
				shownHeadLines:  cut,
				omittedLines:    originalLines - cut,
				omittedChars:    omitted,
			}),
			omittedChars: omitted,
			truncated:    true,
		}
	}

	headRowBudget := int(math.Floor(float64(totalRowBudget) * 0.6))
	tailRowBudget := totalRowBudget - headRowBudget
	headCharBudget := int(math.Floor(float64(totalCharBudget) * 0.6))
	tailCharBudget := totalCharBudget - headCharBudget
	headRows, headChars, headCut := 0, 0, 0
	for i, line := range lines {
		r := lineRows(line, cols)
		c := u16len(line)
		if i > 0 {
			c++
		}
		if headRows+r > headRowBudget || headChars+c > headCharBudget {
			break
		}
		headRows += r
		headChars += c
		headCut = i + 1
	}
	if headCut == 0 {
		headCut = 1
	}
	tailRows, tailChars := 0, 0
	tailStart := len(lines)
	for i := len(lines) - 1; i >= headCut; i-- {
		r := lineRows(lines[i], cols)
		c := u16len(lines[i])
		if i < len(lines)-1 {
			c++
		}
		if tailRows+r > tailRowBudget || tailChars+c > tailCharBudget {
			break
		}
		tailRows += r
		tailChars += c
		tailStart = i
	}
	if tailStart <= headCut || tailStart >= len(lines) {
		head := strings.Join(lines[:headCut], nlChar)
		omitted := originalChars - u16len(head)
		return truncateResult{
			text: head + buildPagingMarker(pagingMarkerArgs{
				originalChars:   originalChars,
				originalLines:   originalLines,
				originalEstImgs: estImages,
				shownHeadLines:  headCut,
				omittedLines:    originalLines - headCut,
				omittedChars:    omitted,
			}),
			omittedChars: omitted,
			truncated:    true,
		}
	}
	headText := strings.Join(lines[:headCut], nlChar)
	tailText := strings.Join(lines[tailStart:], nlChar)
	omitted := originalChars - u16len(headText) - u16len(tailText)
	return truncateResult{
		text: headText + buildPagingMarker(pagingMarkerArgs{
			originalChars:   originalChars,
			originalLines:   originalLines,
			originalEstImgs: estImages,
			shownHeadLines:  headCut,
			shownTailLines:  len(lines) - tailStart,
			omittedLines:    originalLines - headCut - (len(lines) - tailStart),
			omittedChars:    omitted,
		}) + tailText,
		omittedChars: omitted,
		truncated:    true,
	}
}
