package pxpipe

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/evan-choi/pxpipe-go/render"
)

// Anthropic vision geometry helpers plus the Messages-path view of the shared
// GPT model profiles.

const (
	cacheCreateRate = 1.25
	cacheReadRate   = 0.1
)

type visionTier string

const (
	tierHighRes  visionTier = "high-res"
	tierStandard visionTier = "standard"
)

type modelProfile struct {
	stripCols   int
	maxHeightPx int
	style       render.RenderStyle
	pricing     *GptModelProfile
}

func isClaudeModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "claude") || strings.Contains(m, "anthropic")
}

var claudeVersionRe = regexp.MustCompile(`claude-(?:[a-z]+-)?(\d+)(?:-(\d+))?`)

func isPre47Claude(m string) bool {
	v := claudeVersionRe.FindStringSubmatch(m)
	if v == nil {
		return false
	}
	major, _ := strconv.Atoi(v[1])
	minor := 0
	if v[2] != "" {
		minor, _ = strconv.Atoi(v[2])
	}
	return major < 4 || (major == 4 && minor < 7)
}

var variantTagRe = regexp.MustCompile(`\[[^\]]*\]`)

func resolveProfile(model string) *modelProfile {
	if model == "" {
		return nil
	}
	p := ResolveGptProfile(model)
	return &modelProfile{
		stripCols:   p.StripCols,
		maxHeightPx: p.MaxHeightPx,
		style:       p.Style,
		pricing:     p,
	}
}

const anthropicPatchPx = 28

type anthropicTierLimits struct {
	maxLongEdge     int
	maxVisualTokens int
}

var anthropicTiers = map[visionTier]anthropicTierLimits{
	tierHighRes:  {maxLongEdge: 2576, maxVisualTokens: 4784},
	tierStandard: {maxLongEdge: 1568, maxVisualTokens: 1568},
}

func patchTokens(width, height int) int {
	w, h := width, height
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return ceilDiv(w, anthropicPatchPx) * ceilDiv(h, anthropicPatchPx)
}

func fitsTier(w, h, maxLongEdge, maxVisualTokens int) bool {
	return ceilDiv(w, anthropicPatchPx)*anthropicPatchPx <= maxLongEdge &&
		ceilDiv(h, anthropicPatchPx)*anthropicPatchPx <= maxLongEdge &&
		patchTokens(w, h) <= maxVisualTokens
}

func resizedSize(w, h, maxLongEdge, maxVisualTokens int) (int, int) {
	if fitsTier(w, h, maxLongEdge, maxVisualTokens) {
		return w, h
	}
	if h > w {
		rh, rw := resizedSize(h, w, maxLongEdge, maxVisualTokens)
		return rw, rh
	}
	aspect := float64(h) / float64(w)
	lo, hi, best := 1, w, 1
	for lo <= hi {
		mid := (lo + hi) >> 1
		mh := jsRoundInt(float64(mid) * aspect)
		if mh < 1 {
			mh = 1
		}
		if fitsTier(mid, mh, maxLongEdge, maxVisualTokens) {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	bh := jsRoundInt(float64(best) * aspect)
	if bh < 1 {
		bh = 1
	}
	return best, bh
}

func patchTokensForTier(tier visionTier, width, height int) int {
	limits, ok := anthropicTiers[tier]
	if !ok {
		limits = anthropicTiers[tierStandard]
	}
	w, h := width, height
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	rw, rh := resizedSize(w, h, limits.maxLongEdge, limits.maxVisualTokens)
	return patchTokens(rw, rh)
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

func jsRoundInt(f float64) int {
	if f >= 0 {
		return int(f + 0.5)
	}
	// JS Math.round(-0.5) = -0 (rounds half toward +inf).
	n := int(f)
	if f-float64(n) < -0.5 {
		return n - 1
	}
	return n
}
