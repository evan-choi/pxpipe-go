package pxpipe

import "github.com/evan-choi/pxpipe-go/render"

// RenderOptions configures RenderTextToImages. When Model is set, its profile
// supplies unset geometry and style values.
type RenderOptions struct {
	Model            string
	Cols             int
	Shrink           *bool
	Reflow           bool
	MaxCharsPerImage int
	Style            *render.RenderStyle
	MaxHeightPx      int
}

// RenderTextToImages renders text with model-aware defaults.
func RenderTextToImages(text string, opts RenderOptions) (*render.RenderResult, error) {
	ro := render.RenderOptions{
		Cols:             opts.Cols,
		Shrink:           opts.Shrink,
		Reflow:           opts.Reflow,
		MaxCharsPerImage: opts.MaxCharsPerImage,
		Style:            opts.Style,
		MaxHeightPx:      opts.MaxHeightPx,
	}
	if opts.Model != "" {
		profile := ResolveGptProfile(opts.Model)
		if ro.Cols == 0 {
			ro.Cols = profile.StripCols
		}
		if ro.Style == nil {
			style := profile.Style
			ro.Style = &style
		}
		if ro.MaxHeightPx == 0 {
			ro.MaxHeightPx = profile.MaxHeightPx
		}
	}
	return render.RenderTextToImages(text, ro)
}

// ResolveVisionCost returns the model's configured image-token cost regime.
func ResolveVisionCost(model string) GptVisionCost {
	return ResolveGptProfile(model).Vision
}

// OpenAIVisionTokens returns the per-image input-token cost for the serving
// model. The name is retained for parity with the upstream OpenAI-path API.
func OpenAIVisionTokens(model string, width, height int) int {
	return visionTokensFor(ResolveGptProfile(model), width, height)
}
