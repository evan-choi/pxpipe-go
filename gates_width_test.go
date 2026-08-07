package pxpipe

import (
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

func TestDenseGateGeometryPricesCapacityAtRenderedWidth(t *testing.T) {
	for _, cols := range []int{80, 100, 200, 312} {
		o := resolveOptions(&TransformOptions{Cols: &cols})
		g := denseGateGeometry(o)
		if got, want := g.maxChars, render.MaxCharsPerImage(cols); got != want {
			t.Errorf("cols %d: maxChars = %d, want %d", cols, got, want)
		}
	}
}
