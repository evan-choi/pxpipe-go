package pxpipe

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
)

// profiles.json is dumped from the TS reference (tools/dump-gpt-profiles.ts).
type tsProfileDump struct {
	Profiles    map[string]map[string]any `json:"profiles"`
	Misresolved map[string]bool           `json:"misresolved"`
}

func TestGoldenGptProfiles(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "openai", "profiles.json"))
	if err != nil {
		t.Skipf("profiles.json missing: %v", err)
	}
	var want tsProfileDump
	if err := sonic.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for id, wp := range want.Profiles {
		got := ResolveGptProfile(id)

		wv := wp["vision"].(map[string]any)
		if got.Vision.Regime != wv["regime"].(string) {
			t.Errorf("%s: regime %s want %s", id, got.Vision.Regime, wv["regime"])
			continue
		}
		numEq := func(name string, gotN float64) {
			if w, ok := wv[name].(float64); ok {
				if gotN != w {
					t.Errorf("%s: vision.%s %v want %v", id, name, gotN, w)
				}
			} else if gotN != 0 {
				t.Errorf("%s: vision.%s %v want unset", id, name, gotN)
			}
		}
		numEq("base", got.Vision.Base)
		numEq("perTile", got.Vision.PerTile)
		numEq("multiplier", got.Vision.Multiplier)
		numEq("patchCap", float64(got.Vision.PatchCap))
		numEq("tokensPerMegapixel", got.Vision.TokensPerMegapixel)
		numEq("tokens", float64(got.Vision.Tokens))
		if exact, ok := wv["exact"].(map[string]any); ok {
			if got.Vision.Exact == nil || got.Vision.Exact.WidthPx != int(exact["widthPx"].(float64)) ||
				got.Vision.Exact.HeightPx != int(exact["heightPx"].(float64)) || got.Vision.Exact.Tokens != int(exact["tokens"].(float64)) {
				t.Errorf("%s: vision.exact %+v want %+v", id, got.Vision.Exact, exact)
			}
		} else if got.Vision.Exact != nil {
			t.Errorf("%s: vision.exact %+v want unset", id, got.Vision.Exact)
		}

		if w, ok := wp["cacheReadRate"].(float64); !ok || got.CacheReadRate != w {
			t.Errorf("%s: cacheReadRate %v want %v", id, got.CacheReadRate, wp["cacheReadRate"])
		}
		if w, ok := wp["outputRate"].(float64); !ok || got.OutputRate != w {
			t.Errorf("%s: outputRate %v want %v", id, got.OutputRate, wp["outputRate"])
		}
		if w := int(wp["stripCols"].(float64)); got.StripCols != w {
			t.Errorf("%s: stripCols %d want %d", id, got.StripCols, w)
		}
		if w := int(wp["maxHeightPx"].(float64)); got.MaxHeightPx != w {
			t.Errorf("%s: maxHeightPx %d want %d", id, got.MaxHeightPx, w)
		}
		if w, ok := wp["minCompressTokens"].(float64); ok {
			if got.MinCompressTokens == nil || *got.MinCompressTokens != int(w) {
				t.Errorf("%s: minCompressTokens %v want %v", id, got.MinCompressTokens, w)
			}
		} else if got.MinCompressTokens != nil {
			t.Errorf("%s: minCompressTokens %v want nil", id, *got.MinCompressTokens)
		}
		wantTier, _ := wp["visionTier"].(string)
		if got.VisionTier != wantTier {
			t.Errorf("%s: visionTier %q want %q", id, got.VisionTier, wantTier)
		}
		if w := wp["factSheetFormat"].(string); got.FactSheetFormat != w {
			t.Errorf("%s: factSheetFormat %q want %q", id, got.FactSheetFormat, w)
		}
		wantExact, _ := wp["exactStaticBaseline"].(bool)
		if got.ExactStaticBaseline != wantExact {
			t.Errorf("%s: exactStaticBaseline %v want %v", id, got.ExactStaticBaseline, wantExact)
		}
		if want, ok := wp["providerImageCap"].(float64); ok {
			if got.ProviderImageCap != int(want) {
				t.Errorf("%s: providerImageCap %d want %v", id, got.ProviderImageCap, want)
			}
		} else if got.ProviderImageCap != 0 {
			t.Errorf("%s: providerImageCap %d want unset", id, got.ProviderImageCap)
		}

		wh := wp["history"].(map[string]any)
		hInt := func(name string, gotN int) {
			if w := int(wh[name].(float64)); gotN != w {
				t.Errorf("%s: history.%s %d want %d", id, name, gotN, w)
			}
		}
		hInt("maxImages", got.History.MaxImages)
		hInt("keepTail", got.History.KeepTail)
		hInt("keepRecentPairs", got.History.KeepRecentPairs)
		hInt("minCollapseTokens", got.History.MinCollapseTokens)
		hOptionalInt := func(name string, gotN *int) {
			if want, ok := wh[name].(float64); ok {
				if gotN == nil || *gotN != int(want) {
					t.Errorf("%s: history.%s %v want %v", id, name, gotN, want)
				}
			} else if gotN != nil {
				t.Errorf("%s: history.%s %d want unset", id, name, *gotN)
			}
		}
		hOptionalInt("minCollapsePrefix", got.History.MinCollapsePrefix)
		hOptionalInt("collapseChunk", got.History.CollapseChunk)
		hOptionalInt("freezeChunk", got.History.FreezeChunk)
		if w := wh["responsesMode"].(string); got.History.ResponsesMode != w {
			t.Errorf("%s: responsesMode %q want %q", id, got.History.ResponsesMode, w)
		}
		if w := wh["framing"].(string); got.History.Framing != w {
			t.Errorf("%s: framing %q want %q", id, got.History.Framing, w)
		}
		if w := wh["factSheetScope"].(string); got.History.FactSheetScope != w {
			t.Errorf("%s: factSheetScope %q want %q", id, got.History.FactSheetScope, w)
		}

		ws := wp["style"].(map[string]any)
		if w := ws["font"].(string); got.Style.Font != w {
			t.Errorf("%s: style.font %q want %q", id, got.Style.Font, w)
		}
		if w := ws["aa"].(bool); got.Style.AA != w {
			t.Errorf("%s: style.aa %v want %v", id, got.Style.AA, w)
		}
		if w := int(ws["markerScale"].(float64)); got.Style.MarkerScale != w {
			t.Errorf("%s: style.markerScale %d want %d", id, got.Style.MarkerScale, w)
		}
	}
	for id, wantMis := range want.Misresolved {
		if got := IsMisresolvedModelId(id); got != wantMis {
			t.Errorf("IsMisresolvedModelId(%s) = %v want %v", id, got, wantMis)
		}
	}
}

