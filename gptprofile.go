package pxpipe

import (
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
	"github.com/evan-choi/pxpipe-go/render"
)

// Port of src/core/gpt-model-profiles.ts + profile-base.ts +
// claude-model-profiles.ts (GPT view) + gemini-model-profiles.ts +
// vision-cost.ts regimes. Every provider difference is DATA on the profile;
// visionTokensFor is the single interpreter.

const (
	// GptMaxHeightPx is the max rendered image height for the GPT path.
	// OpenAI resize bounds (2048-bbox / 768 short side) permit tall strips.
	GptMaxHeightPx = 1932
	// DefaultGptStripCols is the downscale-safe strip width (768px).
	DefaultGptStripCols = 152
)

// GptVisionCost is the image-token cost model, tagged by Regime:
//
//	tile:    OpenAI legacy. 2048/768 downscale, then Base + PerTile per 512-px tile.
//	patch:   OpenAI 32-px patches × Multiplier. PatchCap 0 bills original dims.
//	patch28: Anthropic 28-px patches after the tier downscale.
//	mpix:    megapixels × TokensPerMegapixel, min 1 (Grok).
//	flat:    one fixed charge per image (Gemini), with optional exact override.
type GptVisionCost struct {
	Regime             string        `json:"regime"`
	Base               float64       `json:"base,omitempty"`
	PerTile            float64       `json:"perTile,omitempty"`
	Multiplier         float64       `json:"multiplier,omitempty"`
	PatchCap           int           `json:"patchCap,omitempty"`
	TokensPerMegapixel float64       `json:"tokensPerMegapixel,omitempty"`
	Tokens             int           `json:"tokens,omitempty"`
	Exact              *GptFlatExact `json:"exact,omitempty"`
}

// GptFlatExact is a measured exact-canvas override for the flat regime.
type GptFlatExact struct {
	WidthPx  int `json:"widthPx"`
	HeightPx int `json:"heightPx"`
	Tokens   int `json:"tokens"`
}

// GptHistoryProfile carries model-specific history coverage knobs.
type GptHistoryProfile struct {
	MaxImages         int    `json:"maxImages"`
	KeepTail          int    `json:"keepTail"`
	KeepRecentPairs   int    `json:"keepRecentPairs"`
	MinCollapseTokens int    `json:"minCollapseTokens"`
	ResponsesMode     string `json:"responsesMode"`  // "pairs" | "mixed"
	Framing           string `json:"framing"`        // "full" | "compact"
	FactSheetScope    string `json:"factSheetScope"` // "per-segment" | "combined"
}

// GptModelProfile is the complete per-model render + pricing profile.
type GptModelProfile struct {
	Vision        GptVisionCost `json:"vision"`
	CacheReadRate float64       `json:"cacheReadRate"`
	OutputRate    float64       `json:"outputRate"`
	StripCols     int           `json:"stripCols"`
	MaxHeightPx   int           `json:"maxHeightPx"`
	// MinCompressTokens nil preserves the legacy character floor.
	MinCompressTokens *int `json:"minCompressTokens,omitempty"`
	// VisionTier "" = standard; only Claude profiles set it.
	VisionTier                string             `json:"visionTier,omitempty"`
	FactSheetFormat           string             `json:"factSheetFormat"` // "full" | "compact"
	History                   GptHistoryProfile  `json:"history"`
	Style                     render.RenderStyle `json:"style"`
	MaxSerializedRequestBytes int                `json:"maxSerializedRequestBytes,omitempty"`
	ExactStaticBaseline       bool               `json:"exactStaticBaseline,omitempty"`
}

func intPtr(v int) *int { return &v }

// Shared defaults (profile-base.ts).
var basePricing = struct{ cacheReadRate, outputRate float64 }{0.5, 4}
var gpt5Pricing = struct{ cacheReadRate, outputRate float64 }{0.1, 8}

var gptBaseStyle = render.RenderStyle{
	Font:        render.DefaultRenderFont,
	AA:          true,
	MarkerScale: 1,
}

