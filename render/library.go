package render

import (
	"runtime"
	"strings"
	"unsafe"
)

var reflowBufferCache = make(chan []byte, runtime.GOMAXPROCS(0))

func getReflowBuffer(size int) []byte {
	select {
	case buf := <-reflowBufferCache:
		if cap(buf) >= size {
			return buf[:size]
		}
	default:
	}
	return make([]byte, size)
}

func putReflowBuffer(buf []byte) {
	clear(buf)
	if cap(buf) > maxCachedBufferBytes {
		return
	}
	select {
	case reflowBufferCache <- buf[:0]:
	default:
	}
}

// reflowForRender returns a string view of buf; callers must return buf only
// after every consumer of the string has finished.
func reflowForRender(text string) (string, []byte, bool) {
	if strings.Contains(text, NLSentinel) || strings.Contains(text, "\t") {
		packed, ok := Reflow(text)
		return packed, nil, ok
	}
	text = MinifyForRender(text)
	newlines := strings.Count(text, "\n")
	if newlines == 0 {
		return text, nil, true
	}
	extra := len(NLSentinel) - 1
	if newlines > (int(^uint(0)>>1)-len(text))/extra {
		packed, ok := Reflow(text)
		return packed, nil, ok
	}
	buf := getReflowBuffer(len(text) + newlines*extra)
	out := buf[:0]
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			out = append(out, text...)
			break
		}
		out = append(out, text[:i]...)
		out = append(out, NLSentinel...)
		text = text[i+1:]
	}
	return unsafe.String(unsafe.SliceData(out), len(out)), out, true
}

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
		if packed, buf, ok := reflowForRender(text); ok {
			source = packed
			if buf != nil {
				defer putReflowBuffer(buf)
			}
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
