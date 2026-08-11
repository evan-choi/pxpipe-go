package render

import (
	"crypto/sha256"
	"encoding/base64"
	"hash/maphash"
	"io"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	defaultRenderCacheBytes   int64 = 64 << 20
	renderCacheAdmissionSlots       = 4096
)

type renderCacheStyleKey struct {
	font          string
	inkDilateAxis string
	gridCols      int
	markerScale   int
	cellHBonus    int
	cellWBonus    int
	inkDilate     int
	paperGray     int
	grid          bool
	markerRed     bool
	aa            bool
	colorCycle    bool
	colorByRole   bool
	invert        bool
	invertSet     bool
	paperGraySet  bool
}

type renderCacheKey struct {
	textHash        uint64
	slotHash        uint64
	style           renderCacheStyleKey
	cols            int
	maxCharsPerPage int
	maxHeightPx     int
	pageLines       int
	pageSlotLines   int
	slotPresent     bool
	reflowed        bool
	page            bool
}

var renderCacheHashSeed = maphash.MakeSeed()

func newRenderCacheKey(text string, cols, maxCharsPerPage int, style RenderStyle, maxHeightPx int, slotText *string, reflowed bool) renderCacheKey {
	key := renderCacheKey{
		textHash:        maphash.String(renderCacheHashSeed, text),
		cols:            cols,
		maxCharsPerPage: maxCharsPerPage,
		maxHeightPx:     maxHeightPx,
		reflowed:        reflowed,
		style: renderCacheStyleKey{
			font:          style.Font,
			inkDilateAxis: style.InkDilateAxis,
			gridCols:      style.GridCols,
			markerScale:   style.MarkerScale,
			cellHBonus:    style.CellHBonus,
			cellWBonus:    style.CellWBonus,
			inkDilate:     style.InkDilate,
			grid:          style.Grid,
			markerRed:     style.MarkerRed,
			aa:            style.AA,
			colorCycle:    style.ColorCycle,
			colorByRole:   style.ColorByRole,
		},
	}
	if style.Invert != nil {
		key.style.invertSet = true
		key.style.invert = *style.Invert
	}
	if style.PaperGray != nil {
		key.style.paperGraySet = true
		key.style.paperGray = *style.PaperGray
	}
	if slotText != nil {
		key.slotPresent = true
		key.slotHash = maphash.String(renderCacheHashSeed, *slotText)
	}
	return key
}

func newRenderPageCacheKey(text string, lines, cols int, style RenderStyle, slotText *string, slotLines int) renderCacheKey {
	key := newRenderCacheKey(text, cols, 0, style, 0, slotText, false)
	key.page = true
	key.pageLines = lines
	key.pageSlotLines = slotLines
	return key
}

type renderedPageCacheEntry struct {
	text        string
	slotText    string
	slotPresent bool
	images      []*RenderedImage
	bytes       int64
	used        atomic.Uint64
}

func (e *renderedPageCacheEntry) matches(text string, slotText *string) bool {
	if e.text != text || e.slotPresent != (slotText != nil) {
		return false
	}
	return slotText == nil || e.slotText == *slotText
}

type renderedPageCache struct {
	maxBytes int64
	values   sync.Map
	// ponytail: fixed admission slots may delay caching under heavy collision;
	// increase the table only if cache-hit telemetry regresses.
	admissions [renderCacheAdmissionSlots]atomic.Uint64
	mu         sync.Mutex
	entries    atomic.Int64
	bytes      atomic.Int64
	hits       atomic.Uint64
	misses     atomic.Uint64
	clock      atomic.Uint64
}

type renderedPageCacheStats struct {
	entries int64
	bytes   int64
	hits    uint64
	misses  uint64
}

func newRenderedPageCache(maxBytes int64) *renderedPageCache {
	return &renderedPageCache{maxBytes: max(0, maxBytes)}
}

func (c *renderedPageCache) stats() renderedPageCacheStats {
	return renderedPageCacheStats{
		entries: c.entries.Load(),
		bytes:   c.bytes.Load(),
		hits:    c.hits.Load(),
		misses:  c.misses.Load(),
	}
}

func (c *renderedPageCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values.Range(func(key, _ any) bool {
		c.values.Delete(key)
		return true
	})
	for i := range c.admissions {
		c.admissions[i].Store(0)
	}
	c.entries.Store(0)
	c.bytes.Store(0)
	c.hits.Store(0)
	c.misses.Store(0)
}

