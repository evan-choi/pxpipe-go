package pxpipe

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

type goldenOpts struct {
	Model                      string   `json:"model"`
	Compress                   *bool    `json:"compress"`
	CharsPerToken              *float64 `json:"charsPerToken"`
	EmitRecoverable            bool     `json:"emitRecoverable"`
	KeepSharp                  string   `json:"keepSharp"`
	HistoryAmortizationHorizon *int     `json:"historyAmortizationHorizon"`
}

func loadGoldenOpts(t *testing.T, dir string) *TransformOptions {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "opts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g goldenOpts
	if err := sonic.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	opts := &TransformOptions{
		Model:                      g.Model,
		Compress:                   g.Compress,
		CharsPerToken:              g.CharsPerToken,
		EmitRecoverable:            g.EmitRecoverable,
		HistoryAmortizationHorizon: g.HistoryAmortizationHorizon,
	}
	if strings.HasPrefix(g.KeepSharp, "contains:") {
		needle := strings.TrimPrefix(g.KeepSharp, "contains:")
		opts.KeepSharp = func(b KeepSharpBlock) bool { return strings.Contains(b.Text, needle) }
	}
	return opts
}

func decodePNGPixels(t *testing.T, data []byte) *image.RGBA {
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

// isImageBlock reports whether v is an Anthropic base64 image block.
func isImageBlock(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok || m["type"] != "image" {
		return nil, false
	}
	src, ok := m["source"].(map[string]any)
	if !ok || src["type"] != "base64" {
		return nil, false
	}
	return m, true
}

// deepCompare walks want/got in parallel; image blocks compare by decoded
// pixels, everything else by exact value.
func deepCompare(t *testing.T, path string, want, got any) {
	t.Helper()
	if wm, ok := isImageBlock(want); ok {
		gm, gok := isImageBlock(got)
		if !gok {
			t.Errorf("%s: want image block, got %T", path, got)
			return
		}
		wsrc := wm["source"].(map[string]any)
		gsrc := gm["source"].(map[string]any)
		wdata, _ := base64.StdEncoding.DecodeString(wsrc["data"].(string))
		gdata, _ := base64.StdEncoding.DecodeString(gsrc["data"].(string))
		wimg := decodePNGPixels(t, wdata)
		gimg := decodePNGPixels(t, gdata)
		if wimg.Bounds() != gimg.Bounds() {
			t.Errorf("%s: image bounds %v != %v", path, gimg.Bounds(), wimg.Bounds())
			return
		}
		if !bytes.Equal(wimg.Pix, gimg.Pix) {
			t.Errorf("%s: image pixels differ", path)
		}
		if mt := wsrc["media_type"]; mt != gsrc["media_type"] {
			t.Errorf("%s: media_type %v != %v", path, gsrc["media_type"], mt)
		}
		deepCompare(t, path+".cache_control", wm["cache_control"], gm["cache_control"])
		return
	}
	switch wv := want.(type) {
	case map[string]any:
		gv, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: want object, got %T", path, got)
			return
		}
		for k := range wv {
			if _, has := gv[k]; !has {
				t.Errorf("%s: missing key %q", path, k)
			}
		}
		for k := range gv {
			if _, has := wv[k]; !has {
				t.Errorf("%s: extra key %q", path, k)
			}
		}
		for k, w := range wv {
			deepCompare(t, path+"."+k, w, gv[k])
		}
	case []any:
		gv, ok := got.([]any)
		if !ok {
			t.Errorf("%s: want array, got %T", path, got)
			return
		}
		if len(wv) != len(gv) {
			t.Errorf("%s: array len %d != %d", path, len(gv), len(wv))
			return
		}
		for i := range wv {
			deepCompare(t, fmt.Sprintf("%s[%d]", path, i), wv[i], gv[i])
		}
	default:
		wj, _ := sonic.Marshal(want)
		gj, _ := sonic.Marshal(got)
		if string(wj) != string(gj) {
			ws, gs := string(wj), string(gj)
			if len(ws) > 200 {
				ws = ws[:200] + "…"
			}
			if len(gs) > 200 {
				gs = gs[:200] + "…"
			}
			t.Errorf("%s: %s != want %s", path, gs, ws)
		}
	}
}