var gptBaseHistory = GptHistoryProfile{
	MaxImages:         32,
	KeepTail:          6,
	KeepRecentPairs:   6,
	MinCollapseTokens: 2000,
	ResponsesMode:     "pairs",
	Framing:           "full",
	FactSheetScope:    "per-segment",
}

// DefaultGptProfile is the conservative fallback for unrecognized models:
// tile 85/170 over-states cost, biasing the gate toward pass-through.
var DefaultGptProfile = &GptModelProfile{
	Vision:            GptVisionCost{Regime: "tile", Base: 85, PerTile: 170},
	CacheReadRate:     basePricing.cacheReadRate,
	OutputRate:        basePricing.outputRate,
	StripCols:         DefaultGptStripCols,
	MaxHeightPx:       GptMaxHeightPx,
	MinCompressTokens: intPtr(500),
	FactSheetFormat:   "full",
	History:           gptBaseHistory,
	Style:             gptBaseStyle,
}

var gpt56SolProfile = &GptModelProfile{
	Vision:              GptVisionCost{Regime: "patch", Multiplier: 1},
	CacheReadRate:       gpt5Pricing.cacheReadRate,
	OutputRate:          gpt5Pricing.outputRate,
	ExactStaticBaseline: true,
	StripCols:           84,
	MaxHeightPx:         1954,
	MinCompressTokens:   intPtr(500),
	FactSheetFormat:     "full",
	History: GptHistoryProfile{
		MaxImages:         64,
		KeepTail:          1,
		KeepRecentPairs:   1,
		MinCollapseTokens: 1000,
		ResponsesMode:     "mixed",
		Framing:           "compact",
		FactSheetScope:    "combined",
	},
	Style: render.RenderStyle{Font: "jetbrains-mono-14", AA: true, MarkerScale: 1},
}

func miniNanoProfile(multiplier float64, pricing struct{ cacheReadRate, outputRate float64 }) *GptModelProfile {
	return &GptModelProfile{
		Vision:            GptVisionCost{Regime: "patch", Multiplier: multiplier, PatchCap: 1536},
		CacheReadRate:     pricing.cacheReadRate,
		OutputRate:        pricing.outputRate,
		StripCols:         DefaultGptStripCols,
		MaxHeightPx:       GptMaxHeightPx,
		MinCompressTokens: intPtr(500),
		FactSheetFormat:   "full",
		History:           gptBaseHistory,
		Style:             gptBaseStyle,
	}
}

func flagshipGptProfile(v GptVisionCost) *GptModelProfile {
	return &GptModelProfile{
		Vision:            v,
		CacheReadRate:     gpt5Pricing.cacheReadRate,
		OutputRate:        gpt5Pricing.outputRate,
		StripCols:         DefaultGptStripCols,
		MaxHeightPx:       GptMaxHeightPx,
		MinCompressTokens: intPtr(500),
		FactSheetFormat:   "full",
		History:           gptBaseHistory,
		Style:             gptBaseStyle,
	}
}

var o13Profile = &GptModelProfile{
	Vision:            GptVisionCost{Regime: "tile", Base: 75, PerTile: 150},
	CacheReadRate:     basePricing.cacheReadRate,
	OutputRate:        basePricing.outputRate,
	StripCols:         DefaultGptStripCols,
	MaxHeightPx:       GptMaxHeightPx,
	MinCompressTokens: intPtr(500),
	FactSheetFormat:   "full",
	History:           gptBaseHistory,
	Style:             gptBaseStyle,
}

var grokProfile = &GptModelProfile{
	Vision:            GptVisionCost{Regime: "mpix", TokensPerMegapixel: 1000},
	CacheReadRate:     0.25,
	OutputRate:        3,
	StripCols:         84,
	MaxHeightPx:       512,
	MinCompressTokens: intPtr(500),
	FactSheetFormat:   "full",
	History: func() GptHistoryProfile {
		h := gptBaseHistory
		h.MaxImages = 24
		return h
	}(),
	Style: render.RenderStyle{Font: "jetbrains-mono-14", AA: true, MarkerScale: 1},
}

