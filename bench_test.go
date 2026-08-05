package pxpipe

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

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
