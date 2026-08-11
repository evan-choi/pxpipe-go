package render

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/klauspost/compress/zlib"
)

func TestFilterAverage8MatchesScalar(t *testing.T) {
	state := uint64(1)
	for range 1 << 16 {
		state = state*6364136223846793005 + 1
		value := state
		state = state*6364136223846793005 + 1
		left := state
		state = state*6364136223846793005 + 1
		above := state
		got := filterAverage8(value, left, above)
		for shift := 0; shift < 64; shift += 8 {
			v := byte(value >> shift)
			l := byte(left >> shift)
			a := byte(above >> shift)
			want := v - byte((uint16(l)+uint16(a))>>1)
			if lane := byte(got >> shift); lane != want {
				t.Fatalf("lane %d: got %d, want %d", shift/8, lane, want)
			}
		}
	}
}

func TestPreferAverageFilter(t *testing.T) {
	const width, height = 768, 100
	uniform := make([]byte, width*height)
	for i := range uniform {
		uniform[i] = 255
	}
	if preferAverageFilter(uniform, width, height) {
		t.Fatal("uniform page selected Average")
	}

	noise := make([]byte, width*height)
	state := uint64(1)
	for i := range noise {
		state = state*6364136223846793005 + 1
		noise[i] = byte(state >> 32)
	}
	if !preferAverageFilter(noise, width, height) {
		t.Fatal("high-entropy page selected None")
	}
}

func TestSIMDAdlerPNGMatchesZlib(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		width, height, bytesPerPixel int
		colorType                    byte
	}{
		{"empty_gray", 0, 0, 1, 0},
		{"zero_width_gray", 0, 7, 1, 0},
		{"single_gray", 1, 1, 1, 0},
		{"sampled_gray", 768, 100, 1, 0},
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
				zw, err := zlib.NewWriterLevel(&compressed, pngCompressionLevel)
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
			filter := byte(3)
			if tc.bytesPerPixel == 1 {
				filter = 0
				if tc.width <= 1024 && preferAverageFilter(pixels, tc.width, tc.height) {
					filter = 3
				}
			}
			compressed := compress(filter)
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