// Claude profiles as seen by the GPT path (claude-model-profiles.ts).
var claudeGptProfile = &GptModelProfile{
	Vision:          GptVisionCost{Regime: "patch28"},
	CacheReadRate:   0.1,
	OutputRate:      5,
	StripCols:       render.AnthropicSlabCols,
	MaxHeightPx:     render.MaxHeightPx,
	VisionTier:      "high-res",
	FactSheetFormat: "full",
	History: GptHistoryProfile{
		MaxImages:         96,
		KeepTail:          6,
		KeepRecentPairs:   6,
		MinCollapseTokens: 2000,
		ResponsesMode:     "mixed",
		Framing:           "compact",
		FactSheetScope:    "combined",
	},
	Style: gptBaseStyle,
}

var claudeLegacyGptProfile = func() *GptModelProfile {
	p := *claudeGptProfile
	p.VisionTier = "standard"
	return &p
}()

func resolveClaudeGptProfile(m string) *GptModelProfile {
	if isPre47Claude(m) {
		return claudeLegacyGptProfile
	}
	return claudeGptProfile
}

// Gemini 3.6 Flash (gemini-model-profiles.ts).
var gemini36FlashProfile = &GptModelProfile{
	Vision: GptVisionCost{
		Regime: "flat",
		Tokens: 1120,
		Exact:  &GptFlatExact{WidthPx: 1568, HeightPx: 728, Tokens: 1078},
	},
	CacheReadRate:     basePricing.cacheReadRate,
	OutputRate:        basePricing.outputRate,
	StripCols:         render.AnthropicSlabCols,
	MaxHeightPx:       render.MaxHeightPx,
	MinCompressTokens: intPtr(500),
	FactSheetFormat:   "full",
	History:           gptBaseHistory,
	Style:             gptBaseStyle,
}

func isGeminiModel(model string) bool {
	id := strings.ToLower(model)
	return id == "gemini-3.6-flash" || id == "google/gemini-3.6-flash"
}

func isGrokModel(m string) bool { return strings.HasPrefix(m, "grok-") }

var miniNanoRe = regexp.MustCompile(`^(?:gpt-5(?:\.\d+)?|gpt-4\.1)-(?:mini|nano)`)
var gpt5FlagshipRe = regexp.MustCompile(`^gpt-5\.\d`)
var o13Re = regexp.MustCompile(`^o[13]`)

func isMiniNanoPatch(m string) bool {
	return miniNanoRe.MatchString(m) || strings.HasPrefix(m, "o4-mini")
}

// resolveGptBuiltin mirrors BUILTIN_RULES order exactly.
func resolveGptBuiltin(m string) *GptModelProfile {
	if isClaudeModel(m) {
		return resolveClaudeGptProfile(m)
	}
	switch {
	case isMiniNanoPatch(m) && strings.Contains(m, "nano") && strings.HasPrefix(m, "gpt-5"):
		return miniNanoProfile(2.46, gpt5Pricing)
	case isMiniNanoPatch(m) && strings.Contains(m, "nano"):
		return miniNanoProfile(2.46, basePricing)
	case isMiniNanoPatch(m) && strings.HasPrefix(m, "gpt-5"):
		return miniNanoProfile(1.62, gpt5Pricing)
	case isMiniNanoPatch(m):
		return miniNanoProfile(1.62, basePricing)
	case m == "gpt-5.6-sol" || strings.HasPrefix(m, "gpt-5.6-sol-"):
		return gpt56SolProfile
	case gpt5FlagshipRe.MatchString(m):
		return flagshipGptProfile(GptVisionCost{Regime: "patch", Multiplier: 1, PatchCap: 10000})
	case strings.HasPrefix(m, "gpt-5"):
		return flagshipGptProfile(GptVisionCost{Regime: "tile", Base: 70, PerTile: 140})
	case o13Re.MatchString(m):
		return o13Profile
	case isGrokModel(m):
		return grokProfile
	}
	return DefaultGptProfile
}

