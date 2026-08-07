package render

import "testing"

func TestMaxCharsPerImageTracksRenderedWidth(t *testing.T) {
	for _, tc := range []struct {
		cols int
		want int
	}{
		{cols: 0, want: 90},
		{cols: 80, want: 7200},
		{cols: 100, want: 9000},
		{cols: 200, want: 18000},
		{cols: DenseContentCols, want: DenseContentCharsPerImage},
		{cols: 400, want: ReadableCharsPerImage},
	} {
		if got := MaxCharsPerImage(tc.cols); got != tc.want {
			t.Errorf("MaxCharsPerImage(%d) = %d, want %d", tc.cols, got, tc.want)
		}
	}
}
