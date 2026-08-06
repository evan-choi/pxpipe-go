package render

import (
	"bytes"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

func TestWrapLinesPreservesRuneBoundaries(t *testing.T) {
	want := []string{"a" + NLSentinel, "b"}
	if got := WrapLines("a"+NLSentinel+"b", 2, 1, DefaultRenderFont); !slices.Equal(got, want) {
		t.Fatalf("WrapLines() = %q, want %q", got, want)
	}
}

func TestMinifyForRender(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"unchanged", "한글\ntext", "한글\ntext"},
		{"trailing whitespace", "a  \nb\t", "a\nb"},
		{"excess newlines", "a\n\n\n\nb", "a\n\n\nb"},
		{"whitespace line", "a\n \n\n\nb", "a\n\n\nb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MinifyForRender(tc.input); got != tc.want {
				t.Fatalf("MinifyForRender() = %q, want %q", got, tc.want)
			}
		})
	}

	var got string
	if allocs := testing.AllocsPerRun(100, func() { got = MinifyForRender("한글\ntext") }); allocs != 0 {
		t.Fatalf("unchanged input allocated %v times: %q", allocs, got)
	}
}

func TestEscapeMissingGlyphs(t *testing.T) {
	def := atlasSet(DefaultRenderFont).Bit
	for r := rune(0); r < 0x80; r++ {
		if !isEscapeExempt(r) && def.Rank(r) < 0 {
			t.Fatalf("ASCII U+%04X is missing from the default atlas", r)
		}
	}
	if got, want := EscapeMissingGlyphs("ASCII"+NLSentinel), "ASCII"+NLSentinel; got != want {
		t.Fatalf("EscapeMissingGlyphs() = %q, want %q", got, want)
	}
	if got, want := EscapeMissingGlyphs("x\U0010ffff"), "x[U+10FFFF]"; got != want {
		t.Fatalf("EscapeMissingGlyphs() = %q, want %q", got, want)
	}
}

func TestReflowFastPath(t *testing.T) {
	got, ok := Reflow("a\nb")
	if !ok || got != "a"+NLSentinel+"b" {
		t.Fatalf("Reflow() = %q, %v", got, ok)
	}
	if allocs := testing.AllocsPerRun(100, func() { got, ok = Reflow("a\nb") }); allocs > 1 {
		t.Fatalf("Reflow() allocated %v times: %q, %v", allocs, got, ok)
	}
}

func TestReflowForRender(t *testing.T) {
	got, buf, ok := reflowForRender("a\nb")
	if !ok || got != "a"+NLSentinel+"b" || buf == nil {
		t.Fatalf("reflowForRender() = %q, %v, %v", got, buf, ok)
	}
	putReflowBuffer(buf)

	if got, buf, ok = reflowForRender("ab"); !ok || got != "ab" || buf != nil {
		t.Fatalf("reflowForRender(no newline) = %q, %v, %v", got, buf, ok)
	}
	want, wantOK := Reflow("a\tb")
	if got, buf, ok = reflowForRender("a\tb"); ok != wantOK || got != want || buf != nil {
		t.Fatalf("reflowForRender(tab) = %q, %v, %v", got, buf, ok)
	}
	if got, buf, ok = reflowForRender("a" + NLSentinel + "b"); ok || got != "" || buf != nil {
		t.Fatalf("reflowForRender(sentinel) = %q, %v, %v", got, buf, ok)
	}
}

