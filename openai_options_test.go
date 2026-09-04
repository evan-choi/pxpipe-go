package pxpipe

import (
	"reflect"
	"strings"
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

func TestBelowMinGptTokens(t *testing.T) {
	for _, tc := range []struct {
		text    string
		minimum int
	}{
		{"hello world", 3},
		{strings.Repeat("field ", 500), 500},
		{"한글 prompt\twith\nspaces", 4},
	} {
		want := gptTextTokens(tc.text) < tc.minimum
		gotCount, got := belowMinGptTokens(tc.text, tc.minimum)
		if got != want {
			t.Errorf("belowMinGptTokens(%q, %d) = %v, want %v", tc.text, tc.minimum, got, want)
		}
		if got && gotCount != gptTextTokens(tc.text) {
			t.Errorf("belowMinGptTokens(%q, %d) count = %d, want %d", tc.text, tc.minimum, gotCount, gptTextTokens(tc.text))
		}
	}
}

func TestOpenAIImageSourceTextUsesKnownUTF16Length(t *testing.T) {
	short := "a😀"
	if got := openAIImageSourceText(short, 3); got != short {
		t.Fatalf("short image source = %q, want %q", got, short)
	}
	long := strings.Repeat("😀", openAIImageSourcePreviewChars/2+1)
	if got := openAIImageSourceText(long, openAIImageSourcePreviewChars+2); got != long[:len(long)-4] {
		t.Fatalf("long image source bytes = %d, want %d", len(got), len(long)-4)
	}
}

func TestGptMaybeReflowMatchesReference(t *testing.T) {
	for _, text := range []string{
		"a" + render.NLSentinel + "b\nc",
		"a \n\n\n\nb" + render.NLSentinel,
		"a\t" + render.NLSentinel + "\nb",
		"한글" + render.NLSentinel + "\n😀",
		string([]byte{'a', 0xff, '\n', 'b'}),
	} {
		want, ok := render.Reflow(render.NeutralizeSentinel(text))
		if !ok {
			t.Fatalf("reference reflow rejected %q", text)
		}
		if got := gptMaybeReflow(text, true); got != want {
			t.Fatalf("gptMaybeReflow(%q) = %q, want %q", text, got, want)
		}
	}
	if text := "a\nb"; gptMaybeReflow(text, false) != text {
		t.Fatal("disabled reflow changed text")
	}
}

func TestGptHistoryOptionsPrecedence(t *testing.T) {
	intp := func(v int) *int { return &v }
	boolp := func(v bool) *bool { return &v }
	stringp := func(v string) *string { return &v }
	style := render.RenderStyle{Font: render.DefaultRenderFont, MarkerScale: 3}
	overrides := &GptHistoryOptions{
		KeepTail:          intp(2),
		MaxImages:         intp(3),
		KeepRecentPairs:   intp(4),
		ResponsesMode:     stringp("pairs"),
		MinCollapsePrefix: intp(0),
		MinCollapseTokens: intp(6),
		Cols:              intp(7),
		CollapseChunk:     intp(0),
		FreezeChunk:       intp(0),
		SectionTokens:     intp(10),
		MaxHeightPx:       intp(11),
		Style:             &style,
		Reflow:            boolp(true),
	}
	profile := ResolveGptProfile("gpt-5.6-sol")
	got := gptHistoryOptsFor("gpt-5.6-sol", resolveOpenAIOpts(&TransformOptions{
		Reflow:     boolp(false),
		GptHistory: overrides,
	}), profile)

	if got.KeepTail != 2 || got.MaxImages != 3 || got.KeepRecentPairs != 4 ||
		got.MinCollapsePrefix != 0 || got.MinCollapseTokens != 6 || got.Cols != 7 ||
		got.CollapseChunk != 0 || got.FreezeChunk != 0 || got.SectionTokens != 10 ||
		got.MaxHeightPx != 11 || !reflect.DeepEqual(got.Style, style) {
		t.Fatalf("history overrides not applied: %+v", got)
	}
	if got.Reflow {
		t.Error("gptHistory.reflow must not override top-level reflow")
	}
	if got.ResponsesMode != profile.History.ResponsesMode {
		t.Errorf("responses mode = %q, want profile %q", got.ResponsesMode, profile.History.ResponsesMode)
	}
}

func TestGptHistoryOptionsInheritProfileAndEnvironment(t *testing.T) {
	t.Setenv("PXPIPE_GPT_HISTORY_MAX_IMAGES", "70")
	profile := ResolveGptProfile("gpt-5.6-sol")
	got := gptHistoryOptsFor("gpt-5.6-sol", resolveOpenAIOpts(&TransformOptions{
		GptHistory: &GptHistoryOptions{},
	}), profile)

	if got.KeepTail != profile.History.KeepTail ||
		got.KeepRecentPairs != profile.History.KeepRecentPairs ||
		got.MinCollapseTokens != profile.History.MinCollapseTokens ||
		got.Cols != profile.StripCols || got.MaxHeightPx != profile.MaxHeightPx ||
		!reflect.DeepEqual(got.Style, profile.Style) || got.ResponsesMode != profile.History.ResponsesMode {
		t.Fatalf("profile defaults not inherited: %+v", got)
	}
	if got.MaxImages != 70 {
		t.Errorf("max images = %d, want environment override 70", got.MaxImages)
	}
}

func TestQwenHistoryUsesRemainingProviderImageBudget(t *testing.T) {
	profile := ResolveGptProfile("qwen3.8-27b")
	got := gptHistoryOptsFor("qwen3.8-27b", resolveOpenAIOpts(nil), profile, 24)
	if got.MaxImages != 8 {
		t.Fatalf("Qwen history max images = %d, want 8", got.MaxImages)
	}
	if got.MinCollapsePrefix != 1 || got.CollapseChunk != 1 || got.FreezeChunk != 1 {
		t.Fatalf("Qwen history chunks = prefix %d, collapse %d, freeze %d", got.MinCollapsePrefix, got.CollapseChunk, got.FreezeChunk)
	}
}

func TestOpenAIImageDetailMatchesModelFamily(t *testing.T) {
	if got := openAIImageDetail("gpt-5.6-sol"); got != "original" {
		t.Fatalf("GPT-5 detail = %q", got)
	}
	if got := openAIImageDetail("grok-4.6"); got != "high" {
		t.Fatalf("non-GPT-5 detail = %q", got)
	}
}

func TestQwenStaticSlabRespectsProviderImageCap(t *testing.T) {
	images := make([]any, 32)
	for i := range images {
		images[i] = map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,x"}}
	}
	body := jsStringify(map[string]any{
		"model": "qwen3.8-27b",
		"messages": []any{
			map[string]any{"role": "system", "content": strings.Repeat("instruction alpha beta gamma delta path=/tmp/file.json\n", 200)},
			map[string]any{"role": "user", "content": images},
		},
	})
	minChars, collapse := 1, false
	out, info := TransformOpenAIChatCompletions(body, &TransformOptions{MinCompressChars: &minChars, CollapseHistory: &collapse})
	if info.Reason != "provider_image_cap" || !reflect.DeepEqual(out, body) {
		t.Fatalf("Qwen cap result = reason %q, body changed %v", info.Reason, !reflect.DeepEqual(out, body))
	}
}

func TestChatHistoryPreservesCallerImages(t *testing.T) {
	bulk := strings.Repeat("history alpha beta gamma path=/tmp/file.json ", 400)
	callerImage := "data:image/png;base64,caller-image"
	body := jsStringify(map[string]any{
		"model": "qwen3.8-27b",
		"messages": []any{
			map[string]any{"role": "user", "content": "opening"},
			map[string]any{"role": "assistant", "content": bulk},
			map[string]any{"role": "user", "content": bulk},
			map[string]any{"role": "assistant", "content": bulk},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "inspect this image"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": callerImage}},
			}},
			map[string]any{"role": "assistant", "content": "acknowledged"},
			map[string]any{"role": "user", "content": "continue"},
		},
	})
	charsPerToken := 1.0
	out, info := TransformOpenAIChatCompletions(body, &TransformOptions{CharsPerToken: &charsPerToken})
	if !info.Compressed || info.HistoryReason != "collapsed" {
		t.Fatalf("history fixture did not collapse: %+v", info)
	}
	if !strings.Contains(string(out), callerImage) {
		t.Fatal("history collapse removed a caller image")
	}
}

