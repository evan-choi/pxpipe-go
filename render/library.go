package render

// RenderOptions mirrors pxpipe's renderTextToImages options. Zero values mean
// "use the production defaults" except Shrink, which defaults to true and is
// only disabled when explicitly set to false.
type RenderOptions struct {
	Cols             int          `json:"cols,omitempty"`
	Shrink           *bool        `json:"shrink,omitempty"`
	Reflow           bool         `json:"reflow,omitempty"`
	MaxCharsPerImage int          `json:"maxCharsPerImage,omitempty"`
	Style            *RenderStyle `json:"style,omitempty"`
	MaxHeightPx      int          `json:"maxHeightPx,omitempty"`
}

type RenderedTextImage struct {
	PNG    []byte
	Width  int
	Height int
}

type RenderResult struct {
	Pages        []RenderedTextImage
	DroppedChars int
	Pixels       int
}

// RenderTextToImages renders arbitrary text to dense PNG pages — the public
// entry mirroring pxpipe's renderTextToImages (library.ts).
func RenderTextToImages(text string, opts RenderOptions) (*RenderResult, error) {
	maxCols := opts.Cols
	if maxCols == 0 {
		maxCols = DenseContentCols
	}
	if maxCols < 1 {
		maxCols = 1
	}
	style := DenseRenderStyle
	if opts.Style != nil {
		style = *opts.Style
	}
	maxHeightPx := opts.MaxHeightPx
	if maxHeightPx == 0 {
		maxHeightPx = MaxHeightPx
	}
	maxChars := opts.MaxCharsPerImage
	if maxChars == 0 {
		maxChars = DenseContentCharsPerImage
	}

	source := text
	if opts.Reflow {
		if packed, ok := Reflow(text); ok {
			source = packed
		}
	}

	// Content width is measured with the default font/markerScale, matching
	// library.ts which does not thread style into measureContentCols.
	cols := maxCols
	if opts.Shrink == nil || *opts.Shrink {
		cols = MeasureContentCols(source, maxCols, 1, DefaultRenderFont)
	}

	imgs, err := RenderTextToPngsWithCharLimit(source, cols, maxChars, style, maxHeightPx, nil)
	if err != nil {
		return nil, err
	}
	res := &RenderResult{}
	for _, im := range imgs {
		res.DroppedChars += im.DroppedChars
		res.Pixels += im.Width * im.Height
		res.Pages = append(res.Pages, RenderedTextImage{PNG: im.PNG, Width: im.Width, Height: im.Height})
	}
	return res, nil
}