func TestLatestBuiltInProfiles(t *testing.T) {
	gemini := ResolveGptProfile("google/gemini-3.7-flash")
	if gemini.CacheReadRate != 0.25 || gemini.FactSheetFormat != "compact" ||
		gemini.History.MaxImages != 32 || gemini.History.KeepTail != 4 {
		t.Fatalf("Gemini 3.7 profile = %+v", gemini)
	}

	for _, model := range []string{"gpt-5.6-sol", "grok-4.6", "workers-ai/@cf/qwen/qwen3.8-27b"} {
		profile := ResolveGptProfile(model)
		if profile.Style.Font != "jetbrains-mono-14" || profile.History.MinCollapseTokens != 0 ||
			profile.History.MinCollapsePrefix == nil || *profile.History.MinCollapsePrefix != 1 ||
			profile.History.ResponsesMode != "mixed" {
			t.Errorf("%s native 14px profile = %+v", model, profile)
		}
	}
	if qwen := ResolveGptProfile("qwen3.8-27b"); qwen.ProviderImageCap != 32 {
		t.Fatalf("Qwen provider image cap = %d", qwen.ProviderImageCap)
	}
	if !IsMisresolvedModelId("qwen3-30b") {
		t.Fatal("unmeasured Qwen model must fail closed")
	}
	if claude := ResolveGptProfile("claude-opus-5"); claude.StripCols != 172 || claude.Style.Font != "jetbrains-mono-14" {
		t.Fatalf("Claude legible profile = cols %d, font %q", claude.StripCols, claude.Style.Font)
	}
}