type goldenInfo struct {
	Compressed        bool   `json:"compressed"`
	Reason            string `json:"reason"`
	OrigChars         int    `json:"origChars"`
	CompressedChars   int    `json:"compressedChars"`
	ImageCount        int    `json:"imageCount"`
	ImageBytes        int    `json:"imageBytes"`
	ImagePixels       int    `json:"imagePixels"`
	NativeImages      int    `json:"nativeImages"`
	ImageBudgetSkips  int    `json:"imageBudgetSkips"`
	WireImages        int    `json:"wireImages"`
	StaticChars       int    `json:"staticChars"`
	DynamicChars      int    `json:"dynamicChars"`
	DynamicBlockCount int    `json:"dynamicBlockCount"`
	DroppedChars      int    `json:"droppedChars"`
	FirstUserSha8     string `json:"firstUserSha8"`
	SystemSha8        string `json:"systemSha8"`
	ToolDocsChars     int    `json:"toolDocsChars"`
	OutgoingTextChars int    `json:"outgoingTextChars"`
	GateEval          *struct {
		Site          string  `json:"site"`
		ImageTokens   float64 `json:"imageTokens"`
		TextTokens    float64 `json:"textTokens"`
		BurnImageSide float64 `json:"burnImageSide"`
		BurnTextSide  float64 `json:"burnTextSide"`
		Profitable    bool    `json:"profitable"`
	} `json:"gateEval"`
	BucketChars            map[string]int `json:"bucketChars"`
	PassthroughReasons     map[string]int `json:"passthroughReasons"`
	CollapsedTurns         int            `json:"collapsedTurns"`
	CollapsedChars         int            `json:"collapsedChars"`
	CollapsedImages        int            `json:"collapsedImages"`
	HistoryReason          string         `json:"historyReason"`
	HistoryTextChars       int            `json:"historyTextChars"`
	HistoryImageSha        string         `json:"historyImageSha"`
	HistoryFreezeStep      int            `json:"historyFreezeStep"`
	HistoryPackFill        bool           `json:"historyPackFill"`
	HistoryBudgetTrimmed   bool           `json:"historyBudgetTrimmed"`
	CachePrefixSha8        string         `json:"cachePrefixSha8"`
	CachePrefixBytes       int            `json:"cachePrefixBytes"`
	CachePrefixToolsSha8   string         `json:"cachePrefixToolsSha8"`
	CachePrefixSystemSha8  string         `json:"cachePrefixSystemSha8"`
	CachePrefixHeadSha8    string         `json:"cachePrefixHeadSha8"`
	CachePrefixMarkedSha8  string         `json:"cachePrefixMarkedSha8"`
	CachePrefixMarkedBytes int            `json:"cachePrefixMarkedBytes"`
	CachePrefixMarkerPos   string         `json:"cachePrefixMarkerPos"`
	ImageSourceText        string         `json:"imageSourceText"`
	ImageDims              []imageDim     `json:"imageDims"`
	FirstImageWidth        int            `json:"firstImageWidth"`
	FirstImageHeight       int            `json:"firstImageHeight"`
	TruncatedToolResults   int            `json:"truncatedToolResults"`
	OmittedChars           int            `json:"omittedChars"`
	KeptSharpBlocks        int            `json:"keptSharpBlocks"`
	PinChars               int            `json:"pinChars"`
	Recoverable            []struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		ToolUseID  string `json:"toolUseId"`
		Text       string `json:"text"`
		ImageCount int    `json:"imageCount"`
	} `json:"recoverable"`
}

func eqInt(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %d want %d", name, got, want)
	}
}

func eqStr(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q want %q", name, got, want)
	}
}