// candidateIds returns both spellings of a vendor-qualified id.
func candidateIds(m string) []string {
	if slash := strings.LastIndexByte(m, '/'); slash >= 0 {
		return []string{m, m[slash+1:]}
	}
	return []string{m}
}

// --- env override (PXPIPE_GPT_PROFILES) ------------------------------------

var (
	gptEnvMu    sync.Mutex
	gptEnvCache atomic.Pointer[gptEnvSnapshot]
)

type gptEnvSnapshot struct {
	raw      string
	profiles map[string]*GptModelProfile
	// order preserves insertion order for the longest-match tie rule.
	order []string
}

type envVisionIn struct {
	Regime             string   `json:"regime"`
	Base               *float64 `json:"base"`
	PerTile            *float64 `json:"perTile"`
	Multiplier         *float64 `json:"multiplier"`
	PatchCap           *float64 `json:"patchCap"`
	TokensPerMegapixel *float64 `json:"tokensPerMegapixel"`
	Tokens             *float64 `json:"tokens"`
	Exact              *struct {
		WidthPx  *float64 `json:"widthPx"`
		HeightPx *float64 `json:"heightPx"`
		Tokens   *float64 `json:"tokens"`
	} `json:"exact"`
}

type envStyleIn struct {
	Font        *string  `json:"font"`
	CellWBonus  *float64 `json:"cellWBonus"`
	CellHBonus  *float64 `json:"cellHBonus"`
	AA          *bool    `json:"aa"`
	Grid        *bool    `json:"grid"`
	GridCols    *float64 `json:"gridCols"`
	ColorCycle  *bool    `json:"colorCycle"`
	MarkerScale *float64 `json:"markerScale"`
	MarkerRed   *bool    `json:"markerRed"`
	InkDilate   *float64 `json:"inkDilate"`
}

type envHistoryIn struct {
	MaxImages         *float64 `json:"maxImages"`
	KeepTail          *float64 `json:"keepTail"`
	KeepRecentPairs   *float64 `json:"keepRecentPairs"`
	MinCollapseTokens *float64 `json:"minCollapseTokens"`
	ResponsesMode     *string  `json:"responsesMode"`
	Framing           *string  `json:"framing"`
	FactSheetScope    *string  `json:"factSheetScope"`
}

type envProfileIn struct {
	Vision                    *envVisionIn  `json:"vision"`
	CacheReadRate             *float64      `json:"cacheReadRate"`
	OutputRate                *float64      `json:"outputRate"`
	StripCols                 *float64      `json:"stripCols"`
	MaxHeightPx               *float64      `json:"maxHeightPx"`
	VisionTier                *string       `json:"visionTier"`
	MinCompressTokens         *float64      `json:"minCompressTokens"`
	FactSheetFormat           *string       `json:"factSheetFormat"`
	History                   *envHistoryIn `json:"history"`
	Style                     *envStyleIn   `json:"style"`
	MaxSerializedRequestBytes *float64      `json:"maxSerializedRequestBytes"`
	ExactStaticBaseline       *bool         `json:"exactStaticBaseline"`
}

func finitePos(v *float64) bool  { return v != nil && !math.IsInf(*v, 0) && !math.IsNaN(*v) && *v > 0 }
func finiteNneg(v *float64) bool { return v != nil && !math.IsInf(*v, 0) && !math.IsNaN(*v) && *v >= 0 }

func envRate(v *float64, fallback float64) float64 {
	if finitePos(v) {
		return *v
	}
	return fallback
}

func envPosInt(v *float64, fallback int) int {
	if finitePos(v) {
		return int(math.Floor(*v))
	}
	return fallback
}

func envNonNegInt(v *float64, fallback int) int {
	if finiteNneg(v) {
		return int(math.Floor(*v))
	}
	return fallback
}

func envRenderFont(v *string, fallback string) string {
	if v != nil {
		switch *v {
		case "spleen-5x8", "jetbrains-mono-10", "jetbrains-mono-12", "jetbrains-mono-14":
			return *v
		}
	}
	return fallback
}

func envEnum(v *string, fallback string, allowed ...string) string {
	if v != nil {
		for _, a := range allowed {
			if *v == a {
				return *v
			}
		}
	}
	return fallback
}

