// Package atlas loads the pre-baked glyph atlases (Spleen 5x8 + JetBrains Mono
// 10/12/14; 1-bit and 8-bit grayscale companions) dumped from the pxpipe TS
// reference implementation.
package atlas

import (
	"compress/gzip"
	"embed"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/bytedance/sonic"
)

//go:embed data/*.gz data/meta.json
var dataFS embed.FS

// Atlas holds one glyph table. For 1-bit atlases Pixels is bit-packed
// MSB-first and Offsets are bit offsets; for grayscale atlases Pixels is one
// byte per pixel and Offsets are byte offsets.
type Atlas struct {
	CellW      int
	CellH      int
	Ascent     int
	Codepoints []uint32
	Offsets    []uint32
	Wide       []byte
	Pixels     []byte
}

// Rank binary-searches the sparse codepoint table; -1 = not in atlas.
func (a *Atlas) Rank(cp rune) int {
	lo, hi := 0, len(a.Codepoints)-1
	for lo <= hi {
		mid := (lo + hi) >> 1
		v := rune(a.Codepoints[mid])
		switch {
		case v == cp:
			return mid
		case v < cp:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return -1
}

// Set pairs a font's 1-bit atlas with its grayscale companion.
type Set struct {
	Bit  *Atlas
	Gray *Atlas
}

type atlasMeta struct {
	CellW     int `json:"cellW"`
	CellH     int `json:"cellH"`
	Ascent    int `json:"ascent"`
	NumGlyphs int `json:"numGlyphs"`
}

func loadMeta() map[string]atlasMeta {
	b, err := dataFS.ReadFile("data/meta.json")
	if err != nil {
		panic(fmt.Sprintf("atlas: missing meta.json: %v", err))
	}
	var m map[string]atlasMeta
	if err := sonic.Unmarshal(b, &m); err != nil {
		panic(fmt.Sprintf("atlas: bad meta.json: %v", err))
	}
	return m
}

func gunzip(name string) []byte {
	f, err := dataFS.Open("data/" + name + ".gz")
	if err != nil {
		panic(fmt.Sprintf("atlas: missing %s: %v", name, err))
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		panic(fmt.Sprintf("atlas: bad gzip %s: %v", name, err))
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		panic(fmt.Sprintf("atlas: read %s: %v", name, err))
	}
	return b
}

func u32le(b []byte) []uint32 {
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return out
}

func load(meta map[string]atlasMeta, name string) *Atlas {
	m, ok := meta[name]
	if !ok {
		panic("atlas: unknown atlas " + name)
	}
	return &Atlas{
		CellW:      m.CellW,
		CellH:      m.CellH,
		Ascent:     m.Ascent,
		Codepoints: u32le(gunzip(name + ".codepoints.bin")),
		Offsets:    u32le(gunzip(name + ".offsets.bin")),
		Wide:       gunzip(name + ".wide.bin"),
		Pixels:     gunzip(name + ".pixels.bin"),
	}
}

var (
	once sync.Once
	sets map[string]*Set
)

// ForFont returns the atlas set for a render font name; unknown names fall
// back to the default "spleen-5x8".
func ForFont(font string) *Set {
	once.Do(func() {
		meta := loadMeta()
		sets = map[string]*Set{
			"spleen-5x8":        {Bit: load(meta, "spleen-5x8.bit"), Gray: load(meta, "spleen-5x8.gray")},
			"jetbrains-mono-10": {Bit: load(meta, "jbmono10.bit"), Gray: load(meta, "jbmono10.gray")},
			"jetbrains-mono-12": {Bit: load(meta, "jbmono12.bit"), Gray: load(meta, "jbmono12.gray")},
			"jetbrains-mono-14": {Bit: load(meta, "jbmono14.bit"), Gray: load(meta, "jbmono14.gray")},
		}
	})
	if s, ok := sets[font]; ok {
		return s
	}
	return sets["spleen-5x8"]
}

// Default returns the Spleen 5x8 set (the fallback for every other font).
func Default() *Set { return ForFont("spleen-5x8") }
