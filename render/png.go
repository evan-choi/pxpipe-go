package render

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"runtime"

	"github.com/klauspost/compress/flate"
	adler32 "github.com/mhr3/adler32-simd"
)

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
var pngFilterNone [1]byte

const (
	maxCachedBufferBytes = 8 << 20
	pngCompressionLevel  = 6
)

const (
	packedLowBits  uint64 = 0x7f7f7f7f7f7f7f7f
	packedHighBits uint64 = 0x8080808080808080
)

func filterAverage8(value, left, above uint64) uint64 {
	average := (left & above) + (((left ^ above) >> 1) & packedLowBits)
	difference := ((value & packedLowBits) | packedHighBits) - (average & packedLowBits)
	return (difference & packedLowBits) | ((value ^ average ^ ^difference) & packedHighBits)
}

func preferAverageFilter(pixels []byte, width, height int) bool {
	// ponytail: this bounded sample can miss a rare size winner; add per-row
	// selection only if representative payloads regress.
	const (
		rowStride = 5
		colStride = 7
		threshold = 47
	)
	score, samples := 0, 0
	for y := rowStride / 2; y < height; y += rowStride {
		row := pixels[y*width : (y+1)*width]
		above := pixels[(y-1)*width : y*width]
		for x := colStride / 2; x < width; x += colStride {
			value := row[x]
			filtered := value - byte((uint16(row[x-1])+uint16(above[x]))>>1)
			if filtered < 128 {
				score += int(filtered)
			} else {
				score += 256 - int(filtered)
			}
			samples++
		}
	}
	return score > samples*threshold
}

type pngEncoder struct {
	compressed bytes.Buffer
	filtered   []byte
	zw         *flate.Writer
	checksum   hash.Hash32
}

var pngEncoderCache = make(chan *pngEncoder, runtime.GOMAXPROCS(0))
var pixelBufferCache = make(chan []byte, runtime.GOMAXPROCS(0))

func newPNGEncoder() *pngEncoder {
	e := &pngEncoder{checksum: adler32.New()}
	e.zw, _ = flate.NewWriter(&e.compressed, pngCompressionLevel)
	return e
}

func getPNGEncoder() *pngEncoder {
	select {
	case e := <-pngEncoderCache:
		return e
	default:
		return newPNGEncoder()
	}
}

func putPNGEncoder(e *pngEncoder) {
	if e.compressed.Cap() > maxCachedBufferBytes || cap(e.filtered) > maxCachedBufferBytes {
		return
	}
	select {
	case pngEncoderCache <- e:
	default:
	}
}

func getPixelBuffer(size int) []byte {
	select {
	case buf := <-pixelBufferCache:
		if cap(buf) >= size {
			buf = buf[:size]
			clear(buf)
			return buf
		}
	default:
	}
	return make([]byte, size)
}

func putPixelBuffer(buf []byte) {
	if cap(buf) > maxCachedBufferBytes {
		return
	}
	select {
	case pixelBufferCache <- buf[:0]:
	default:
	}
}

func writeChunk(out []byte, offset int, typ string, data []byte) int {
	binary.BigEndian.PutUint32(out[offset:], uint32(len(data)))
	offset += 4
	start := offset
	offset += copy(out[offset:], typ)
	offset += copy(out[offset:], data)
	binary.BigEndian.PutUint32(out[offset:], crc32.ChecksumIEEE(out[start:offset]))
	return offset + 4
}

