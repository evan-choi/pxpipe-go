package pxpipe

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

var factSheetPatternMatches int
var benchmarkCachePrefixSHA string
var benchmarkCachePrefixBytes int
var benchmarkCachePrefixOK bool
var factSheetEntries []FactSheetEntry

func BenchmarkTransformBigClaudeCode(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	opts := &TransformOptions{Model: "claude-fable-5"}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, info := TransformRequest(input, opts)
		if !info.Compressed {
			b.Fatal("expected compression")
		}
	}
}

func BenchmarkCachePrefixDigest(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	body, info := TransformRequest(input, &TransformOptions{Model: "claude-fable-5"})
	if !info.Compressed {
		b.Fatal("expected compression")
	}
	req, err := parseOrderedJSON(body)
	if err != nil {
		b.Fatal(err)
	}
	if _, _, ok := cachePrefixDigest(req); !ok {
		b.Fatal("expected cache prefix")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkCachePrefixSHA, benchmarkCachePrefixBytes, benchmarkCachePrefixOK = cachePrefixDigest(req)
	}
}

func BenchmarkRenderDensePage(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "render", "multi-page", "input.txt"))
	if err != nil {
		b.Fatal(err)
	}
	text := string(input)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := render.RenderTextToImages(text, render.RenderOptions{Reflow: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderColorPage(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "render", "multi-page", "input.txt"))
	if err != nil {
		b.Fatal(err)
	}
	text := string(input)
	style := render.DenseRenderStyle
	style.ColorCycle = true
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := render.RenderTextToImages(text, render.RenderOptions{Reflow: true, Style: &style}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderDensePageParallel(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "render", "multi-page", "input.txt"))
	if err != nil {
		b.Fatal(err)
	}
	text := string(input)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := render.RenderTextToImages(text, render.RenderOptions{Reflow: true}); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkFactSheetPatterns(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "render", "multi-page", "input.txt"))
	if err != nil {
		b.Fatal(err)
	}
	type factSheetBenchChunk struct {
		text     string
		features fsFeature
	}
	var chunks []factSheetBenchChunk
	for _, chunk := range strings.FieldsFunc(string(input), isJSSpace) {
		if n := u16len(chunk); n >= fsMinLen && n <= fsMaxChunk {
			chunks = append(chunks, factSheetBenchChunk{chunk, factSheetFeatures(chunk)})
		}
	}
	for i, pattern := range fsPatterns {
		b.Run(strconv.Itoa(i), func(b *testing.B) {
			b.ReportAllocs()
			matches := 0
			for range b.N {
				for _, chunk := range chunks {
					if chunk.features&pattern.required != pattern.required {
						continue
					}
					if pattern.scan != nil {
						for from := 0; ; {
							_, end := pattern.scan(chunk.text, from)
							if end < 0 {
								break
							}
							matches++
							from = end
						}
						continue
					}
					matches += len(pattern.re.FindAllStringSubmatchIndex(chunk.text, -1))
				}
			}
			factSheetPatternMatches = matches
		})
	}
}

func BenchmarkExtractFactSheetEntries(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "render", "multi-page", "input.txt"))
	if err != nil {
		b.Fatal(err)
	}
	text := string(input)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		factSheetEntries = ExtractFactSheetEntries(text)
	}
}

func BenchmarkTransformOpenAIChat(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "openai", "chat-big-slab", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, info := TransformOpenAIChatCompletions(input, nil)
		if !info.Compressed {
			b.Fatal("expected compression")
		}
	}
}

func BenchmarkTransformOpenAIResponses(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "openai", "responses-codex-pairs", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, info := TransformOpenAIResponses(input, nil)
		if !info.Compressed {
			b.Fatal("expected compression")
		}
	}
}
