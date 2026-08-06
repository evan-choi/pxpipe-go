package o200k

import (
	"strings"
	"sync"
	"testing"
)

func TestCountTokensMatchesGptTokenizer(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"hello world", 2},
		{"const x = foo(bar, 42); // baz", 11},
		{"프록시는 요청 본문을 PNG 이미지로 변환합니다.", 14},
		{"<|endoftext|>", 7},
		{"a<|endofprompt|>b", 9},
	}
	for _, c := range cases {
		if got := CountTokens(c.text); got != c.want {
			t.Errorf("CountTokens(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestCountTokensCacheHitAllocatesNothing(t *testing.T) {
	text := strings.Repeat("stable EKS prompt payload ", 256)
	want := CountTokens(text)
	var got int
	if allocs := testing.AllocsPerRun(100, func() { got = CountTokens(text) }); allocs != 0 {
		t.Fatalf("warm CountTokens allocated %v times", allocs)
	}
	if got != want {
		t.Fatalf("warm CountTokens = %d, want %d", got, want)
	}
}

func TestCountTokensCacheConcurrent(t *testing.T) {
	text := strings.Repeat("shared pod prompt ", 128)
	want := CountTokens(text)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := CountTokens(text); got != want {
				t.Errorf("CountTokens = %d, want %d", got, want)
			}
		}()
	}
	wg.Wait()
}