func envValidVision(v *envVisionIn) *GptVisionCost {
	if v == nil {
		return nil
	}
	finite := func(p *float64) bool { return p != nil && !math.IsInf(*p, 0) && !math.IsNaN(*p) }
	switch v.Regime {
	case "tile":
		if finite(v.Base) && finite(v.PerTile) {
			return &GptVisionCost{Regime: "tile", Base: *v.Base, PerTile: *v.PerTile}
		}
	case "patch":
		if finite(v.Multiplier) && (v.PatchCap == nil || (finite(v.PatchCap) && *v.PatchCap > 0)) {
			out := &GptVisionCost{Regime: "patch", Multiplier: *v.Multiplier}
			if v.PatchCap != nil {
				out.PatchCap = int(*v.PatchCap)
			}
			return out
		}
	case "patch28":
		return &GptVisionCost{Regime: "patch28"}
	case "mpix":
		if finite(v.TokensPerMegapixel) && *v.TokensPerMegapixel > 0 {
			return &GptVisionCost{Regime: "mpix", TokensPerMegapixel: *v.TokensPerMegapixel}
		}
	case "flat":
		if !finite(v.Tokens) || *v.Tokens <= 0 {
			return nil
		}
		out := &GptVisionCost{Regime: "flat", Tokens: int(*v.Tokens)}
		if v.Exact == nil {
			return out
		}
		e := v.Exact
		if finite(e.WidthPx) && *e.WidthPx > 0 && finite(e.HeightPx) && *e.HeightPx > 0 && finite(e.Tokens) && *e.Tokens > 0 {
			out.Exact = &GptFlatExact{WidthPx: int(*e.WidthPx), HeightPx: int(*e.HeightPx), Tokens: int(*e.Tokens)}
			return out
		}
	}
	return nil
}

