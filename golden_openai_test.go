package pxpipe

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

type goldenOpenAIOpts struct {
	Endpoint      string   `json:"endpoint"`
	Compress      *bool    `json:"compress"`
	CharsPerToken *float64 `json:"charsPerToken"`
}

func dataPNG(v any) ([]byte, bool) {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "data:image/png;base64,") {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "data:image/png;base64,"))
	if err != nil || !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, false
	}
	return raw, true
}

// openAIImageURL extracts the data-URL from either OpenAI image part shape.
func openAIImageURL(v any) (any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	switch m["type"] {
	case "image_url":
		inner, ok := m["image_url"].(map[string]any)
		if !ok {
			return nil, false
		}
		if _, isPNG := dataPNG(inner["url"]); isPNG {
			return inner["url"], true
		}
	case "input_image":
		if _, isPNG := dataPNG(m["image_url"]); isPNG {
			return m["image_url"], true
		}
	}
	return nil, false
}

func deepCompareOpenAI(t *testing.T, path string, want, got any) {
	t.Helper()
	if wurl, ok := openAIImageURL(want); ok {
		gurl, gok := openAIImageURL(got)
		if !gok {
			t.Errorf("%s: want image part, got %T", path, got)
			return
		}
		wdata, _ := dataPNG(wurl)
		gdata, _ := dataPNG(gurl)
		wimg := decodePNGPixels(t, wdata)
		gimg := decodePNGPixels(t, gdata)
		if wimg.Bounds() != gimg.Bounds() {
			t.Errorf("%s: image bounds %v != %v", path, gimg.Bounds(), wimg.Bounds())
			return
		}
		if !bytes.Equal(wimg.Pix, gimg.Pix) {
			t.Errorf("%s: image pixels differ", path)
		}
		wm := want.(map[string]any)
		gm := got.(map[string]any)
		deepCompareOpenAI(t, path+".detail", wm["detail"], gm["detail"])
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
			deepCompareOpenAI(t, path+"."+k, w, gv[k])
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
			deepCompareOpenAI(t, fmt.Sprintf("%s[%d]", path, i), wv[i], gv[i])
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

type goldenOpenAIInfo struct {
	Compressed           bool           `json:"compressed"`
	Reason               string         `json:"reason"`
	OrigChars            int            `json:"origChars"`
	CompressedChars      int            `json:"compressedChars"`
	ImageCount           int            `json:"imageCount"`
	ImageBytes           int            `json:"imageBytes"`
	ImagePixels          int            `json:"imagePixels"`
	StaticChars          int            `json:"staticChars"`
	DroppedChars         int            `json:"droppedChars"`
	FirstUserSha8        string         `json:"firstUserSha8"`
	SystemSha8           string         `json:"systemSha8"`
	OutgoingTextChars    int            `json:"outgoingTextChars"`
	ImageTokens          int            `json:"imageTokens"`
	BaselineImagedTokens int            `json:"baselineImagedTokens"`
	NativeInjectedTokens int            `json:"nativeInjectedTokens"`
	BucketChars          map[string]int `json:"bucketChars"`
	GateEval             *struct {
		Site        string  `json:"site"`
		ImageTokens float64 `json:"imageTokens"`
		TextTokens  float64 `json:"textTokens"`
		Profitable  bool    `json:"profitable"`
	} `json:"gateEval"`
	CollapsedTurns       int                   `json:"collapsedTurns"`
	CollapsedChars       int                   `json:"collapsedChars"`
	CollapsedImages      int                   `json:"collapsedImages"`
	HistoryReason        string                `json:"historyReason"`
	HistoryTextChars     int                   `json:"historyTextChars"`
	HistoryImageSha      string                `json:"historyImageSha"`
	ImageSourceText      string                `json:"imageSourceText"`
	ImageSourceTexts     []string              `json:"imageSourceTexts"`
	ImageDims            []imageDim            `json:"imageDims"`
	FirstImageWidth      int                   `json:"firstImageWidth"`
	FirstImageHeight     int                   `json:"firstImageHeight"`
	ResponsesComposition *ResponsesComposition `json:"responsesComposition"`
}

func TestGoldenOpenAI(t *testing.T) {
	root := filepath.Join("testdata", "openai")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ran++
		t.Run(e.Name(), func(t *testing.T) {
			dir := filepath.Join(root, e.Name())
			input, err := os.ReadFile(filepath.Join(dir, "input.json"))
			if err != nil {
				t.Fatal(err)
			}
			optsRaw, err := os.ReadFile(filepath.Join(dir, "opts.json"))
			if err != nil {
				t.Fatal(err)
			}
			var g goldenOpenAIOpts
			if err := sonic.Unmarshal(optsRaw, &g); err != nil {
				t.Fatal(err)
			}
			opts := &TransformOptions{Compress: g.Compress, CharsPerToken: g.CharsPerToken}
			wantOut, err := os.ReadFile(filepath.Join(dir, "output.json"))
			if err != nil {
				t.Fatal(err)
			}
			wantInfoRaw, err := os.ReadFile(filepath.Join(dir, "info.json"))
			if err != nil {
				t.Fatal(err)
			}
			var wantInfo goldenOpenAIInfo
			if err := sonic.Unmarshal(wantInfoRaw, &wantInfo); err != nil {
				t.Fatal(err)
			}

			var gotBody []byte
			var info *TransformInfo
			if g.Endpoint == "chat" {
				gotBody, info = TransformOpenAIChatCompletions(input, opts)
			} else {
				gotBody, info = TransformOpenAIResponses(input, opts)
			}

			if info.Compressed != wantInfo.Compressed {
				t.Fatalf("compressed: got %v want %v (reason %q)", info.Compressed, wantInfo.Compressed, info.Reason)
			}
			eqStr(t, "reason", info.Reason, wantInfo.Reason)
			eqInt(t, "origChars", info.OrigChars, wantInfo.OrigChars)
			eqInt(t, "compressedChars", info.CompressedChars, wantInfo.CompressedChars)
			eqInt(t, "imageCount", info.ImageCount, wantInfo.ImageCount)
			eqInt(t, "imagePixels", info.ImagePixels, wantInfo.ImagePixels)
			eqInt(t, "staticChars", info.StaticChars, wantInfo.StaticChars)
			eqInt(t, "droppedChars", info.DroppedChars, wantInfo.DroppedChars)
			eqStr(t, "firstUserSha8", info.FirstUserSha8, wantInfo.FirstUserSha8)
			eqStr(t, "systemSha8", info.SystemSha8, wantInfo.SystemSha8)
			eqInt(t, "outgoingTextChars", info.OutgoingTextChars, wantInfo.OutgoingTextChars)
			eqInt(t, "imageTokens", info.ImageTokens, wantInfo.ImageTokens)
			eqInt(t, "baselineImagedTokens", info.BaselineImagedTokens, wantInfo.BaselineImagedTokens)
			eqInt(t, "nativeInjectedTokens", info.NativeInjectedTokens, wantInfo.NativeInjectedTokens)
			eqInt(t, "collapsedTurns", info.CollapsedTurns, wantInfo.CollapsedTurns)
			eqInt(t, "collapsedChars", info.CollapsedChars, wantInfo.CollapsedChars)
			eqInt(t, "collapsedImages", info.CollapsedImages, wantInfo.CollapsedImages)
			eqStr(t, "historyReason", info.HistoryReason, wantInfo.HistoryReason)
			eqInt(t, "historyTextChars", info.HistoryTextChars, wantInfo.HistoryTextChars)
			eqStr(t, "imageSourceText", info.ImageSourceText, wantInfo.ImageSourceText)
			eqInt(t, "firstImageWidth", info.FirstImageWidth, wantInfo.FirstImageWidth)
			eqInt(t, "firstImageHeight", info.FirstImageHeight, wantInfo.FirstImageHeight)
			if (wantInfo.HistoryImageSha != "") != (info.HistoryImageSha != "") {
				t.Errorf("historyImageSha presence: got %q want-present=%v", info.HistoryImageSha, wantInfo.HistoryImageSha != "")
			}
			if wantInfo.ImageBytes > 0 {
				ratio := float64(info.ImageBytes) / float64(wantInfo.ImageBytes)
				if math.Abs(ratio-1) > 0.35 {
					t.Errorf("imageBytes: got %d want ~%d (ratio %.2f)", info.ImageBytes, wantInfo.ImageBytes, ratio)
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
			if len(wantInfo.ImageSourceTexts) != len(info.ImageSourceTexts) {
				t.Errorf("imageSourceTexts len: got %d want %d", len(info.ImageSourceTexts), len(wantInfo.ImageSourceTexts))
			} else {
				for i := range wantInfo.ImageSourceTexts {
					if wantInfo.ImageSourceTexts[i] != info.ImageSourceTexts[i] {
						t.Errorf("imageSourceTexts[%d] differs", i)
					}
				}
			}
			if wantInfo.GateEval != nil {
				if info.GateEval == nil {
					t.Errorf("gateEval missing")
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
			} else if info.GateEval != nil {
				t.Errorf("gateEval: got %+v want nil", info.GateEval)
			}
			for k, v := range wantInfo.BucketChars {
				if info.BucketChars[k] != v {
					t.Errorf("bucketChars[%s]: got %d want %d", k, info.BucketChars[k], v)
				}
			}
			if wantInfo.ResponsesComposition != nil {
				if info.ResponsesComposition == nil {
					t.Errorf("responsesComposition missing")
				} else {
					wj, _ := sonic.Marshal(wantInfo.ResponsesComposition)
					gj, _ := sonic.Marshal(info.ResponsesComposition)
					if string(wj) != string(gj) {
						t.Errorf("responsesComposition:\n got %s\nwant %s", gj, wj)
					}
				}
			}

			var wantTree, gotTree any
			if err := sonic.Unmarshal(wantOut, &wantTree); err != nil {
				t.Fatal(err)
			}
			if err := sonic.Unmarshal(gotBody, &gotTree); err != nil {
				t.Fatal(err)
			}
			deepCompareOpenAI(t, "$", wantTree, gotTree)
		})
	}
	if ran == 0 {
		t.Fatal("no golden cases")
	}
}