func (e *pngEncoder) compress(pixels []byte, width, height, bytesPerPixel int, filter byte) []byte {
	rowLen := width * bytesPerPixel
	e.compressed.Reset()
	e.compressed.WriteString("\x78\x9c")
	e.zw.Reset(&e.compressed)
	e.checksum.Reset()
	if filter == 0 {
		for y := 0; y < height; y++ {
			row := pixels[y*rowLen : (y+1)*rowLen]
			_, _ = e.zw.Write(pngFilterNone[:])
			_, _ = e.zw.Write(row)
			_, _ = e.checksum.Write(pngFilterNone[:])
			_, _ = e.checksum.Write(row)
		}
	} else {
		if cap(e.filtered) < rowLen+1 {
			e.filtered = make([]byte, rowLen+1)
		} else {
			e.filtered = e.filtered[:rowLen+1]
		}
		filtered := e.filtered
		for y := 0; y < height; y++ {
			row := pixels[y*rowLen : (y+1)*rowLen]
			filtered[0] = filter
			first := min(bytesPerPixel, rowLen)
			if y == 0 {
				copy(filtered[1:first+1], row[:first])
				x := first
				for ; x+8 <= rowLen; x += 8 {
					value := binary.LittleEndian.Uint64(row[x:])
					left := binary.LittleEndian.Uint64(row[x-bytesPerPixel:])
					binary.LittleEndian.PutUint64(filtered[x+1:], filterAverage8(value, left, 0))
				}
				for ; x < rowLen; x++ {
					filtered[x+1] = row[x] - row[x-bytesPerPixel]/2
				}
			} else {
				above := pixels[(y-1)*rowLen : y*rowLen]
				for x := range first {
					filtered[x+1] = row[x] - above[x]/2
				}
				x := first
				for ; x+8 <= rowLen; x += 8 {
					value := binary.LittleEndian.Uint64(row[x:])
					left := binary.LittleEndian.Uint64(row[x-bytesPerPixel:])
					up := binary.LittleEndian.Uint64(above[x:])
					binary.LittleEndian.PutUint64(filtered[x+1:], filterAverage8(value, left, up))
				}
				for ; x < rowLen; x++ {
					filtered[x+1] = row[x] - byte((uint16(row[x-bytesPerPixel])+uint16(above[x]))>>1)
				}
			}
			_, _ = e.zw.Write(filtered)
			_, _ = e.checksum.Write(filtered)
		}
	}
	_ = e.zw.Close()
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], e.checksum.Sum32())
	e.compressed.Write(checksum[:])
	return e.compressed.Bytes()
}

func encodePNG(pixels []byte, width, height, bytesPerPixel int, colorType byte) []byte {
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(height))
	ihdr[8] = 8
	ihdr[9] = colorType

	e := getPNGEncoder()
	var compressed []byte
	if bytesPerPixel == 1 {
		filter := byte(0)
		if width <= 1024 && preferAverageFilter(pixels, width, height) {
			filter = 3
		}
		compressed = e.compress(pixels, width, height, bytesPerPixel, filter)
	} else {
		compressed = e.compress(pixels, width, height, bytesPerPixel, 3)
	}
	out := make([]byte, len(compressed)+57)
	offset := copy(out, pngSignature)
	offset = writeChunk(out, offset, "IHDR", ihdr[:])
	offset = writeChunk(out, offset, "IDAT", compressed)
	writeChunk(out, offset, "IEND", nil)
	putPNGEncoder(e)
	return out
}

// EncodeGrayPNG encodes a row-major single-channel buffer (len = w×h) as an
// 8-bit grayscale PNG (sample-selected None/Average filter, single IDAT).
func EncodeGrayPNG(pixels []byte, width, height int) ([]byte, error) {
	if len(pixels) != width*height {
		return nil, fmt.Errorf("EncodeGrayPNG: pixels=%d != %d×%d", len(pixels), width, height)
	}
	return encodePNG(pixels, width, height, 1, 0), nil
}

// EncodeRGBPNG encodes an interleaved RGB buffer (len = w×h×3) as an 8-bit
// truecolor PNG (filter Average, single IDAT).
func EncodeRGBPNG(pixels []byte, width, height int) ([]byte, error) {
	if len(pixels) != width*height*3 {
		return nil, fmt.Errorf("EncodeRGBPNG: pixels=%d != %d×%d×3", len(pixels), width, height)
	}
	return encodePNG(pixels, width, height, 3, 2), nil
}
