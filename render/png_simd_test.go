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
		{"empty_gray", 0, 0, 1, 0},
		{"zero_width_gray", 0, 7, 1, 0},
		{"single_gray", 1, 1, 1, 0},
		{"dense_gray", 1568, 720, 1, 0},
		{"dense_rgb", 1568, 720, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pixels := make([]byte, tc.width*tc.height*tc.bytesPerPixel)
			for i := range pixels {
				pixels[i] = byte(i*31 + i/7)
			}
			got := encodePNG(pixels, tc.width, tc.height, tc.bytesPerPixel, tc.colorType)

			rowLen := tc.width * tc.bytesPerPixel
			compress := func(filter byte) []byte {
				var raw, compressed bytes.Buffer
				for y := 0; y < tc.height; y++ {
					row := pixels[y*rowLen : (y+1)*rowLen]
					raw.WriteByte(filter)
					if filter == 0 {
						raw.Write(row)
						continue
					}
					for x, value := range row {
						left, above := byte(0), byte(0)
						if x >= tc.bytesPerPixel {
							left = row[x-tc.bytesPerPixel]
						}
						if y > 0 {
							above = pixels[(y-1)*rowLen+x]
						}
						raw.WriteByte(value - byte((uint16(left)+uint16(above))>>1))
					}
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
				return compressed.Bytes()
			}
			compressed := compress(3)
			if tc.bytesPerPixel == 1 {
				unfiltered := compress(0)
				if tc.width > 1024 || len(unfiltered) < len(compressed) {
					compressed = unfiltered
				}
			}
			want := make([]byte, len(compressed)+57)
			offset := copy(want, pngSignature)
			var ihdr [13]byte
			binary.BigEndian.PutUint32(ihdr[0:], uint32(tc.width))
			binary.BigEndian.PutUint32(ihdr[4:], uint32(tc.height))
			ihdr[8], ihdr[9] = 8, tc.colorType
			offset = writeChunk(want, offset, "IHDR", ihdr[:])
			offset = writeChunk(want, offset, "IDAT", compressed)
			writeChunk(want, offset, "IEND", nil)

			if !bytes.Equal(got, want) {
				t.Fatalf("PNG differs from zlib output: got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}