func (c *renderedPageCache) admit(key renderCacheKey) bool {
	fingerprint := key.textHash ^ key.slotHash ^ uint64(uint32(key.cols))<<32 ^ uint64(uint32(key.maxCharsPerPage))
	fingerprint ^= uint64(uint32(key.maxHeightPx)) * 0x9e3779b97f4a7c15
	if key.page {
		fingerprint ^= 0xd6e8feb86659fd93 ^ uint64(uint32(key.pageLines))<<32 ^ uint64(uint32(key.pageSlotLines))
	}
	if key.reflowed {
		fingerprint ^= 0xa0761d6478bd642f
	}
	if fingerprint == 0 {
		fingerprint = 1
	}
	slot := &c.admissions[fingerprint&(renderCacheAdmissionSlots-1)]
	if slot.Load() == fingerprint {
		return true
	}
	slot.Store(fingerprint)
	return false
}

type renderedImageBase64 struct {
	once    sync.Once
	uses    atomic.Uint32
	ready   atomic.Bool
	encoded []byte
}

type renderedImageSequence struct {
	images int
	once   sync.Once
	sum    [sha256.Size]byte
}

// AppendPNGBase64 appends the image's base64 payload to dst. Cached render
// clones share the encoding; uncached images encode directly into dst.
func (image *RenderedImage) appendPNGBase64(dst []byte, directUses uint32) []byte {
	cache := image.base64
	if cache == nil {
		return base64.StdEncoding.AppendEncode(dst, image.PNG)
	}
	if cache.ready.Load() {
		return append(dst, cache.encoded...)
	}
	if cache.uses.Add(1) <= directUses {
		return base64.StdEncoding.AppendEncode(dst, image.PNG)
	}
	cache.once.Do(func() {
		cache.encoded = make([]byte, base64.StdEncoding.EncodedLen(len(image.PNG)))
		base64.StdEncoding.Encode(cache.encoded, image.PNG)
		cache.ready.Store(true)
	})
	return append(dst, cache.encoded...)
}

func (image *RenderedImage) AppendPNGBase64(dst []byte) []byte {
	return image.appendPNGBase64(dst, 1)
}

// AppendPNGBase64Deferred avoids retaining an encoded copy until an image is
// used more than twice, which keeps one-shot multi-pass transforms lean.
func (image *RenderedImage) AppendPNGBase64Deferred(dst []byte) []byte {
	return image.appendPNGBase64(dst, 2)
}

// WritePNGBase64 writes the image's base64 payload to dst.
func (image *RenderedImage) WritePNGBase64(dst io.Writer) error {
	cache := image.base64
	if cache != nil && cache.ready.Load() {
		_, err := dst.Write(cache.encoded)
		return err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, dst)
	if _, err := encoder.Write(image.PNG); err != nil {
		return err
	}
	return encoder.Close()
}

func pngBase64SHA256(images []*RenderedImage) (sum [sha256.Size]byte) {
	h := sha256.New()
	for _, image := range images {
		_ = image.WritePNGBase64(h)
	}
	_ = h.Sum(sum[:0])
	return sum
}

// PNGBase64SHA256 hashes the concatenated base64 payloads. Complete image
// sequences returned by the render cache share the exact digest.
func PNGBase64SHA256(images []*RenderedImage) [sha256.Size]byte {
	if len(images) > 0 {
		sequence := images[0].sequence
		if sequence != nil && sequence.images == len(images) {
			exact := true
			for i, image := range images {
				if image.sequence != sequence || image.sequenceIndex != i {
					exact = false
					break
				}
			}
			if exact {
				sequence.once.Do(func() { sequence.sum = pngBase64SHA256(images) })
				return sequence.sum
			}
		}
	}
	return pngBase64SHA256(images)
}

func cloneRenderedImages(images []*RenderedImage) []*RenderedImage {
	out := make([]*RenderedImage, len(images))
	for i, image := range images {
		clone := *image
		clone.DroppedCodepoints = maps.Clone(image.DroppedCodepoints)
		out[i] = &clone
	}
	return out
}

func (c *renderedPageCache) get(key renderCacheKey, text string, slotText *string) ([]*RenderedImage, bool) {
	if c.maxBytes == 0 {
		return nil, false
	}
	value, ok := c.values.Load(key)
	if !ok || !value.(*renderedPageCacheEntry).matches(text, slotText) {
		c.misses.Add(1)
		return nil, false
	}
	entry := value.(*renderedPageCacheEntry)
	entry.used.Store(c.clock.Add(1))
	c.hits.Add(1)
	return cloneRenderedImages(entry.images), true
}

