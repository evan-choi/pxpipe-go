package render

import (
	"hash/maphash"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const defaultRenderCacheBytes int64 = 64 << 20

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
	slotPresent     bool
	reflowed        bool
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
	mu       sync.Mutex
	entries  atomic.Int64
	bytes    atomic.Int64
	hits     atomic.Uint64
	misses   atomic.Uint64
	clock    atomic.Uint64
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
	c.entries.Store(0)
	c.bytes.Store(0)
	c.hits.Store(0)
	c.misses.Store(0)
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
		retained += int64(len(image.PNG))
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
	images, err := renderTextToPngsWithCharLimitUncached(text, cols, maxCharsPerImage, style, maxHeightPx, slotText, reflowed)
	if err != nil {
		return nil, err
	}
	return cache.put(key, text, slotText, images), nil
}
