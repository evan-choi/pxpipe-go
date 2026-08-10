package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const renderCacheTestText = "const x = 1;\nfunction f(a, b) { return a + b; }\n// comment line\n"

func renderCacheTestPages(cache *renderedPageCache, text string, style RenderStyle, slotText *string, reflowed bool) ([]*RenderedImage, error) {
	return renderTextToPngsCached(cache, text, 64, 500, style, 96, slotText, reflowed)
}

func TestRenderedPageCacheReturnsIndependentMetadata(t *testing.T) {
	cache := newRenderedPageCache(defaultRenderCacheBytes)
	first, err := renderCacheTestPages(cache, renderCacheTestText, DenseRenderStyle, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderCacheTestPages(cache, renderCacheTestText, DenseRenderStyle, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	stats := cache.stats()
	if stats.entries != 1 || stats.hits != 1 || stats.misses != 1 || stats.bytes <= 0 {
		t.Fatalf("cache stats = %+v", stats)
	}
	if len(first) != len(second) {
		t.Fatalf("pages = %d, want %d", len(second), len(first))
	}
	for i := range first {
		if !bytes.Equal(first[i].PNG, second[i].PNG) || first[i].Width != second[i].Width || first[i].Height != second[i].Height {
			t.Fatalf("page %d differs", i)
		}
	}
	if first[0].base64 == nil || first[0].base64 != second[0].base64 {
		t.Fatal("cached render clones do not share base64 state")
	}
	if got, want := string(first[0].AppendPNGBase64(nil)), base64.StdEncoding.EncodeToString(first[0].PNG); got != want {
		t.Fatalf("base64 = %q, want %q", got, want)
	}
	if first[0].base64.encoded != nil {
		t.Fatal("first use populated the base64 cache")
	}
	second[0].AppendPNGBase64(nil)
	if !first[0].base64.ready.Load() || len(first[0].base64.encoded) == 0 {
		t.Fatal("reused render did not cache base64")
	}

	first[0].Width = 0
	first[0].DroppedCodepoints['A'] = 999
	third, err := renderCacheTestPages(cache, renderCacheTestText, DenseRenderStyle, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if third[0].Width == 0 || third[0].DroppedCodepoints['A'] == 999 {
		t.Fatal("caller mutation leaked into cache")
	}
}

func TestPNGBase64SHA256CachesOnlyExactSequence(t *testing.T) {
	cache := newRenderedPageCache(defaultRenderCacheBytes)
	images := []*RenderedImage{{PNG: []byte("first")}, {PNG: []byte("second")}}
	key := newRenderCacheKey("text", 64, 500, DenseRenderStyle, 96, nil, false)
	first := cache.put(key, "text", nil, images)
	second, ok := cache.get(key, "text", nil)
	if !ok || first[0].sequence == nil || first[0].sequence != second[0].sequence {
		t.Fatal("cached render clones do not share sequence state")
	}
	want := sha256.Sum256([]byte(base64.StdEncoding.EncodeToString(images[0].PNG) + base64.StdEncoding.EncodeToString(images[1].PNG)))
	if got := PNGBase64SHA256(first); got != want || PNGBase64SHA256(second) != want {
		t.Fatalf("sequence digest = %x, want %x", got, want)
	}
	reversedWant := sha256.Sum256([]byte(base64.StdEncoding.EncodeToString(images[1].PNG) + base64.StdEncoding.EncodeToString(images[0].PNG)))
	if got := PNGBase64SHA256([]*RenderedImage{second[1], second[0]}); got != reversedWant {
		t.Fatalf("reversed digest = %x, want %x", got, reversedWant)
	}
}

func TestAppendPNGBase64DeferredWaitsForReuse(t *testing.T) {
	cache := newRenderedPageCache(defaultRenderCacheBytes)
	key := newRenderCacheKey("deferred", 64, 500, DenseRenderStyle, 96, nil, false)
	images := cache.put(key, "deferred", nil, []*RenderedImage{{PNG: []byte("image")}})
	images[0].AppendPNGBase64Deferred(nil)
	images[0].AppendPNGBase64Deferred(nil)
	if images[0].base64.ready.Load() {
		t.Fatal("two uses populated the deferred base64 cache")
	}
	images[0].AppendPNGBase64Deferred(nil)
	if !images[0].base64.ready.Load() {
		t.Fatal("reused image did not populate the deferred base64 cache")
	}
}

func TestRenderCacheKeyCoversEveryRenderInput(t *testing.T) {
	invert, paper := true, 240
	style := RenderStyle{
		Font:          "font",
		Grid:          true,
		GridCols:      2,
		MarkerScale:   3,
		MarkerRed:     true,
		CellHBonus:    4,
		CellWBonus:    5,
		AA:            true,
		ColorCycle:    true,
		ColorByRole:   true,
		InkDilate:     6,
		InkDilateAxis: "both",
		Invert:        &invert,
		PaperGray:     &paper,
	}
	slot := "slots"
	base := newRenderCacheKey("text", 64, 500, style, 96, &slot, false)

	variants := map[string]renderCacheKey{
		"text":        newRenderCacheKey("text!", 64, 500, style, 96, &slot, false),
		"cols":        newRenderCacheKey("text", 63, 500, style, 96, &slot, false),
		"max chars":   newRenderCacheKey("text", 64, 499, style, 96, &slot, false),
		"max height":  newRenderCacheKey("text", 64, 500, style, 95, &slot, false),
		"slot absent": newRenderCacheKey("text", 64, 500, style, 96, nil, false),
		"slot empty":  newRenderCacheKey("text", 64, 500, style, 96, new(string), false),
		"slot text":   newRenderCacheKey("text", 64, 500, style, 96, ptr("other"), false),
		"reflowed":    newRenderCacheKey("text", 64, 500, style, 96, &slot, true),
	}

	styleVariants := []struct {
		name   string
		mutate func(*RenderStyle)
	}{
		{"font", func(s *RenderStyle) { s.Font += "!" }},
		{"grid", func(s *RenderStyle) { s.Grid = !s.Grid }},
		{"grid cols", func(s *RenderStyle) { s.GridCols++ }},
		{"marker scale", func(s *RenderStyle) { s.MarkerScale++ }},
		{"marker red", func(s *RenderStyle) { s.MarkerRed = !s.MarkerRed }},
		{"cell height", func(s *RenderStyle) { s.CellHBonus++ }},
		{"cell width", func(s *RenderStyle) { s.CellWBonus++ }},
		{"aa", func(s *RenderStyle) { s.AA = !s.AA }},
		{"color cycle", func(s *RenderStyle) { s.ColorCycle = !s.ColorCycle }},
		{"color by role", func(s *RenderStyle) { s.ColorByRole = !s.ColorByRole }},
		{"ink dilate", func(s *RenderStyle) { s.InkDilate++ }},
		{"ink axis", func(s *RenderStyle) { s.InkDilateAxis += "!" }},
		{"invert", func(s *RenderStyle) { value := !*s.Invert; s.Invert = &value }},
		{"paper gray", func(s *RenderStyle) { value := *s.PaperGray + 1; s.PaperGray = &value }},
	}
	for _, variant := range styleVariants {
		changed := style
		variant.mutate(&changed)
		variants[variant.name] = newRenderCacheKey("text", 64, 500, changed, 96, &slot, false)
	}
	for name, key := range variants {
		if key == base {
			t.Errorf("%s did not change cache key", name)
		}
	}

	invert2, paper2 := true, 240
	sameValues := style
	sameValues.Invert, sameValues.PaperGray = &invert2, &paper2
	if key := newRenderCacheKey("text", 64, 500, sameValues, 96, &slot, false); key != base {
		t.Fatal("equal pointer values produced different keys")
	}
}

func ptr[T any](value T) *T { return &value }

func TestRenderedPageCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newRenderedPageCache(2_850)
	put := func(text string, fill byte) {
		images := []*RenderedImage{{PNG: bytes.Repeat([]byte{fill}, 600), DroppedCodepoints: map[rune]int{}}}
		cache.put(newRenderCacheKey(text, 64, 500, DenseRenderStyle, 96, nil, false), text, nil, images)
	}
	get := func(text string) bool {
		_, ok := cache.get(newRenderCacheKey(text, 64, 500, DenseRenderStyle, 96, nil, false), text, nil)
		return ok
	}

	put("a", 'a')
	put("b", 'b')
	if !get("a") {
		t.Fatal("a was not cached")
	}
	put("c", 'c')
	if get("b") || !get("a") || !get("c") {
		t.Fatal("cache did not evict the least recently used entry")
	}
	stats := cache.stats()
	if stats.entries != 2 || stats.bytes > cache.maxBytes {
		t.Fatalf("cache stats = %+v", stats)
	}
}

func TestRenderedPageCacheNeverServesHashCollision(t *testing.T) {
	cache := newRenderedPageCache(defaultRenderCacheBytes)
	key := newRenderCacheKey("first", 64, 500, DenseRenderStyle, 96, nil, false)
	first := []*RenderedImage{{PNG: []byte("first")}}
	cache.put(key, "first", nil, first)

	if _, ok := cache.get(key, "second", nil); ok {
		t.Fatal("hash collision returned the wrong entry")
	}
	second := []*RenderedImage{{PNG: []byte("second")}}
	cache.put(key, "second", nil, second)
	got, ok := cache.get(key, "first", nil)
	if !ok || !bytes.Equal(got[0].PNG, first[0].PNG) {
		t.Fatal("hash collision replaced the original entry")
	}
}

func TestRenderedPageCacheBudgetAndDisable(t *testing.T) {
	oversized := newRenderedPageCache(100)
	images := []*RenderedImage{{PNG: make([]byte, 200)}}
	oversized.put(newRenderCacheKey("x", 1, 1, RenderStyle{}, 1, nil, false), "x", nil, images)
	if stats := oversized.stats(); stats.entries != 0 || stats.bytes != 0 {
		t.Fatalf("oversized entry was retained: %+v", stats)
	}

	disabled := newRenderedPageCache(0)
	for range 2 {
		if _, err := renderCacheTestPages(disabled, renderCacheTestText, DenseRenderStyle, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if stats := disabled.stats(); stats != (renderedPageCacheStats{}) {
		t.Fatalf("disabled cache stats = %+v", stats)
	}
}

func TestRenderedPageCacheConcurrentHits(t *testing.T) {
	cache := newRenderedPageCache(defaultRenderCacheBytes)
	text := strings.Repeat(renderCacheTestText, 20)
	want, err := renderCacheTestPages(cache, text, DenseRenderStyle, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	wantBase64 := base64.StdEncoding.EncodeToString(want[0].PNG)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				got, err := renderCacheTestPages(cache, text, DenseRenderStyle, nil, false)
				if err != nil {
					errs <- err
					return
				}
				if len(got) != len(want) || !bytes.Equal(got[0].PNG, want[0].PNG) || string(got[0].AppendPNGBase64(nil)) != wantBase64 {
					errs <- fmt.Errorf("cached render differs")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestRenderCacheBudgetEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"0", 0},
		{" 1024 ", 1024},
		{"-1", defaultRenderCacheBytes},
		{"invalid", defaultRenderCacheBytes},
		{"", defaultRenderCacheBytes},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("PXPIPE_RENDER_CACHE_BYTES", tc.value)
			if got := renderCacheBudget(); got != tc.want {
				t.Fatalf("renderCacheBudget() = %d, want %d", got, tc.want)
			}
		})
	}
}

func BenchmarkRenderDensePageCacheHitParallel(b *testing.B) {
	text, err := os.ReadFile(filepath.Join("..", "testdata", "render", "multi-page", "input.txt"))
	if err != nil {
		b.Fatal(err)
	}
	source := string(text)
	cache := newRenderedPageCache(defaultRenderCacheBytes)
	if _, err := renderTextToPngsCached(cache, source, DenseContentCols, DenseContentCharsPerImage, DenseRenderStyle, MaxHeightPx, nil, false); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := renderTextToPngsCached(cache, source, DenseContentCols, DenseContentCharsPerImage, DenseRenderStyle, MaxHeightPx, nil, false); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
