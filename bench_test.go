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

func BenchmarkFactSheetPatterns(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "render", "multi-page", "input.txt"))
	if err != nil {
		b.Fatal(err)
	}
	var chunks []string
	for _, chunk := range strings.FieldsFunc(string(input), isJSSpace) {
		if n := u16len(chunk); n >= fsMinLen && n <= fsMaxChunk {
			chunks = append(chunks, chunk)
		}
	}
	for i, pattern := range fsPatterns {
		b.Run(strconv.Itoa(i), func(b *testing.B) {
			b.ReportAllocs()
			matches := 0
			for range b.N {
				for _, chunk := range chunks {
					matches += len(pattern.re.FindAllStringSubmatchIndex(chunk, -1))
				}
			}
			factSheetPatternMatches = matches
		})
	}
}
