package pxpipe

import (
	"reflect"
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

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
