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

const maxCachedBufferBytes = 8 << 20

type pngEncoder struct {
	compressed bytes.Buffer
	alternate  bytes.Buffer
	filtered   []byte
	zw         *flate.Writer
	checksum   hash.Hash32
}

var pngEncoderCache = make(chan *pngEncoder, runtime.GOMAXPROCS(0))
var pixelBufferCache = make(chan []byte, runtime.GOMAXPROCS(0))

func newPNGEncoder() *pngEncoder {
	e := &pngEncoder{checksum: adler32.New()}
	e.zw, _ = flate.NewWriter(&e.compressed, 6)
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
	if e.compressed.Cap() > maxCachedBufferBytes || e.alternate.Cap() > maxCachedBufferBytes || cap(e.filtered) > maxCachedBufferBytes {
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
	if cap(e.filtered) < rowLen+1 {
		e.filtered = make([]byte, rowLen+1)
	} else {
		e.filtered = e.filtered[:rowLen+1]
	}
	filtered := e.filtered
	for y := 0; y < height; y++ {
		row := pixels[y*rowLen : (y+1)*rowLen]
		filtered[0] = filter
		if filter == 0 {
			copy(filtered[1:], row)
		} else {
			for x, value := range row {
				left, above := byte(0), byte(0)
				if x >= bytesPerPixel {
					left = row[x-bytesPerPixel]
				}
				if y > 0 {
					above = pixels[(y-1)*rowLen+x]
				}
				residual := value - byte((uint16(left)+uint16(above))>>1)
				filtered[x+1] = residual
			}
		}
		_, _ = e.zw.Write(filtered)
		_, _ = e.checksum.Write(filtered)
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
	if bytesPerPixel == 1 && width > 1024 {
		// ponytail: wide grayscale pages use the measured winner; compare both if a future profile regresses.
		compressed = e.compress(pixels, width, height, bytesPerPixel, 0)
	} else {
		compressed = e.compress(pixels, width, height, bytesPerPixel, 3)
		if bytesPerPixel == 1 {
			e.alternate.Reset()
			e.alternate.Write(compressed)
			if unfiltered := e.compress(pixels, width, height, bytesPerPixel, 0); len(unfiltered) < e.alternate.Len() {
				compressed = unfiltered
			} else {
				compressed = e.alternate.Bytes()
			}
		}
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
// 8-bit grayscale PNG (smallest of None/Average, single IDAT).
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