func TestGptHistoryPlanReusesExactSectionSources(t *testing.T) {
	pinned := "pin"
	turns := []historyTurn{
		{Text: "zero"},
		{Text: "one"},
		{Text: "pin", UserText: &pinned},
		{Text: "three"},
		{Text: "four"},
	}
	o := defaultGptHistoryOptions()
	o.KeepTail = 0
	o.MinCollapsePrefix = 1
	o.MinCollapseTokens = 0
	o.CollapseChunk = 0
	o.FreezeChunk = 1
	o.SectionTokens = 2
	o.MaxImages = 10
	o.tokenCounts = gptTokenCounter{"zero": 1, "one": 1, "pin": 1, "three": 1, "four": 1}

	plan, err := planGptCollapse(turns, 0, func(string, int, int) bool { return true }, o)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Text != "zero\n\none\n\nthree\n\nfour" {
		t.Fatalf("collapsed text = %q", plan.Text)
	}
	if len(plan.ImageSources) == 0 || len(plan.ImageSourcesAfter) == 0 {
		t.Fatalf("expected images on both sides of pin: %+v", plan)
	}
	for _, source := range plan.ImageSources {
		if source != "zero\n\none" {
			t.Fatalf("before-pin source = %q", source)
		}
	}
	for _, source := range plan.ImageSourcesAfter {
		if source != "three\n\nfour" {
			t.Fatalf("after-pin source = %q", source)
		}
	}
	if plan.PinText == nil || *plan.PinText != pinned {
		t.Fatalf("pin text = %v", plan.PinText)
	}
}

func TestHistoryImageShaMatchesCachedBase64(t *testing.T) {
	images, err := render.RenderTextToPngs("history image sha cache", 64, render.DenseRenderStyle, 96, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := historyImageShaOf(images)
	for range 2 {
		images[0].AppendPNGBase64(nil)
	}
	var encoded strings.Builder
	if err := images[0].WritePNGBase64(&encoded); err != nil || encoded.Len() == 0 {
		t.Fatalf("write cached base64: len=%d err=%v", encoded.Len(), err)
	}
	if got := historyImageShaOf(images); got != want {
		t.Fatalf("cached history image sha = %q, want %q", got, want)
	}
}