func TestGptEnvProfileOverride(t *testing.T) {
	t.Setenv("PXPIPE_GPT_PROFILES", `{"kimi-k3":{"vision":{"regime":"mpix","tokensPerMegapixel":1000},"stripCols":152,"maxHeightPx":1932},"gpt-5.6-sol":{"stripCols":120,"history":{"minCollapsePrefix":0,"collapseChunk":2,"freezeChunk":3},"historyStripCols":111,"historyStyle":{"font":"jetbrains-mono-12"}}}`)
	p := ResolveGptProfile("moonshotai/kimi-k3")
	if p.Vision.Regime != "mpix" || p.Vision.TokensPerMegapixel != 1000 {
		t.Errorf("kimi-k3 vision = %+v", p.Vision)
	}
	if p.StripCols != 152 || p.MaxHeightPx != 1932 {
		t.Errorf("kimi-k3 geometry = %d/%d", p.StripCols, p.MaxHeightPx)
	}
	sol := ResolveGptProfile("gpt-5.6-sol-20260101")
	if sol.StripCols != 120 {
		t.Errorf("sol override stripCols = %d want 120", sol.StripCols)
	}
	// Inherits Sol's font even though the env only set stripCols.
	if sol.Style.Font != "jetbrains-mono-14" {
		t.Errorf("sol override font = %q", sol.Style.Font)
	}
	if sol.HistoryStripCols == nil || *sol.HistoryStripCols != 111 || sol.HistoryStyle == nil || sol.HistoryStyle.Font != "jetbrains-mono-12" {
		t.Errorf("sol history geometry = cols %v style %+v", sol.HistoryStripCols, sol.HistoryStyle)
	}
	if sol.History.MinCollapsePrefix == nil || *sol.History.MinCollapsePrefix != 0 ||
		sol.History.CollapseChunk == nil || *sol.History.CollapseChunk != 2 ||
		sol.History.FreezeChunk == nil || *sol.History.FreezeChunk != 3 {
		t.Errorf("sol history chunks = %+v", sol.History)
	}
	if IsMisresolvedModelId("moonshotai/kimi-k3") {
		t.Error("declared env id must not be misresolved")
	}
	t.Setenv("PXPIPE_GPT_PROFILES", "not json")
	if got := ResolveGptProfile("gpt-5.4"); got.StripCols != 152 {
		t.Errorf("malformed env should fall back: %+v", got.StripCols)
	}
}

func TestStripBracketVariants(t *testing.T) {
	tests := map[string]string{
		"gpt-5.4":                 "gpt-5.4",
		"gpt[preview]-5[dated].4": "gpt-5.4",
		"gpt-[unterminated":       "gpt-[unterminated",
		"a[[nested]b]":            "ab]",
	}
	for input, want := range tests {
		if got := stripBracketVariants(input); got != want {
			t.Errorf("stripBracketVariants(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBuiltInModelMatchers(t *testing.T) {
	miniNano := map[string]bool{
		"gpt-5-mini": true, "gpt-5.12-nano-preview": true,
		"gpt-4.1-mini": true, "o4-mini-extra": true,
		"gpt-5.1.2-mini": false, "gpt-5.-mini": false, "gpt-5.4": false,
	}
	for model, want := range miniNano {
		if got := isMiniNanoPatch(model); got != want {
			t.Errorf("isMiniNanoPatch(%q) = %v, want %v", model, got, want)
		}
	}

	pre47 := map[string]bool{
		"claude-3-5-sonnet": true, "claude-opus-4-6": true,
		"claude-opus-4-7": false, "claude-fable-5": false,
		"claude-sonnet-new-4": false, "not-claude": false,
		"claude-bad-prefix/claude-3-5": true,
	}
	for model, want := range pre47 {
		if got := isPre47Claude(model); got != want {
			t.Errorf("isPre47Claude(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestGptEnvProfileRefresh(t *testing.T) {
	t.Setenv("PXPIPE_GPT_PROFILES", `{"gpt-5.4":{"stripCols":120}}`)
	if got := ResolveGptProfile("gpt-5.4").StripCols; got != 120 {
		t.Fatalf("initial stripCols = %d, want 120", got)
	}
	t.Setenv("PXPIPE_GPT_PROFILES", `{"gpt-5.4":{"stripCols":144}}`)
	if got := ResolveGptProfile("gpt-5.4").StripCols; got != 144 {
		t.Fatalf("refreshed stripCols = %d, want 144", got)
	}
}

func TestGptEnvProfileConcurrentCacheFill(t *testing.T) {
	t.Setenv("PXPIPE_GPT_PROFILES", `{"gpt-cache-race":{"stripCols":123}}`)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			<-start
			if got := ResolveGptProfile("gpt-cache-race").StripCols; got != 123 {
				t.Errorf("stripCols = %d, want 123", got)
			}
		})
	}
	close(start)
	wg.Wait()
}
