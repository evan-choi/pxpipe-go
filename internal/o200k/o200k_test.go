package o200k

import "testing"

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