func renderCacheEntryBytes(text string, slotText *string, images []*RenderedImage) int64 {
	retained := int64(len(text))
	if slotText != nil {
		retained += int64(len(*slotText))
	}
	for _, image := range images {
		retained += int64(len(image.PNG)) + int64(base64.StdEncoding.EncodedLen(len(image.PNG)))
	}
	return retained
}

type renderCacheEvictionCandidate struct {
	key   renderCacheKey
	entry *renderedPageCacheEntry
}

func (c *renderedPageCache) put(key renderCacheKey, text string, slotText *string, images []*RenderedImage) []*RenderedImage {
	retained := renderCacheEntryBytes(text, slotText, images)
	if c.maxBytes == 0 || retained > c.maxBytes {
		return images
	}

	c.mu.Lock()
	if value, ok := c.values.Load(key); ok {
		entry := value.(*renderedPageCacheEntry)
		if entry.matches(text, slotText) {
			entry.used.Store(c.clock.Add(1))
			c.mu.Unlock()
			return cloneRenderedImages(entry.images)
		}
		// A digest collision is a miss, never permission to replace or serve the
		// other source text.
		c.mu.Unlock()
		return images
	}

	if c.bytes.Load()+retained > c.maxBytes {
		candidates := make([]renderCacheEvictionCandidate, 0, c.entries.Load())
		c.values.Range(func(key, value any) bool {
			candidates = append(candidates, renderCacheEvictionCandidate{
				key:   key.(renderCacheKey),
				entry: value.(*renderedPageCacheEntry),
			})
			return true
		})
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].entry.used.Load() < candidates[j].entry.used.Load()
		})
		for _, candidate := range candidates {
			if c.bytes.Load()+retained <= c.maxBytes {
				break
			}
			if c.values.CompareAndDelete(candidate.key, candidate.entry) {
				c.entries.Add(-1)
				c.bytes.Add(-candidate.entry.bytes)
			}
		}
	}

	storedKey := key
	storedKey.style.font = strings.Clone(key.style.font)
	storedKey.style.inkDilateAxis = strings.Clone(key.style.inkDilateAxis)
	sequence := &renderedImageSequence{images: len(images)}
	for i, image := range images {
		if image.base64 == nil {
			image.base64 = &renderedImageBase64{}
		}
		image.sequence = sequence
		image.sequenceIndex = i
	}
	entry := &renderedPageCacheEntry{
		text:        strings.Clone(text),
		slotPresent: slotText != nil,
		images:      images,
		bytes:       retained,
	}
	if slotText != nil {
		entry.slotText = strings.Clone(*slotText)
	}
	entry.used.Store(c.clock.Add(1))
	c.values.Store(storedKey, entry)
	c.entries.Add(1)
	c.bytes.Add(retained)
	c.mu.Unlock()
	return cloneRenderedImages(images)
}

func (c *renderedPageCache) putRepeated(key renderCacheKey, text string, slotText *string, images []*RenderedImage) []*RenderedImage {
	if !c.admit(key) {
		return images
	}
	return c.put(key, text, slotText, images)
}

func renderCacheBudget() int64 {
	raw, ok := os.LookupEnv("PXPIPE_RENDER_CACHE_BYTES")
	if !ok || strings.TrimSpace(raw) == "" {
		return defaultRenderCacheBytes
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 {
		return defaultRenderCacheBytes
	}
	return value
}

var renderResultCache = newRenderedPageCache(renderCacheBudget())

func renderTextToPngsCached(cache *renderedPageCache, text string, cols, maxCharsPerImage int, style RenderStyle, maxHeightPx int, slotText *string, reflowed bool) ([]*RenderedImage, error) {
	if cache.maxBytes == 0 {
		return renderTextToPngsWithCharLimitUncached(text, cols, maxCharsPerImage, style, maxHeightPx, slotText, reflowed)
	}
	key := newRenderCacheKey(text, cols, maxCharsPerImage, style, maxHeightPx, slotText, reflowed)
	if images, ok := cache.get(key, text, slotText); ok {
		return images, nil
	}
	// ponytail: simultaneous cold misses may render twice; add per-key flights
	// only if a production cold-start profile shows stampedes.
	images, err := renderTextToPngsWithCharLimitCachedPages(text, cols, maxCharsPerImage, style, maxHeightPx, slotText, reflowed, cache)
	if err != nil {
		return nil, err
	}
	return cache.putRepeated(key, text, slotText, images), nil
}
