package render

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"runtime"

	"github.com/klauspost/compress/zlib"
)

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

const maxCachedPNGBuffer = 8 << 20

type pngEncoder struct {
	raw        bytes.Buffer
	compressed bytes.Buffer
	zw         *zlib.Writer
}

var pngEncoderCache = make(chan *pngEncoder, runtime.GOMAXPROCS(0))

func newPNGEncoder() *pngEncoder {
	e := &pngEncoder{}
	e.zw, _ = zlib.NewWriterLevel(&e.compressed, 6)
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
	if e.raw.Cap() > maxCachedPNGBuffer || e.compressed.Cap() > maxCachedPNGBuffer {
		return
	}
	select {
	case pngEncoderCache <- e:
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

func encodePNG(pixels []byte, width, height, bytesPerPixel int, colorType byte) []byte {
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(height))
	ihdr[8] = 8
	ihdr[9] = colorType

	e := getPNGEncoder()
	e.raw.Reset()
	stride := width*bytesPerPixel + 1
	e.raw.Grow(stride * height)
	rowLen := width * bytesPerPixel
	for y := 0; y < height; y++ {
		e.raw.WriteByte(0)
		e.raw.Write(pixels[y*rowLen : (y+1)*rowLen])
	}

	e.compressed.Reset()
	e.zw.Reset(&e.compressed)
	_, _ = e.zw.Write(e.raw.Bytes())
	_ = e.zw.Close()

	compressed := e.compressed.Bytes()
	out := make([]byte, len(compressed)+57)
	offset := copy(out, pngSignature)
	offset = writeChunk(out, offset, "IHDR", ihdr[:])
	offset = writeChunk(out, offset, "IDAT", compressed)
	writeChunk(out, offset, "IEND", nil)
	putPNGEncoder(e)
	return out
}

// EncodeGrayPNG encodes a row-major single-channel buffer (len = w×h) as an
// 8-bit grayscale PNG (filter None, single IDAT).
func EncodeGrayPNG(pixels []byte, width, height int) ([]byte, error) {
	if len(pixels) != width*height {
		return nil, fmt.Errorf("EncodeGrayPNG: pixels=%d != %d×%d", len(pixels), width, height)
	}
	return encodePNG(pixels, width, height, 1, 0), nil
}

// EncodeRGBPNG encodes an interleaved RGB buffer (len = w×h×3) as an 8-bit
// truecolor PNG (filter None, single IDAT).
func EncodeRGBPNG(pixels []byte, width, height int) ([]byte, error) {
	if len(pixels) != width*height*3 {
		return nil, fmt.Errorf("EncodeRGBPNG: pixels=%d != %d×%d×3", len(pixels), width, height)
	}
	return encodePNG(pixels, width, height, 3, 2), nil
}
