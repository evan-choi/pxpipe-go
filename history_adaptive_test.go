package pxpipe

import (
	"strings"
	"testing"

	"github.com/evan-choi/pxpipe-go/render"
)

func historyMessages(n, chars int) []any {
	messages := make([]any, n)
	for i := range messages {
		messages[i] = map[string]any{
			"role":    "assistant",
			"content": "turn " + strings.Repeat("x", chars),
		}
	}
	return messages
}

func TestUserTurnBlocksBatchesOversizedPrompts(t *testing.T) {
	messages := make([]any, 10)
	for i := range messages {
		messages[i] = map[string]any{
			"role":    "user",
			"content": "PASTED " + strings.Repeat("y", 3500),
		}
	}
	images := 0
	blocks, err := userTurnBlocks(messages, 0, len(messages), func(*render.RenderedImage) {
		images++
	})
	if err != nil {
		t.Fatal(err)
	}
	if images == 0 {
		t.Fatal("batched oversized prompts emitted no images")
	}
	if images >= len(messages) {
		t.Fatalf("rendered %d images for %d prompts; want byte-scaled batching", images, len(messages))
	}
	imageBlocks := 0
	var cue string
	for _, block := range blocks {
		m, ok := asMap(block)
		if !ok {
			continue
		}
		switch blockType(block) {
		case "image":
			imageBlocks++
		case "text":
			cue, _ = getStr(m, "text")
		}
	}
	if imageBlocks != images {
		t.Fatalf("image blocks = %d, callback images = %d", imageBlocks, images)
	}
	for _, turn := range []string{`<user t="0">`, `<user t="4">`, `<user t="9">`} {
		if !strings.Contains(cue, turn) {
			t.Errorf("batch cue does not name %s", turn)
		}
	}
	if strings.Contains(cue, `<user t="0"> (3507 chars) Preview:`) {
		t.Error("oldest prompt unexpectedly received one of the eight bounded previews")
	}
}

func TestHistoryDefaultsUseWidthAwareBudgetGeometry(t *testing.T) {
	o := historyDefaults()
	if got, want := o.pageChars, render.MaxCharsPerImage(o.cols); got != want {
		t.Fatalf("pageChars = %d, want %d at cols=%d", got, want, o.cols)
	}
	if o.imageBudget != AnthropicHistoryImageBudget || o.packFill || o.minFreezeStep != 0 {
		t.Fatalf("history budget defaults = %+v", o)
	}
}

func TestCollapseHistoryTrimsRangeToImageBudget(t *testing.T) {
	o := historyDefaults()
	o.keepTail = 0
	o.minCollapsePrefix = 1
	o.collapseChunk = 0
	o.freezeChunk = 10
	o.pageChars = 9000
	o.imageBudget = 2

	messages := historyMessages(30, 3000)
	_, info, err := collapseHistory(messages, func(string, int) bool { return true }, o)
	if err != nil {
		t.Fatal(err)
	}
	if !info.budgetTrimmed {
		t.Fatal("budgetTrimmed = false, want true")
	}
	if info.collapsedTurns >= len(messages) {
		t.Fatalf("collapsedTurns = %d, want a trimmed prefix", info.collapsedTurns)
	}
	if info.collapsedImages > o.imageBudget {
		t.Fatalf("collapsedImages = %d, budget = %d", info.collapsedImages, o.imageBudget)
	}
}

func TestCollapseHistoryAdaptiveFreezeGrid(t *testing.T) {
	o := historyDefaults()
	o.keepTail = 0
	o.minCollapsePrefix = 1
	o.collapseChunk = 0
	o.freezeChunk = 2
	o.pageChars = 9000
	o.imageBudget = 4

	_, info, err := collapseHistory(historyMessages(30, 500), func(string, int) bool { return true }, o)
	if err != nil {
		t.Fatal(err)
	}
	if info.freezeStep <= o.freezeChunk || info.freezeStep%o.freezeChunk != 0 {
		t.Fatalf("freezeStep = %d, want a coarser multiple of %d", info.freezeStep, o.freezeChunk)
	}
	ratio := info.freezeStep / o.freezeChunk
	if ratio&(ratio-1) != 0 {
		t.Fatalf("freezeStep = %d, want a power-of-two multiple of %d", info.freezeStep, o.freezeChunk)
	}
	if info.collapsedImages > o.imageBudget {
		t.Fatalf("collapsedImages = %d, budget = %d", info.collapsedImages, o.imageBudget)
	}
}

func TestCollapseHistoryPackFillRaisesFreezeStep(t *testing.T) {
	o := historyDefaults()
	o.keepTail = 0
	o.minCollapsePrefix = 1
	o.collapseChunk = 0
	o.freezeChunk = 2
	o.pageChars = 9000
	o.imageBudget = 100
	o.packFill = true

	_, info, err := collapseHistory(historyMessages(30, 500), func(string, int) bool { return true }, o)
	if err != nil {
		t.Fatal(err)
	}
	if info.freezeStep <= o.freezeChunk {
		t.Fatalf("freezeStep = %d, want packFill to coarsen the grid", info.freezeStep)
	}
	if info.collapsedImages > 3 {
		t.Fatalf("collapsedImages = %d, want at most one page over the packed two-page render", info.collapsedImages)
	}
}

func TestCollapseHistoryHonorsStickyMinimumFreezeStep(t *testing.T) {
	o := historyDefaults()
	o.keepTail = 0
	o.minCollapsePrefix = 1
	o.collapseChunk = 0
	o.freezeChunk = 2
	o.imageBudget = 100
	o.minFreezeStep = 16

	_, info, err := collapseHistory(historyMessages(30, 200), func(string, int) bool { return true }, o)
	if err != nil {
		t.Fatal(err)
	}
	if info.freezeStep != 16 {
		t.Fatalf("freezeStep = %d, want sticky floor 16", info.freezeStep)
	}
}
