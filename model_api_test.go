package pxpipe

import (
	"reflect"
	"strings"
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

func TestRenderTextToImagesUsesModelProfile(t *testing.T) {
	t.Setenv("PXPIPE_GPT_PROFILES", "")
	shrink := false
	text := strings.Repeat("model-aware render\n", 120)
	got, err := RenderTextToImages(text, RenderOptions{Model: "gpt-5.6-sol", Shrink: &shrink})
	if err != nil {
		t.Fatal(err)
	}
	profile := ResolveGptProfile("gpt-5.6-sol")
	style := profile.Style
	want, err := render.RenderTextToImages(text, render.RenderOptions{
		Cols:        profile.StripCols,
		Shrink:      &shrink,
		Style:       &style,
		MaxHeightPx: profile.MaxHeightPx,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("model profile defaults were not forwarded to render")
	}
}

func TestVisionCostPublicHelpers(t *testing.T) {
	t.Setenv("PXPIPE_GPT_PROFILES", "")
	tests := []struct {
		model         string
		width, height int
		regime        string
		tokens        int
	}{
		{"gpt-5.6-sol", 768, 1932, "patch", 1464},
		{"gpt-5.6-mini", 768, 1932, "patch", 2372},
		{"gpt-5", 2048, 2048, "tile", 630},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := ResolveVisionCost(tt.model).Regime; got != tt.regime {
				t.Errorf("regime = %q, want %q", got, tt.regime)
			}
			if got := OpenAIVisionTokens(tt.model, tt.width, tt.height); got != tt.tokens {
				t.Errorf("tokens = %d, want %d", got, tt.tokens)
			}
		})
	}
}
