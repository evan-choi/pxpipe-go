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
