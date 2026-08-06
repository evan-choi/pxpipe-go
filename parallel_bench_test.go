package pxpipe

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func BenchmarkTransformBigClaudeCodeParallel(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	opts := &TransformOptions{Model: "claude-fable-5"}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			out, info := TransformRequest(input, opts)
			if !info.Compressed {
				b.Error("expected compression")
				return
			}
			runtime.KeepAlive(out)
		}
	})
}

func BenchmarkTransformOpenAIChatParallel(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "openai", "chat-big-slab", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			out, info := TransformOpenAIChatCompletions(input, nil)
			if !info.Compressed {
				b.Error("expected compression")
				return
			}
			runtime.KeepAlive(out)
		}
	})
}

func BenchmarkTransformOpenAIResponsesParallel(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "openai", "responses-codex-pairs", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			out, info := TransformOpenAIResponses(input, nil)
			if !info.Compressed {
				b.Error("expected compression")
				return
			}
			runtime.KeepAlive(out)
		}
	})
}
