package pxpipe

import (
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

func TestDenseGateUsesResolvedNonClaudeProfile(t *testing.T) {
	o := resolveOptions(&TransformOptions{Model: "gpt-5.6-sol"})
	g := denseGateGeometry(o)
	p := ResolveGptProfile(o.Model)

	if o.Cols != p.StripCols || g.cols != p.StripCols || g.maxHeightPx != p.MaxHeightPx {
		t.Fatalf("gate geometry = cols %d/%d, height %d; want %d/%d", o.Cols, g.cols, g.maxHeightPx, p.StripCols, p.MaxHeightPx)
	}
	if g.style != p.Style {
		t.Fatalf("gate style = %+v, want %+v", g.style, p.Style)
	}
	if g.pricing != p || g.pricing.Vision.Regime != "patch" {
		t.Fatalf("gate pricing = %+v, want resolved patch profile", g.pricing)
	}
}

func TestDenseGateUsesGeminiFlatVisionCost(t *testing.T) {
	o := resolveOptions(&TransformOptions{Model: "gemini-3.6-flash"})
	g := denseGateGeometry(o)
	rows := (g.maxHeightPx - 2*render.PadY) / render.RenderCellHeight(g.style)

	if got, want := imageTokensForRows(rows, g.cols, 0, g.maxChars, g), 1186.0; got != want {
		t.Fatalf("full-page Gemini gate cost = %.0f, want %.0f", got, want)
	}
}

func TestDenseGatePreservesClaudeProfile(t *testing.T) {
	o := resolveOptions(&TransformOptions{Model: "claude-fable-5"})
	g := denseGateGeometry(o)

	if g.cols != render.AnthropicSlabCols || g.maxHeightPx != render.MaxHeightPx {
		t.Fatalf("Claude gate geometry = %d/%d", g.cols, g.maxHeightPx)
	}
	if g.pricing.Vision.Regime != "patch28" || g.pricing.VisionTier != "high-res" {
		t.Fatalf("Claude gate pricing = %+v tier %q", g.pricing.Vision, g.pricing.VisionTier)
	}
}
