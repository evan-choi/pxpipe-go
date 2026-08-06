package render

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/klauspost/compress/zlib"
)

func TestSIMDAdlerPNGMatchesZlib(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		width, height, bytesPerPixel int
		colorType                    byte
	}{
		{"gray", 1568, 720, 1, 0},
		{"rgb", 1568, 96, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pixels := make([]byte, tc.width*tc.height*tc.bytesPerPixel)
			for i := range pixels {
				pixels[i] = byte(i*31 + i/7)
			}
			got := encodePNG(pixels, tc.width, tc.height, tc.bytesPerPixel, tc.colorType)

			var raw, compressed bytes.Buffer
			rowLen := tc.width * tc.bytesPerPixel
			for y := 0; y < tc.height; y++ {
				raw.WriteByte(0)
				raw.Write(pixels[y*rowLen : (y+1)*rowLen])
			}
			zw, err := zlib.NewWriterLevel(&compressed, 6)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := zw.Write(raw.Bytes()); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			want := make([]byte, len(compressed.Bytes())+57)
			offset := copy(want, pngSignature)
			var ihdr [13]byte
			binary.BigEndian.PutUint32(ihdr[0:], uint32(tc.width))
			binary.BigEndian.PutUint32(ihdr[4:], uint32(tc.height))
			ihdr[8], ihdr[9] = 8, tc.colorType
			offset = writeChunk(want, offset, "IHDR", ihdr[:])
			offset = writeChunk(want, offset, "IDAT", compressed.Bytes())
			writeChunk(want, offset, "IEND", nil)

			if !bytes.Equal(got, want) {
				t.Fatalf("PNG differs from zlib output: got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}
