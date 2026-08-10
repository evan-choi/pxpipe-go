package pxpipe

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

func benchmarkVariantScratch(b *testing.B, input []byte, anchor string) ([]byte, int) {
	b.Helper()
	const replacement = "PVAR=00000000"
	scratch := bytes.Replace(input, []byte(anchor), []byte(replacement), 1)
	marker := bytes.Index(scratch, []byte(replacement))
	if marker < 0 {
		b.Fatalf("benchmark fixture is missing %q", anchor)
	}
	return scratch, marker + len("PVAR=")
}

func putBenchmarkSerial(dst []byte, n uint64) {
	n %= 100_000_000
	for i := len(dst) - 1; i >= 0; i-- {
		dst[i] = byte('0' + n%10)
		n /= 10
	}
}

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

func BenchmarkTransformBigClaudeCodeHighCardinalityParallel(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "transform", "big-claude-code", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	opts := &TransformOptions{Model: "claude-fable-5"}
	var serial atomic.Uint64
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		scratch, marker := benchmarkVariantScratch(b, input, "You are Claude Code")
		for pb.Next() {
			putBenchmarkSerial(scratch[marker:marker+8], serial.Add(1))
			out, info := TransformRequest(scratch, opts)
			if !info.Compressed {
				b.Error("expected compression")
				return
			}
			runtime.KeepAlive(out)
		}
	})
}

func BenchmarkTransformOpenAIChatHighCardinalityParallel(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "openai", "chat-big-slab", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	var serial atomic.Uint64
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		scratch, marker := benchmarkVariantScratch(b, input, "Agent Operating Manual")
		for pb.Next() {
			putBenchmarkSerial(scratch[marker:marker+8], serial.Add(1))
			out, info := TransformOpenAIChatCompletions(scratch, nil)
			if !info.Compressed {
				b.Error("expected compression")
				return
			}
			runtime.KeepAlive(out)
		}
	})
}

func BenchmarkTransformOpenAIResponsesHighCardinalityParallel(b *testing.B) {
	input, err := os.ReadFile(filepath.Join("testdata", "openai", "responses-codex-pairs", "input.json"))
	if err != nil {
		b.Fatal(err)
	}
	var serial atomic.Uint64
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		scratch, marker := benchmarkVariantScratch(b, input, "Agent Operating Manual")
		for pb.Next() {
			putBenchmarkSerial(scratch[marker:marker+8], serial.Add(1))
			out, info := TransformOpenAIResponses(scratch, nil)
			if !info.Compressed {
				b.Error("expected compression")
				return
			}
			runtime.KeepAlive(out)
		}
	})
}