func parseGptEnvProfiles(raw string) (map[string]*GptModelProfile, []string) {
	out := map[string]*GptModelProfile{}
	var order []string
	if raw == "" {
		return out, order
	}
	// sonic preserves no key order via map; decode into an ordered raw form.
	var keys []string
	var byKey map[string]envProfileIn
	if err := sonic.UnmarshalString(raw, &byKey); err != nil {
		return out, order // malformed env never throws
	}
	// Recover insertion order by scanning the raw JSON keys.
	root, err := sonic.GetFromString(raw)
	if err == nil {
		_ = root.ForEach(func(path ast.Sequence, node *ast.Node) bool {
			if path.Key != nil {
				keys = append(keys, *path.Key)
			}
			return true
		})
	}
	if len(keys) == 0 {
		for k := range byKey {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		p, ok := byKey[k]
		if !ok {
			continue
		}
		key := strings.ToLower(k)
		var base *GptModelProfile
		if key == "gpt-5.6-sol" {
			base = gpt56SolProfile
		} else {
			base = resolveGptBuiltin(key)
		}
		style := render.RenderStyle{
			Font:        envRenderFont(nil, base.Style.Font),
			CellWBonus:  base.Style.CellWBonus,
			CellHBonus:  base.Style.CellHBonus,
			AA:          base.Style.AA,
			Grid:        base.Style.Grid,
			GridCols:    base.Style.GridCols,
			ColorCycle:  base.Style.ColorCycle,
			MarkerScale: base.Style.MarkerScale,
			MarkerRed:   base.Style.MarkerRed,
			InkDilate:   base.Style.InkDilate,
		}
		if s := p.Style; s != nil {
			style.Font = envRenderFont(s.Font, base.Style.Font)
			style.CellWBonus = envNonNegInt(s.CellWBonus, base.Style.CellWBonus)
			style.CellHBonus = envNonNegInt(s.CellHBonus, base.Style.CellHBonus)
			if s.AA != nil {
				style.AA = *s.AA
			}
			if s.Grid != nil {
				style.Grid = *s.Grid
			}
			style.GridCols = envNonNegInt(s.GridCols, base.Style.GridCols)
			if s.ColorCycle != nil {
				style.ColorCycle = *s.ColorCycle
			}
			style.MarkerScale = envPosInt(s.MarkerScale, base.Style.MarkerScale)
			if s.MarkerRed != nil {
				style.MarkerRed = *s.MarkerRed
			}
			style.InkDilate = envNonNegInt(s.InkDilate, base.Style.InkDilate)
		}
		history := base.History
		if h := p.History; h != nil {
			history = GptHistoryProfile{
				MaxImages:         envPosInt(h.MaxImages, base.History.MaxImages),
				KeepTail:          envNonNegInt(h.KeepTail, base.History.KeepTail),
				KeepRecentPairs:   envNonNegInt(h.KeepRecentPairs, base.History.KeepRecentPairs),
				MinCollapseTokens: envNonNegInt(h.MinCollapseTokens, base.History.MinCollapseTokens),
				ResponsesMode:     envEnum(h.ResponsesMode, base.History.ResponsesMode, "pairs", "mixed"),
				Framing:           envEnum(h.Framing, base.History.Framing, "full", "compact"),
				FactSheetScope:    envEnum(h.FactSheetScope, base.History.FactSheetScope, "per-segment", "combined"),
			}
		}
		vision := base.Vision
		if v := envValidVision(p.Vision); v != nil {
			vision = *v
		}
		minCompress := base.MinCompressTokens
		if p.MinCompressTokens != nil {
			fb := 0
			if base.MinCompressTokens != nil {
				fb = *base.MinCompressTokens
			}
			minCompress = intPtr(envNonNegInt(p.MinCompressTokens, fb))
		}
		visionTier := base.VisionTier
		if p.VisionTier != nil && (*p.VisionTier == "high-res" || *p.VisionTier == "standard") {
			visionTier = *p.VisionTier
		}
		maxSer := base.MaxSerializedRequestBytes
		if p.MaxSerializedRequestBytes != nil {
			maxSer = envPosInt(p.MaxSerializedRequestBytes, base.MaxSerializedRequestBytes)
		}
		exactBase := base.ExactStaticBaseline
		if p.ExactStaticBaseline != nil {
			exactBase = *p.ExactStaticBaseline
		}
		out[key] = &GptModelProfile{
			Vision:                    vision,
			CacheReadRate:             envRate(p.CacheReadRate, base.CacheReadRate),
			OutputRate:                envRate(p.OutputRate, base.OutputRate),
			StripCols:                 envPosInt(p.StripCols, base.StripCols),
			MaxHeightPx:               envPosInt(p.MaxHeightPx, base.MaxHeightPx),
			MinCompressTokens:         minCompress,
			VisionTier:                visionTier,
			FactSheetFormat:           envEnum(p.FactSheetFormat, base.FactSheetFormat, "full", "compact"),
			History:                   history,
			Style:                     style,
			MaxSerializedRequestBytes: maxSer,
			ExactStaticBaseline:       exactBase,
		}
		order = append(order, key)
	}
	return out, order
}

func gptEnvProfiles() (map[string]*GptModelProfile, []string) {
	raw := os.Getenv("PXPIPE_GPT_PROFILES")
	if cached := gptEnvCache.Load(); cached != nil && cached.raw == raw {
		return cached.profiles, cached.order
	}
	gptEnvMu.Lock()
	defer gptEnvMu.Unlock()
	if cached := gptEnvCache.Load(); cached != nil && cached.raw == raw {
		return cached.profiles, cached.order
	}
	profiles, order := parseGptEnvProfiles(raw)
	cached := &gptEnvSnapshot{raw: raw, profiles: profiles, order: order}
	gptEnvCache.Store(cached)
	return profiles, order
}

var bracketVariantRe = regexp.MustCompile(`\[[^\]]*\]`)

// ResolveGptProfile resolves the render/pricing profile for a model id,
// mirroring resolveGptProfile in gpt-model-profiles.ts.
func ResolveGptProfile(model string) *GptModelProfile {
	m := bracketVariantRe.ReplaceAllString(strings.ToLower(model), "")
	ids := candidateIds(m)
	for _, id := range ids {
		if isGeminiModel(id) {
			return gemini36FlashProfile
		}
	}
	env, order := gptEnvProfiles()
	if len(env) > 0 {
		var best *GptModelProfile
		bestLen := -1
		for _, k := range order {
			p := env[k]
			for _, id := range ids {
				if strings.HasPrefix(id, k) && len(k) > bestLen {
					best = p
					bestLen = len(k)
				}
			}
		}
		if best != nil {
			return best
		}
	}
	if len(ids) > 1 {
		if q := resolveGptBuiltin(ids[0]); q != DefaultGptProfile {
			return q
		}
	}
	return resolveGptBuiltin(ids[len(ids)-1])
}

// hasDeclaredGptProfile reports whether the operator declared this id in
// PXPIPE_GPT_PROFILES.
func hasDeclaredGptProfile(m string) bool {
	env, _ := gptEnvProfiles()
	if len(env) == 0 {
		return false
	}
	ids := candidateIds(m)
	for k := range env {
		for _, id := range ids {
			if strings.HasPrefix(id, k) {
				return true
			}
		}
	}
	return false
}

// IsMisresolvedModelId is true when an id NAMES a known provider family but
// does not match that family's profile test (e.g. gemini-3.6-pro).
func IsMisresolvedModelId(model string) bool {
	m := strings.ToLower(model)
	if hasDeclaredGptProfile(m) {
		return false
	}
	ids := candidateIds(m)
	type guard struct {
		mentions func(string) bool
		matches  func(string) bool
	}
	guards := []guard{
		{func(id string) bool { return strings.Contains(id, "gemini") }, isGeminiModel},
		{func(id string) bool { return strings.Contains(id, "grok") }, isGrokModel},
	}
	for _, g := range guards {
		mentioned, matched := false, false
		for _, id := range ids {
			if g.mentions(id) {
				mentioned = true
			}
			if g.matches(id) {
				matched = true
			}
		}
		if mentioned && !matched {
			return true
		}
	}
	return false
}

// --- vision cost (vision-cost.ts) -------------------------------------------

const (
	openAITileMaxLongEdge  = 2048
	openAITileMaxShortEdge = 768
	openAITilePx           = 512
	openAIPatchPx          = 32
)

// visionTokensFor prices one width×height image under the profile's regime.
func visionTokensFor(p *GptModelProfile, width, height int) int {
	c := p.Vision
	switch c.Regime {
	case "patch28":
		tier := tierStandard
		if p.VisionTier == "high-res" {
			tier = tierHighRes
		}
		return patchTokensForTier(tier, width, height)
	case "patch":
		rawPatches := ceilDiv(width, openAIPatchPx) * ceilDiv(height, openAIPatchPx)
		patches := rawPatches
		if c.PatchCap > 0 && c.PatchCap < patches {
			patches = c.PatchCap
		}
		return int(math.Ceil(float64(patches) * c.Multiplier))
	case "mpix":
		pixels := float64(maxInt(0, width) * maxInt(0, height))
		return maxInt(1, int(math.Ceil(pixels/1_000_000*c.TokensPerMegapixel)))
	case "flat":
		if c.Exact != nil && width == c.Exact.WidthPx && height == c.Exact.HeightPx {
			return c.Exact.Tokens
		}
		return c.Tokens
	case "tile":
		w, h := width, height
		long := maxInt(w, h)
		if long > openAITileMaxLongEdge {
			r := float64(openAITileMaxLongEdge) / float64(long)
			w = int(math.Floor(float64(w) * r))
			h = int(math.Floor(float64(h) * r))
		}
		short := minInt(w, h)
		if short > openAITileMaxShortEdge {
			r := float64(openAITileMaxShortEdge) / float64(short)
			w = int(math.Floor(float64(w) * r))
			h = int(math.Floor(float64(h) * r))
		}
		return int(c.Base) + int(c.PerTile)*(ceilDiv(w, openAITilePx)*ceilDiv(h, openAITilePx))
	}
	return 0
}
