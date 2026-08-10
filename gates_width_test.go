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

func TestEstimateImageCountFromMetrics(t *testing.T) {
	if got := estimateImageCountFromMetrics(201, 9, 10, 100, 4); got != 3 {
		t.Fatalf("estimateImageCountFromMetrics() = %d, want 3", got)
	}
}

func TestMeasureVisualRows(t *testing.T) {
	rows, chars := measureVisualRows("ab\n\n😀c", 2)
	if rows != 4 || chars != 7 {
		t.Fatalf("measureVisualRows() = (%d, %d), want (4, 7)", rows, chars)
	}
	for _, text := range []string{"", "ascii\ntext", "한글\n😀", string([]byte{'a', 0xff, 'b'})} {
		if _, chars := measureVisualRows(text, 80); chars != u16len(text) {
			t.Fatalf("measureVisualRows(%q) chars = %d, want %d", text, chars, u16len(text))
		}
	}
}
