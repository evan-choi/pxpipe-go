package render

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
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

func TestReflowFastPath(t *testing.T) {
	got, ok := Reflow("a\nb")
	if !ok || got != "a"+NLSentinel+"b" {
		t.Fatalf("Reflow() = %q, %v", got, ok)
	}
	if allocs := testing.AllocsPerRun(100, func() { got, ok = Reflow("a\nb") }); allocs > 1 {
		t.Fatalf("Reflow() allocated %v times: %q, %v", allocs, got, ok)
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
