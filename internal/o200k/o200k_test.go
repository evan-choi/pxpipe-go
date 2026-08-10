package o200k

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCountTokensDigitNormalizationPreservesExactCount(t *testing.T) {
	e, err := encoding()
	if err != nil {
		t.Fatal(err)
	}
	for width := 1; width <= 3; width++ {
		limit := 1
		for range width {
			limit *= 10
		}
		for n := 0; n < 1000; n++ {
			digits := fmt.Sprintf("%0*d", width, n%limit)
			for _, text := range []string{
				digits,
				"id=" + digits + "; next",
				"prefix" + digits + "suffix",
				"↵id=" + digits,
			} {
				want, err := e.Count(text)
				if err != nil {
					t.Fatal(err)
				}
				if got := CountTokens(text); got != want {
					t.Fatalf("CountTokens(%q) = %d, want %d", text, got, want)
				}
			}
		}
	}
	for width := 4; width <= 32; width++ {
		for _, digits := range []string{
			strings.Repeat("0", width),
			strings.Repeat("9", width),
			strings.Repeat("1234567890", width/10+1)[:width],
		} {
			want, err := e.Count("prefix" + digits + "suffix")
			if err != nil {
				t.Fatal(err)
			}
			if got := CountTokens("prefix" + digits + "suffix"); got != want {
				t.Fatalf("CountTokens(%q) = %d, want %d", digits, got, want)
			}
		}
	}
}

func TestDigitNormalizationRejectsMixedUnicodeNumbers(t *testing.T) {
	if !canNormalizeDigits("↵ 123") {
		t.Error("canNormalizeDigits rejected non-number Unicode")
	}
	for _, text := range []string{"१२3", "3²", "Ⅳ4"} {
		if canNormalizeDigits(text) {
			t.Errorf("canNormalizeDigits(%q) = true", text)
		}
	}
}

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
