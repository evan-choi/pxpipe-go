package o200k

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dlclark/regexp2/v2"
	"github.com/tiktoken-go/tokenizer"
)

func TestCountTokensDigitNormalizationPreservesExactCount(t *testing.T) {
	e, err := tokenizer.Get(tokenizer.O200kBase)
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

func TestCounterMatchesTokenizerAndSplitPattern(t *testing.T) {
	const pattern = `[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+(?i:'s|'t|'re|'ve|'m|'ll|'d)?|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*(?i:'s|'t|'re|'ve|'m|'ll|'d)?|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n/]*|\s*[\r\n]+|\s+(?!\S)|\s+`
	reference, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatal(err)
	}
	splitter := regexp2.MustCompile(pattern, regexp2.None)
	alphabet := []rune("aAzZ09 '\t\r\n/!éÉǅʰ中\u0301ा⃝²Ⅳ٣\u0085\u00a0\u2028🙂ſ")
	inputs := []string{
		"hello world",
		"ABCDef AbC ABC 中日語 don't WE'LL",
		"  words  1234 /\r\n/ symbols",
		"\u0301 \u0301A A\u0301 a\u0301 École",
		string([]byte{0xff, 'a', 0xfe}),
	}
	rng := rand.New(rand.NewSource(1))
	for range 2_000 {
		var b strings.Builder
		for range 1 + rng.Intn(48) {
			b.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		inputs = append(inputs, b.String())
	}
	for _, text := range inputs {
		wantPieces, err := regexpPieces(splitter, text)
		if err != nil {
			t.Fatal(err)
		}
		if got := scannerPieces(text); !slices.Equal(got, wantPieces) {
			t.Fatalf("split %q = %#v, want %#v", text, got, wantPieces)
		}
		want, err := reference.Count(text)
		if err != nil {
			t.Fatal(err)
		}
		got, err := countTokensUncached(text)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("count %q = %d, want %d", text, got, want)
		}
	}
}

func regexpPieces(splitter *regexp2.Regexp, text string) ([]string, error) {
	var pieces []string
	match, err := splitter.FindStringMatch(text)
	for match != nil && err == nil {
		pieces = append(pieces, match.String())
		match, err = splitter.FindNextMatch(match)
	}
	return pieces, err
}

func scannerPieces(text string) []string {
	text = normalizeText(text)
	var pieces []string
	for start := 0; start < len(text); {
		end := nextPieceEnd(text, start)
		pieces = append(pieces, text[start:end])
		start = end
	}
	return pieces
}

func TestDigitNormalizationRejectsMixedUnicodeNumbers(t *testing.T) {
	want, ok := digitNormalizedKeyIfSafe("↵ 123")
	if !ok {
		t.Fatal("digitNormalizedKeyIfSafe rejected ASCII digits")
	}
	if got, ok := digitNormalizedKeyIfSafe("↵ 987"); !ok || got != want {
		t.Fatal("ASCII digit variants did not share a normalized key")
	}
	for _, text := range []string{"१२3", "3²", "Ⅳ4"} {
		if _, ok := digitNormalizedKeyIfSafe(text); ok {
			t.Errorf("digitNormalizedKeyIfSafe(%q) accepted mixed Unicode numbers", text)
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

func TestExactTokenCountCacheAdmitsVerifiedRepeat(t *testing.T) {
	text := strings.Repeat("verified repeated prompt payload ", 512)
	fingerprint := tokenCountFingerprint(text)
	exactSlot := &exactTokenCountCache[fingerprint&(exactTokenCountCacheSlots-1)]
	key := sha256.Sum256([]byte(text))
	digestSlot := &tokenCountCache[binary.LittleEndian.Uint64(key[:])&(tokenCountCacheSlots-1)]
	previousExact := exactSlot.Swap(nil)
	previousDigest := digestSlot.Swap(nil)
	defer exactSlot.Store(previousExact)
	defer digestSlot.Store(previousDigest)

	want := CountTokens(text)
	if cached := exactSlot.Load(); cached != nil {
		t.Fatal("first observation entered exact cache")
	}
	if got := CountTokens(text); got != want {
		t.Fatalf("repeated CountTokens = %d, want %d", got, want)
	}
	if cached := exactSlot.Load(); cached == nil || cached.text != text || cached.count != want {
		t.Fatalf("exact cache entry = %#v, want count %d", cached, want)
	}
}

func TestExactTokenCountCacheRejectsSampleCollision(t *testing.T) {
	text := "exact collision candidate"
	fingerprint := tokenCountFingerprint(text)
	slot := &exactTokenCountCache[fingerprint&(exactTokenCountCacheSlots-1)]
	previous := slot.Swap(&exactTokenCountCacheEntry{fingerprint: fingerprint, text: text + "!", count: -1})
	defer slot.Store(previous)
	if got := CountTokens(text); got < 0 {
		t.Fatalf("sample collision returned cached count %d", got)
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
