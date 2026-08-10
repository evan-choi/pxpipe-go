package o200k

import (
	"strings"
	"sync/atomic"
	"testing"
)

var highCardinalityBenchmarkSerial atomic.Uint64

func benchmarkCountTokensVariants(b *testing.B, alphabet string) {
	b.Helper()
	scratch := []byte(strings.Repeat("Agent Operating Manual section 123 with stable payload.\n", 512) + "PVAR=aaaaaaaa")
	marker := len(scratch) - 8
	b.SetBytes(int64(len(scratch)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		n := highCardinalityBenchmarkSerial.Add(1)
		for i := len(scratch) - 1; i >= marker; i-- {
			scratch[i] = alphabet[n%uint64(len(alphabet))]
			n /= uint64(len(alphabet))
		}
		if CountTokens(string(scratch)) == 0 {
			b.Fatal("expected tokens")
		}
	}
}

func BenchmarkCountTokensHighCardinality(b *testing.B) {
	benchmarkCountTokensVariants(b, "abcdefghijklmnopqrstuvwxyz")
}

func BenchmarkCountTokensNumericVariants(b *testing.B) {
	benchmarkCountTokensVariants(b, "0123456789")
}