func TestGoldenTransform(t *testing.T) {
	root := filepath.Join("testdata", "transform")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
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
			input, err := os.ReadFile(filepath.Join(dir, "input.json"))
			if err != nil {
				t.Fatal(err)
			}
			opts := loadGoldenOpts(t, dir)
			wantOut, err := os.ReadFile(filepath.Join(dir, "output.json"))
			if err != nil {
				t.Fatal(err)
			}
			wantInfoRaw, err := os.ReadFile(filepath.Join(dir, "info.json"))
			if err != nil {
				t.Fatal(err)
			}
			var wantInfo goldenInfo
			if err := sonic.Unmarshal(wantInfoRaw, &wantInfo); err != nil {
				t.Fatal(err)
			}

			gotBody, info := TransformRequest(input, opts)

			if info.Compressed != wantInfo.Compressed {
				t.Fatalf("compressed: got %v want %v (reason %q)", info.Compressed, wantInfo.Compressed, info.Reason)
			}
			eqStr(t, "reason", info.Reason, wantInfo.Reason)
			eqInt(t, "origChars", info.OrigChars, wantInfo.OrigChars)
			eqInt(t, "compressedChars", info.CompressedChars, wantInfo.CompressedChars)
			eqInt(t, "imageCount", info.ImageCount, wantInfo.ImageCount)
			eqInt(t, "imagePixels", info.ImagePixels, wantInfo.ImagePixels)
			eqInt(t, "nativeImages", info.NativeImages, wantInfo.NativeImages)
			eqInt(t, "imageBudgetSkips", info.ImageBudgetSkips, wantInfo.ImageBudgetSkips)
			eqInt(t, "wireImages", info.WireImages, wantInfo.WireImages)
			eqInt(t, "staticChars", info.StaticChars, wantInfo.StaticChars)
			eqInt(t, "dynamicChars", info.DynamicChars, wantInfo.DynamicChars)
			eqInt(t, "dynamicBlockCount", info.DynamicBlockCount, wantInfo.DynamicBlockCount)
			eqInt(t, "droppedChars", info.DroppedChars, wantInfo.DroppedChars)
			eqStr(t, "firstUserSha8", info.FirstUserSha8, wantInfo.FirstUserSha8)
			eqStr(t, "systemSha8", info.SystemSha8, wantInfo.SystemSha8)
			eqInt(t, "toolDocsChars", info.ToolDocsChars, wantInfo.ToolDocsChars)
			eqInt(t, "outgoingTextChars", info.OutgoingTextChars, wantInfo.OutgoingTextChars)
			eqInt(t, "collapsedTurns", info.CollapsedTurns, wantInfo.CollapsedTurns)
			eqInt(t, "collapsedChars", info.CollapsedChars, wantInfo.CollapsedChars)
			eqInt(t, "collapsedImages", info.CollapsedImages, wantInfo.CollapsedImages)
			eqStr(t, "historyReason", info.HistoryReason, wantInfo.HistoryReason)
			eqInt(t, "historyTextChars", info.HistoryTextChars, wantInfo.HistoryTextChars)
			eqInt(t, "historyFreezeStep", info.HistoryFreezeStep, wantInfo.HistoryFreezeStep)
			if info.HistoryPackFill != wantInfo.HistoryPackFill {
				t.Errorf("historyPackFill: got %v want %v", info.HistoryPackFill, wantInfo.HistoryPackFill)
			}
			if info.HistoryBudgetTrimmed != wantInfo.HistoryBudgetTrimmed {
				t.Errorf("historyBudgetTrimmed: got %v want %v", info.HistoryBudgetTrimmed, wantInfo.HistoryBudgetTrimmed)
			}
			if (info.CachePrefixBytes > 0) != (wantInfo.CachePrefixBytes > 0) {
				t.Errorf("cachePrefixBytes presence: got %d want %d", info.CachePrefixBytes, wantInfo.CachePrefixBytes)
			}
			if (info.CachePrefixMarkedBytes > 0) != (wantInfo.CachePrefixMarkedBytes > 0) {
				t.Errorf("cachePrefixMarkedBytes presence: got %d want %d", info.CachePrefixMarkedBytes, wantInfo.CachePrefixMarkedBytes)
			}
			eqStr(t, "cachePrefixMarkerPos", info.CachePrefixMarkerPos, wantInfo.CachePrefixMarkerPos)
			eqStr(t, "imageSourceText", info.ImageSourceText, wantInfo.ImageSourceText)
			eqInt(t, "firstImageWidth", info.FirstImageWidth, wantInfo.FirstImageWidth)
			eqInt(t, "firstImageHeight", info.FirstImageHeight, wantInfo.FirstImageHeight)
			eqInt(t, "truncatedToolResults", info.TruncatedToolResults, wantInfo.TruncatedToolResults)
			eqInt(t, "omittedChars", info.OmittedChars, wantInfo.OmittedChars)
			eqInt(t, "keptSharpBlocks", info.KeptSharpBlocks, wantInfo.KeptSharpBlocks)
			eqInt(t, "pinChars", info.PinChars, wantInfo.PinChars)
			if (wantInfo.HistoryImageSha != "") != (info.HistoryImageSha != "") {
				t.Errorf("historyImageSha presence: got %q want-present=%v", info.HistoryImageSha, wantInfo.HistoryImageSha != "")
			}
			if (wantInfo.CachePrefixSha8 != "") != (info.CachePrefixSha8 != "") {
				t.Errorf("cachePrefixSha8 presence: got %q want-present=%v", info.CachePrefixSha8, wantInfo.CachePrefixSha8 != "")
			}
			for name, pair := range map[string][2]string{
				"cachePrefixToolsSha8":  {info.CachePrefixToolsSha8, wantInfo.CachePrefixToolsSha8},
				"cachePrefixSystemSha8": {info.CachePrefixSystemSha8, wantInfo.CachePrefixSystemSha8},
				"cachePrefixHeadSha8":   {info.CachePrefixHeadSha8, wantInfo.CachePrefixHeadSha8},
				"cachePrefixMarkedSha8": {info.CachePrefixMarkedSha8, wantInfo.CachePrefixMarkedSha8},
			} {
				if (pair[0] != "") != (pair[1] != "") {
					t.Errorf("%s presence: got %q want-present=%v", name, pair[0], pair[1] != "")
				}
			}
			if wantInfo.ImageBytes > 0 {
				ratio := float64(info.ImageBytes) / float64(wantInfo.ImageBytes)
				if math.Abs(ratio-1) > 0.35 {
					t.Errorf("imageBytes: got %d want ~%d (zlib impls differ, ratio %.2f)", info.ImageBytes, wantInfo.ImageBytes, ratio)
				}
			}
			if len(wantInfo.ImageDims) != len(info.ImageDims) {
				t.Errorf("imageDims len: got %d want %d", len(info.ImageDims), len(wantInfo.ImageDims))
			} else {
				for i := range wantInfo.ImageDims {
					if wantInfo.ImageDims[i] != info.ImageDims[i] {
						t.Errorf("imageDims[%d]: got %v want %v", i, info.ImageDims[i], wantInfo.ImageDims[i])
					}
				}
			}
			if wantInfo.GateEval != nil {
				if info.GateEval == nil {
					t.Error("gateEval: missing")
				} else {
					eqStr(t, "gateEval.site", info.GateEval.Site, wantInfo.GateEval.Site)
					if info.GateEval.ImageTokens != wantInfo.GateEval.ImageTokens {
						t.Errorf("gateEval.imageTokens: got %v want %v", info.GateEval.ImageTokens, wantInfo.GateEval.ImageTokens)
					}
					if info.GateEval.TextTokens != wantInfo.GateEval.TextTokens {
						t.Errorf("gateEval.textTokens: got %v want %v", info.GateEval.TextTokens, wantInfo.GateEval.TextTokens)
					}
					if info.GateEval.Profitable != wantInfo.GateEval.Profitable {
						t.Errorf("gateEval.profitable: got %v want %v", info.GateEval.Profitable, wantInfo.GateEval.Profitable)
					}
				}
			}
			for k, v := range wantInfo.BucketChars {
				if info.BucketChars[k] != v {
					t.Errorf("bucketChars[%s]: got %d want %d", k, info.BucketChars[k], v)
				}
			}
			for k, v := range wantInfo.PassthroughReasons {
				if info.PassthroughReasons[k] != v {
					t.Errorf("passthroughReasons[%s]: got %d want %d", k, info.PassthroughReasons[k], v)
				}
			}
			if len(wantInfo.Recoverable) != len(info.Recoverable) {
				t.Errorf("recoverable len: got %d want %d", len(info.Recoverable), len(wantInfo.Recoverable))
			} else {
				for i, w := range wantInfo.Recoverable {
					g := info.Recoverable[i]
					if g.ID != w.ID || g.Kind != w.Kind || g.ToolUseID != w.ToolUseID || g.Text != w.Text || g.ImageCount != w.ImageCount {
						t.Errorf("recoverable[%d]: got {%s %s %s len=%d n=%d} want {%s %s %s len=%d n=%d}",
							i, g.ID, g.Kind, g.ToolUseID, len(g.Text), g.ImageCount,
							w.ID, w.Kind, w.ToolUseID, len(w.Text), w.ImageCount)
					}
				}
			}

			var wantReq, gotReq any
			if err := jsonUnmarshal(wantOut, &wantReq); err != nil {
				t.Fatal(err)
			}
			if err := jsonUnmarshal(gotBody, &gotReq); err != nil {
				t.Fatal(err)
			}
			deepCompare(t, "body", wantReq, gotReq)
		})
	}
}
