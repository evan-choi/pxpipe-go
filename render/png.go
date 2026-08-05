package render

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sync"
)

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

type zlibEncoder struct {
	buf bytes.Buffer
	zw  *zlib.Writer
}

var zlibPool = sync.Pool{
	New: func() any {
		e := &zlibEncoder{}
		e.zw, _ = zlib.NewWriterLevel(&e.buf, zlib.BestSpeed)
		return e
	},
}

var scratchPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

func deflate(raw []byte) []byte {
	e := zlibPool.Get().(*zlibEncoder)
	e.buf.Reset()
	e.zw.Reset(&e.buf)
	_, _ = e.zw.Write(raw)
	_ = e.zw.Close()
	out := make([]byte, e.buf.Len())
	copy(out, e.buf.Bytes())
	zlibPool.Put(e)
	return out
}

func writeChunk(out *bytes.Buffer, typ string, data []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	out.Write(lenBuf[:])
	start := out.Len()
	out.WriteString(typ)
	out.Write(data)
	crc := crc32.ChecksumIEEE(out.Bytes()[start:])
	binary.BigEndian.PutUint32(lenBuf[:], crc)
	out.Write(lenBuf[:])
}

func encodePNG(pixels []byte, width, height, bytesPerPixel int, colorType byte) []byte {
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(height))
	ihdr[8] = 8
	ihdr[9] = colorType

	stride := width*bytesPerPixel + 1
	rawBuf := scratchPool.Get().(*bytes.Buffer)
	rawBuf.Reset()
	rawBuf.Grow(stride * height)
	rowLen := width * bytesPerPixel
	for y := 0; y < height; y++ {
		rawBuf.WriteByte(0)
		rawBuf.Write(pixels[y*rowLen : (y+1)*rowLen])
	}
	compressed := deflate(rawBuf.Bytes())
	scratchPool.Put(rawBuf)

	out := &bytes.Buffer{}
	out.Grow(len(compressed) + 128)
	out.Write(pngSignature)
	writeChunk(out, "IHDR", ihdr[:])
	writeChunk(out, "IDAT", compressed)
	writeChunk(out, "IEND", nil)
	return out.Bytes()
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