func TestCountTextPagesMatchesRender(t *testing.T) {
	for _, tc := range []struct {
		name        string
		text        string
		cols        int
		style       RenderStyle
		maxHeightPx int
	}{
		{"empty", "", 8, DenseRenderStyle, 80},
		{"wrapped", strings.Repeat("abcdef", 100), 8, DenseRenderStyle, 80},
		{"tabs and unicode", strings.Repeat("a\tb 한글\n", 40), 12, DenseRenderStyle, 96},
		{"large marker", strings.Repeat("a"+NLSentinel+"b", 80), 10, RenderStyle{AA: true, MarkerScale: 2}, 96},
	} {
		t.Run(tc.name, func(t *testing.T) {
			images, err := RenderTextToPngs(tc.text, tc.cols, tc.style, tc.maxHeightPx, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := CountTextPages(tc.text, tc.cols, tc.style, tc.maxHeightPx)
			if got != len(images) {
				t.Fatalf("CountTextPages() = %d, rendered %d pages", got, len(images))
			}
			for limit := 0; limit <= got+1; limit++ {
				if fits := FitsTextPages(tc.text, tc.cols, tc.style, tc.maxHeightPx, limit); fits != (limit > 0 && got <= limit) {
					t.Fatalf("FitsTextPages(maxPages=%d) = %v, pages %d", limit, fits, got)
				}
			}
		})
	}
}

func TestReflowedPagePlanMatchesGeneric(t *testing.T) {
	for _, source := range []string{
		strings.Repeat("alpha\tbeta 한글\n", 80),
		strings.Repeat("trailing   \n\n\n\nmissing 🫠 glyph\n", 80),
	} {
		text, ok := Reflow(source)
		if !ok {
			t.Fatal("expected reflow")
		}
		want, err := RenderTextToPngs(text, 24, DenseRenderStyle, 96, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RenderReflowedTextToPngs(text, 24, DenseRenderStyle, 96)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("RenderReflowedTextToPngs() pages = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i].PNG, want[i].PNG) || got[i].Width != want[i].Width || got[i].Height != want[i].Height || got[i].CharsRendered != want[i].CharsRendered {
				t.Fatalf("RenderReflowedTextToPngs() page %d differs", i)
			}
		}
		for limit := 1; limit <= len(want)+1; limit++ {
			if fits := FitsReflowedTextPages(text, 24, DenseRenderStyle, 96, limit); fits != (len(want) <= limit) {
				t.Fatalf("FitsReflowedTextPages(maxPages=%d) = %v, pages %d", limit, fits, len(want))
			}
		}
	}
}

func TestPageCountersMatchPlans(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("ab 한글🫠\t\n")
	for n := 0; n < 300; n++ {
		runes := make([]rune, rng.Intn(400))
		for i := range runes {
			runes[i] = alphabet[rng.Intn(len(alphabet))]
		}
		text := string(runes)
		cols := rng.Intn(40) + 1
		maxChars := rng.Intn(200) + 1
		maxHeightPx := rng.Intn(200) + 1
		style := DenseRenderStyle
		style.MarkerScale = rng.Intn(3) + 1

		want := len(textPages(text, cols, maxChars, style, maxHeightPx))
		if got := countTextPages(text, cols, maxChars, style, maxHeightPx, 0, false); got != want {
			t.Fatalf("case %d: countTextPages() = %d, want %d", n, got, want)
		}
		for limit := 1; limit <= want+1; limit++ {
			got := countTextPages(text, cols, maxChars, style, maxHeightPx, limit, false) <= limit
			if got != (want <= limit) {
				t.Fatalf("case %d: limited count(maxPages=%d) = %v, want %v", n, limit, got, want <= limit)
			}
		}

		reflowed, ok := Reflow(text)
		if !ok {
			t.Fatalf("case %d: source unexpectedly rejected by Reflow", n)
		}
		generic := textPages(reflowed, cols, maxChars, style, maxHeightPx)
		specialized := reflowedTextPages(reflowed, cols, maxChars, style, maxHeightPx)
		if len(specialized) != len(generic) {
			t.Fatalf("case %d: reflowed pages = %d, generic pages %d", n, len(specialized), len(generic))
		}
		for i := range generic {
			if !slices.Equal(specialized[i], generic[i]) {
				t.Fatalf("case %d page %d: reflowed plan = %q, generic plan %q", n, i, specialized[i], generic[i])
			}
		}
		if got := countTextPages(reflowed, cols, maxChars, style, maxHeightPx, 0, true); got != len(generic) {
			t.Fatalf("case %d: reflowed count = %d, want %d", n, got, len(generic))
		}
	}
}

func TestInvertBytes(t *testing.T) {
	buf := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	want := append([]byte(nil), buf...)
	for i := range want {
		want[i] = 255 - want[i]
	}
	invertBytes(buf)
	if !bytes.Equal(buf, want) {
		t.Fatalf("invertBytes() = %v, want %v", buf, want)
	}
}

type goldenPage struct {
	File   string `json:"file"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type goldenResult struct {
	Pages        []goldenPage `json:"pages"`
	DroppedChars int          `json:"droppedChars"`
	Pixels       int          `json:"pixels"`
}

func decodeToRGBA(t *testing.T, data []byte) *image.RGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png decode: %v", err)
	}
	b := img.Bounds()
	out := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}

func TestGoldenRender(t *testing.T) {
	root := filepath.Join("..", "testdata", "render")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no golden cases")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			dir := filepath.Join(root, e.Name())
			input, err := os.ReadFile(filepath.Join(dir, "input.txt"))
			if err != nil {
				t.Fatal(err)
			}
			optsRaw, err := os.ReadFile(filepath.Join(dir, "opts.json"))
			if err != nil {
				t.Fatal(err)
			}
			var opts RenderOptions
			if err := sonic.Unmarshal(optsRaw, &opts); err != nil {
				t.Fatalf("opts: %v", err)
			}
			wantRaw, err := os.ReadFile(filepath.Join(dir, "result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var want goldenResult
			if err := sonic.Unmarshal(wantRaw, &want); err != nil {
				t.Fatalf("result: %v", err)
			}

			got, err := RenderTextToImages(string(input), opts)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if len(got.Pages) != len(want.Pages) {
				t.Fatalf("pages: got %d want %d", len(got.Pages), len(want.Pages))
			}
			if got.DroppedChars != want.DroppedChars {
				t.Errorf("droppedChars: got %d want %d", got.DroppedChars, want.DroppedChars)
			}
			if got.Pixels != want.Pixels {
				t.Errorf("pixels: got %d want %d", got.Pixels, want.Pixels)
			}
			for i, p := range got.Pages {
				w := want.Pages[i]
				if p.Width != w.Width || p.Height != w.Height {
					t.Errorf("page %d: got %dx%d want %dx%d", i, p.Width, p.Height, w.Width, w.Height)
					continue
				}
				goldenPng, err := os.ReadFile(filepath.Join(dir, w.File))
				if err != nil {
					t.Fatal(err)
				}
				wantImg := decodeToRGBA(t, goldenPng)
				gotImg := decodeToRGBA(t, p.PNG)
				if wantImg.Bounds() != gotImg.Bounds() {
					t.Errorf("page %d bounds: got %v want %v", i, gotImg.Bounds(), wantImg.Bounds())
					continue
				}
				if !bytes.Equal(wantImg.Pix, gotImg.Pix) {
					diff := 0
					first := -1
					for j := range wantImg.Pix {
						if wantImg.Pix[j] != gotImg.Pix[j] {
							diff++
							if first < 0 {
								first = j
							}
						}
					}
					t.Errorf("page %d: %d/%d pixel bytes differ (first at byte %d)", i, diff, len(wantImg.Pix), first)
				}
			}
		})
	}
}
