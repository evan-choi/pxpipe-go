package render

import (
	"bytes"
	"image/png"
	"os"
	"testing"
)

var benchmarkPNG []byte

func BenchmarkEncodeGrayPNG(b *testing.B) {
	data, err := os.ReadFile("../testdata/render/multi-page/page-0.png")
	if err != nil {
		b.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		b.Fatal(err)
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	pixels := make([]byte, width*height)
	for y := range height {
		for x := range width {
			gray, _, _, _ := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			pixels[y*width+x] = byte(gray >> 8)
		}
	}

	b.SetBytes(int64(len(pixels)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkPNG, err = EncodeGrayPNG(pixels, width, height)
	}
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(len(benchmarkPNG)), "output-B")
}
