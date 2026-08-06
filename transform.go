package pxpipe

import (
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/evan-choi/pxpipe-go/render"
)

// TransformRequest rewrites an Anthropic Messages API body: the static system
// slab + tool docs are rendered into PNG image blocks, large tool_results are
// imaged/paged, and old closed history collapses into synthetic history
// images. On any error the original bytes come back unchanged.

// KeepSharpBlock describes a live-region block offered to the KeepSharp
// predicate.
type KeepSharpBlock struct {
	Kind      string
	Text      string
	ToolUseID string
}

// RecoverableBlock carries the original text of an imaged block when
// EmitRecoverable is set.
type RecoverableBlock struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ToolUseID  string `json:"toolUseId,omitempty"`
	Text       string `json:"text"`
	ImageCount int    `json:"imageCount"`
}

// TransformOptions mirrors the TS TransformOptions surface that applies to the
// Anthropic path. Nil-able fields distinguish "unset → default".
type TransformOptions struct {
	Model                      string
	Compress                   *bool
	CompressTools              *bool
	CompressToolResults        *bool
	MinCompressChars           *int
	MinToolResultChars         *int
	Cols                       *int
	MaxImagesPerToolResult     *int
	CharsPerToken              *float64
	HistoryAmortizationHorizon *int
	PriorWarmTokens            *float64
	PriorWarmImageTokens       *float64
	Reflow                     *bool
	KeepSharp                  func(KeepSharpBlock) bool
	EmitRecoverable            bool
	// CollapseHistory gates GPT-path history imaging (default true).
	CollapseHistory *bool
	// GptHistory overrides GPT-path history-collapse tuning.
	GptHistory *GptHistoryOptions
}

type resolvedOptions struct {
	Model                      string
	Compress                   bool
	CompressTools              bool
	CompressToolResults        bool
	MinCompressChars           int
	MinToolResultChars         int
	Cols                       int
	MaxImagesPerToolResult     int
	CharsPerToken              float64
	charsPerTokenSet           bool
	HistoryAmortizationHorizon int
	PriorWarmTokens            float64
	PriorWarmImageTokens       float64
	Reflow                     bool
	KeepSharp                  func(KeepSharpBlock) bool
	EmitRecoverable            bool
}

func resolveOptions(opts *TransformOptions) *resolvedOptions {
	o := &resolvedOptions{
		Compress:                   true,
		CompressTools:              true,
		CompressToolResults:        true,
		MinCompressChars:           2000,
		MinToolResultChars:         6000,
		Cols:                       render.AnthropicSlabCols,
		MaxImagesPerToolResult:     10,
		CharsPerToken:              4,
		HistoryAmortizationHorizon: 1,
		Reflow:                     true,
	}
	if opts == nil {
		return o
	}
	o.Model = opts.Model
	if profile := resolveProfile(opts.Model); profile != nil {
		o.Cols = profile.stripCols
	}
	if opts.Compress != nil {
		o.Compress = *opts.Compress
	}
	if opts.CompressTools != nil {
		o.CompressTools = *opts.CompressTools
	}
	if opts.CompressToolResults != nil {
		o.CompressToolResults = *opts.CompressToolResults
	}
	if opts.MinCompressChars != nil {
		o.MinCompressChars = *opts.MinCompressChars
	}
	if opts.MinToolResultChars != nil {
		o.MinToolResultChars = *opts.MinToolResultChars
	}
	if opts.Cols != nil {
		o.Cols = *opts.Cols
	}
	if opts.MaxImagesPerToolResult != nil {
		o.MaxImagesPerToolResult = *opts.MaxImagesPerToolResult
	}
	if opts.CharsPerToken != nil {
		o.CharsPerToken = *opts.CharsPerToken
		o.charsPerTokenSet = true
	}
	if opts.HistoryAmortizationHorizon != nil {
		o.HistoryAmortizationHorizon = *opts.HistoryAmortizationHorizon
	}
	if opts.PriorWarmTokens != nil {
		o.PriorWarmTokens = *opts.PriorWarmTokens
	}
	if opts.PriorWarmImageTokens != nil {
		o.PriorWarmImageTokens = *opts.PriorWarmImageTokens
	}
	if opts.Reflow != nil {
		o.Reflow = *opts.Reflow
	}
	o.KeepSharp = opts.KeepSharp
	o.EmitRecoverable = opts.EmitRecoverable
	return o
}

// OAuth identities must stay the leading system block; see the TS comments on
// the 429 rate_limit classification failure (#149). Longest first so the
// within-SDK line never matches as its CLI prefix.
const (
	ClaudeCodeOAuthIdentity          = "You are Claude Code, Anthropic's official CLI for Claude."
	ClaudeCodeWithinSDKOAuthIdentity = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	ClaudeAgentSDKOAuthIdentity      = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
)

var oauthIdentities = func() []string {
	ids := []string{ClaudeCodeOAuthIdentity, ClaudeCodeWithinSDKOAuthIdentity, ClaudeAgentSDKOAuthIdentity}
	sort.SliceStable(ids, func(i, j int) bool { return u16len(ids[i]) > u16len(ids[j]) })
	return ids
}()

var readFirstTools = map[string]struct{}{"Edit": {}, "Write": {}, "NotebookEdit": {}}

// EnvFields carries telemetry parsed from Claude Code's <env>/git blocks.
type EnvFields struct {
	Cwd       string `json:"cwd,omitempty"`
	IsGitRepo *bool  `json:"isGitRepo,omitempty"`
	GitBranch string `json:"gitBranch,omitempty"`
	Platform  string `json:"platform,omitempty"`
	OSVersion string `json:"osVersion,omitempty"`
	Today     string `json:"today,omitempty"`
}

// TransformInfo is the per-request diagnostic block (Anthropic-path subset of
// the TS TransformInfo).
type TransformInfo struct {
	Compressed        bool   `json:"compressed"`
	Reason            string `json:"reason,omitempty"`
	OrigChars         int    `json:"origChars"`
	CompressedChars   int    `json:"compressedChars"`
	ImageCount        int    `json:"imageCount"`
	ImageBytes        int    `json:"imageBytes"`
	ImagePixels       int    `json:"imagePixels,omitempty"`
	OutgoingTextChars int    `json:"outgoingTextChars,omitempty"`

	SerializedRequestBytes int    `json:"serializedRequestBytes,omitempty"`
	SizeLimitOutcome       string `json:"sizeLimitOutcome,omitempty"`

	PinChars             int                `json:"pinChars,omitempty"`
	PinError             string             `json:"pinError,omitempty"`
	StaticChars          int                `json:"staticChars"`
	DynamicChars         int                `json:"dynamicChars"`
	DynamicBlockCount    int                `json:"dynamicBlockCount"`
	UnknownStaticTags    []string           `json:"unknownStaticTags,omitempty"`
	ChurningStaticTags   []string           `json:"churningStaticTags,omitempty"`
	Env                  *EnvFields         `json:"env,omitempty"`
	SystemSha8           string             `json:"systemSha8,omitempty"`
	FirstUserSha8        string             `json:"firstUserSha8,omitempty"`
	FirstImagePNG        []byte             `json:"-"`
	FirstImageWidth      int                `json:"firstImageWidth,omitempty"`
	FirstImageHeight     int                `json:"firstImageHeight,omitempty"`
	ImagePNGs            [][]byte           `json:"-"`
	ImageDims            []imageDim         `json:"imageDims,omitempty"`
	ImageSourceText      string             `json:"imageSourceText,omitempty"`
	ToolResultImgs       int                `json:"toolResultImgs,omitempty"`
	ToolDocsChars        int                `json:"toolDocsChars,omitempty"`
	DroppedChars         int                `json:"droppedChars"`
	DroppedCodepointsTop map[string]int     `json:"droppedCodepointsTop,omitempty"`
	PassthroughReasons   map[string]int     `json:"passthroughReasons,omitempty"`
	GateEval             *slabGateEval      `json:"gateEval,omitempty"`
	BucketChars          map[string]int     `json:"bucketChars,omitempty"`
	HistoryTextChars     int                `json:"historyTextChars,omitempty"`
	KeptSharpBlocks      int                `json:"keptSharpBlocks,omitempty"`
	Recoverable          []RecoverableBlock `json:"recoverable,omitempty"`
	TruncatedToolResults int                `json:"truncatedToolResults,omitempty"`
	OmittedChars         int                `json:"omittedChars,omitempty"`
	CollapsedTurns       int                `json:"collapsedTurns,omitempty"`
	CollapsedChars       int                `json:"collapsedChars,omitempty"`
	CollapsedImages      int                `json:"collapsedImages,omitempty"`
	HistoryImageSha      string             `json:"historyImageSha,omitempty"`
	CachePrefixSha8      string             `json:"cachePrefixSha8,omitempty"`
	CachePrefixBytes     int                `json:"cachePrefixBytes,omitempty"`
	HistoryReason        string             `json:"historyReason,omitempty"`

	// GPT-path (OpenAI Chat/Responses) fields.
	ImageTokens          int                   `json:"imageTokens,omitempty"`
	BaselineImagedTokens int                   `json:"baselineImagedTokens,omitempty"`
	NativeInjectedTokens int                   `json:"nativeInjectedTokens,omitempty"`
	ImageSourceTexts     []string              `json:"imageSourceTexts,omitempty"`
	ResponsesComposition *ResponsesComposition `json:"responsesComposition,omitempty"`

	cacheControlMarkers      int
	cacheControlMarkersKnown bool
}

// ResponsesComposition is the local o200k decomposition of a Responses
// request plus the planner's native-tool-state classification.
type ResponsesComposition struct {
	Instructions       int `json:"instructions"`
	SystemDeveloper    int `json:"systemDeveloper"`
	UserAssistant      int `json:"userAssistant"`
	FunctionCalls      int `json:"functionCalls"`
	FunctionOutputs    int `json:"functionOutputs"`
	ReasoningEncrypted int `json:"reasoningEncrypted"`
	CompactionOpaque   int `json:"compactionOpaque"`
	ToolsJSON          int `json:"toolsJson"`
	Other              int `json:"other"`
	TotalLocal         int `json:"totalLocal"`
	ImageParts         int `json:"imageParts"`

	CompletedFunctionPairs    *int     `json:"completedFunctionPairs,omitempty"`
	RecentNativeFunctionPairs *int     `json:"recentNativeFunctionPairs,omitempty"`
	OldFunctionPairs          *int     `json:"oldFunctionPairs,omitempty"`
	OpenFunctionCalls         *int     `json:"openFunctionCalls,omitempty"`
	OrphanFunctionOutputs     *int     `json:"orphanFunctionOutputs,omitempty"`
	MalformedFunctionItems    *int     `json:"malformedFunctionItems,omitempty"`
	ImageableFunctionCalls    *int     `json:"imageableFunctionCalls,omitempty"`
	ImageableFunctionOutputs  *int     `json:"imageableFunctionOutputs,omitempty"`
	CollapsedFunctionPairs    *int     `json:"collapsedFunctionPairs,omitempty"`
	CollapsedFunctionCalls    *int     `json:"collapsedFunctionCalls,omitempty"`
	CollapsedFunctionOutputs  *int     `json:"collapsedFunctionOutputs,omitempty"`
	BarrierTypes              []string `json:"barrierTypes,omitempty"`
}

type slabGateEval struct {
	Site string `json:"site"`
	gateEval
}

func bumpPassthrough(info *TransformInfo, reason string) {
	if info.PassthroughReasons == nil {
		info.PassthroughReasons = map[string]int{}
	}
	info.PassthroughReasons[reason]++
}

func bumpBucket(info *TransformInfo, bucket string, chars int) {
	if chars <= 0 {
		return
	}
	if info.BucketChars == nil {
		info.BucketChars = map[string]int{}
	}
	info.BucketChars[bucket] += chars
}

func toolResultBucket(shape string) string {
	switch shape {
	case "structured":
		return "tool_result_json"
	case "log":
		return "tool_result_log"
	}
	return "tool_result_prose"
}

func callerKeepsSharp(fn func(KeepSharpBlock) bool, block KeepSharpBlock) (kept bool) {
	if fn == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			kept = false
		}
	}()
	return fn(block)
}

// --- system text helpers ----------------------------------------------------

func extractSystemText(sys any) (text string, kept any) {
	if sys == nil {
		return "", []any{}
	}
	if s, ok := sys.(string); ok {
		return s, ""
	}
	arr, ok := asArr(sys)
	if !ok {
		return "", []any{}
	}
	hasCacheControlledText := false
	for _, bv := range arr {
		if bm, ok := asMap(bv); ok && blockType(bv) == "text" {
			if _, has := bm["cache_control"]; has {
				hasCacheControlledText = true
				break
			}
		}
	}
	var textParts []string
	keptArr := []any{}
	for _, bv := range arr {
		bm, isMap := asMap(bv)
		if isMap && blockType(bv) == "text" {
			_, hasCC := bm["cache_control"]
			if !hasCacheControlledText || hasCC {
				t, _ := getStr(bm, "text")
				textParts = append(textParts, t)
			} else {
				keptArr = append(keptArr, bv)
			}
		} else {
			keptArr = append(keptArr, bv)
		}
	}
	return strings.Join(textParts, "\n\n"), keptArr
}

func liftIdentityBlock(kept any) (identity string, rest any) {
	arr, ok := asArr(kept)
	if !ok {
		return "", kept
	}
	found := ""
	out := []any{}
	for _, bv := range arr {
		if found == "" {
			if bm, isMap := asMap(bv); isMap && blockType(bv) == "text" {
				t, _ := getStr(bm, "text")
				trimmed := strings.TrimSpace(t)
				matched := ""
				for _, id := range oauthIdentities {
					if trimmed == id {
						matched = id
						break
					}
				}
				if matched != "" {
					found = matched
					continue
				}
			}
		}
		out = append(out, bv)
	}
	return found, out
}

func lastStaticSystemCacheControl(sys any) (any, bool) {
	arr, ok := asArr(sys)
	if !ok {
		return nil, false
	}
	var cc any
	has := false
	for _, bv := range arr {
		bm, isMap := asMap(bv)
		if !isMap || blockType(bv) != "text" {
			continue
		}
		blockCC, hasCC := bm["cache_control"]
		if !hasCC {
			continue
		}
		t, _ := getStr(bm, "text")
		_, body := stripBillingLine(t)
		sd := splitStaticDynamic(body)
		if len(sd.staticText) > 0 {
			cc = blockCC
			has = true
		}
	}
	return cc, has
}

// demoteRelocatedCacheControl drops a `scope` key from a relocated marker: a
// relocated global-scope marker violates Anthropic's prefix rule and 400s.
func demoteRelocatedCacheControl(cc any) any {
	if m, ok := asMap(cc); ok {
		if _, has := m["scope"]; has {
			out := make(map[string]any, len(m))
			for k, v := range m {
				if k != "scope" {
					out[k] = v
				}
			}
			return out
		}
	}
	return cc
}

func stripBillingLine(text string) (kept string, body string) {
	nl := strings.Index(text, "\n")
	first := text
	if nl >= 0 {
		first = text[:nl]
	}
	if strings.HasPrefix(first, "x-anthropic-billing-header:") {
		if nl < 0 {
			return first, ""
		}
		return first, text[nl+1:]
	}
	return "", text
}

// stripMarkdownEnvSection extracts the `# Environment` markdown section
// (churns per turn; would bust the slab cache). Emulates the TS lookahead
// `(?=\n#{1,6}\s|$)` with a manual terminator scan.
func stripMarkdownEnvSection(text string) (kept string, body string) {
	const heading = "# Environment"
	start := -1
	if strings.HasPrefix(text, heading) && (len(text) == len(heading) || !isFactSheetASCIIWord(text[len(heading)])) {
		start = 0
	} else {
		for from := 0; ; {
			i := strings.Index(text[from:], "\n"+heading)
			if i < 0 {
				return "", text
			}
			start = from + i + 1
			after := start + len(heading)
			if after == len(text) || !isFactSheetASCIIWord(text[after]) {
				break
			}
			from = after
		}
	}

	end := len(text)
	for from := start + len(heading); ; {
		i := strings.Index(text[from:], "\n#")
		if i < 0 {
			break
		}
		i += from
		j := i + 1
		for j < len(text) && j-i <= 6 && text[j] == '#' {
			j++
		}
		if r, _ := decodeRuneAt(text, j); isJSSpace(r) {
			end = i
			break
		}
		from = j
	}
	section := strings.TrimRightFunc(text[start:end], isJSSpace)
	return section, text[:start] + text[end:]
}

// --- static/dynamic slab split ----------------------------------------------

var dynamicBlockTags = []string{"env", "context", "git_status", "directoryStructure", "system-reminder"}

var knownStaticTags = map[string]struct{}{
	"types": {}, "skill": {}, "name": {}, "description": {}, "location": {},
	"available_references": {}, "example": {}, "available_skills": {},
	"examples": {}, "rules": {}, "task": {},
	"codeSearchInstructions": {}, "codeSearchToolUseInstructions": {},
	"communicationGuidelines": {}, "gptAgentInstructions": {},
	"outputFormatting": {}, "structuredWorkflow": {}, "toolUseInstructions": {},
}

func isTagNameStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isTagNameChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
}

// findDynamicBlock emulates `<(tag1|...)(\s[^>]*)?>[\s\S]*?</\1>` left-to-right:
// at each position, an opening of any dynamic tag with a later closer wins.
func findDynamicBlock(text string, from int) (start, end int) {
	i := from
	for {
		lt := strings.IndexByte(text[i:], '<')
		if lt < 0 {
			return -1, -1
		}
		lt += i
		for _, tag := range dynamicBlockTags {
			if !strings.HasPrefix(text[lt+1:], tag) {
				continue
			}
			after := lt + 1 + len(tag)
			if after >= len(text) {
				continue
			}
			var gt int
			if text[after] == '>' {
				gt = after
			} else if r, size := decodeRuneAt(text, after); size > 0 && isJSSpace(r) {
				rel := strings.IndexByte(text[after:], '>')
				if rel < 0 {
					continue
				}
				gt = after + rel
			} else {
				continue
			}
			closer := "</" + tag + ">"
			rel := strings.Index(text[gt+1:], closer)
			if rel < 0 {
				continue
			}
			return lt, gt + 1 + rel + len(closer)
		}
		i = lt + 1
	}
}

func decodeRuneAt(s string, i int) (rune, int) {
	if i >= len(s) {
		return 0, 0
	}
	for _, r := range s[i:] {
		return r, len(string(r))
	}
	return 0, 0
}

type staticDynamicSplit struct {
	staticText        string
	dynamicText       string
	blockCount        int
	unknownTags       []string
	staticTagContents map[string]string
	staticTagOrder    []string
}

func splitStaticDynamic(text string) staticDynamicSplit {
	out := staticDynamicSplit{staticTagContents: map[string]string{}}
	if text == "" {
		return out
	}
	var dynamicParts []string
	var staticBuf strings.Builder
	cursor := 0
	for {
		s, e := findDynamicBlock(text, cursor)
		if s < 0 {
			break
		}
		staticBuf.WriteString(text[cursor:s])
		dynamicParts = append(dynamicParts, text[s:e])
		cursor = e
	}
	staticBuf.WriteString(text[cursor:])
	sb := staticBuf.String()

	known := map[string]struct{}{}
	for _, t := range dynamicBlockTags {
		known[t] = struct{}{}
	}
	unknownSet := map[string]struct{}{}
	var unknownOrder []string
	noCloser := map[string]struct{}{}
	i := 0
	for i < len(sb) {
		lt := strings.IndexByte(sb[i:], '<')
		if lt < 0 {
			break
		}
		lt += i
		j := lt + 1
		if j >= len(sb) || !isTagNameStart(sb[j]) {
			i = lt + 1
			continue
		}
		j++
		for j < len(sb) && isTagNameChar(sb[j]) {
			j++
		}
		var gt int
		if j < len(sb) && sb[j] == '>' {
			gt = j
		} else if r, size := decodeRuneAt(sb, j); size > 0 && isJSSpace(r) {
			rel := strings.IndexByte(sb[j:], '>')
			if rel < 0 {
				break
			}
			gt = j + rel
		} else {
			i = lt + 1
			continue
		}
		tag := sb[lt+1 : j]
		contentStart := gt + 1
		closer := "</" + tag + ">"
		end := -1
		if _, skip := noCloser[tag]; !skip {
			rel := strings.Index(sb[contentStart:], closer)
			if rel >= 0 {
				end = contentStart + rel
			}
		}
		if end < 0 {
			noCloser[tag] = struct{}{}
			i = lt + 1
			continue
		}
		if len(tag) <= 64 {
			_, isKnown := known[tag]
			_, isKnownStatic := knownStaticTags[tag]
			if !isKnown && !isKnownStatic {
				if _, seen := unknownSet[tag]; !seen {
					unknownSet[tag] = struct{}{}
					unknownOrder = append(unknownOrder, tag)
				}
			}
			if _, seen := out.staticTagContents[tag]; !seen {
				out.staticTagOrder = append(out.staticTagOrder, tag)
			}
			out.staticTagContents[tag] += sb[contentStart:end]
		}
		i = end + len(closer)
	}

	out.staticText = strings.TrimSpace(collapseNewlineRuns(sb))
	out.dynamicText = strings.Join(dynamicParts, "\n\n")
	out.blockCount = len(dynamicParts)
	out.unknownTags = unknownOrder
	return out
}

// --- static tag churn (session-scoped LRU) ----------------------------------

func fnv1aU16(text string) uint32 {
	h := uint32(0x811c9dc5)
	if isASCII(text) {
		for i := 0; i < len(text); i++ {
			h ^= uint32(text[i])
			h *= 0x01000193
		}
		return h
	}
	for _, r := range text {
		if r >= 0x10000 {
			r -= 0x10000
			h ^= uint32(0xd800 + (r >> 10))
			h *= 0x01000193
			h ^= uint32(0xdc00 + (r & 0x3ff))
			h *= 0x01000193
		} else {
			h ^= uint32(r)
			h *= 0x01000193
		}
	}
	return h
}

const tagObservationsMax = 4096

var (
	tagObsMu   sync.Mutex
	tagObsMap  = map[tagObsKey]*list.Element{}
	tagObsList = list.New()
)

type tagObsKey struct {
	session string
	tag     string
}

type tagObsEntry struct {
	key        string
	sessionLen int
	hash       uint32
}

func observeStaticTagChurn(sessionKey string, tagOrder []string, tagContents map[string]string) []string {
	var inlineHashes [32]uint32
	hashes := inlineHashes[:]
	if len(tagOrder) > len(inlineHashes) {
		hashes = make([]uint32, len(tagOrder))
	} else {
		hashes = hashes[:len(tagOrder)]
	}
	for i, tag := range tagOrder {
		hashes[i] = fnv1aU16(tagContents[tag])
	}

	tagObsMu.Lock()
	defer tagObsMu.Unlock()
	var churned []string
	for i, tag := range tagOrder {
		hash := hashes[i]
		lookupKey := tagObsKey{session: sessionKey, tag: tag}
		if el, ok := tagObsMap[lookupKey]; ok {
			entry := el.Value.(*tagObsEntry)
			if entry.hash != hash {
				churned = append(churned, tag)
				entry.hash = hash
			}
			tagObsList.MoveToBack(el)
			continue
		}

		key := sessionKey + "\x00" + tag
		entry := &tagObsEntry{key: key, sessionLen: len(sessionKey), hash: hash}
		el := tagObsList.PushBack(entry)
		tagObsMap[tagObsKey{session: key[:entry.sessionLen], tag: key[entry.sessionLen+1:]}] = el
	}
	for len(tagObsMap) > tagObservationsMax {
		oldest := tagObsList.Front()
		if oldest == nil {
			break
		}
		oldestEntry := oldest.Value.(*tagObsEntry)
		tagObsList.Remove(oldest)
		delete(tagObsMap, tagObsKey{
			session: oldestEntry.key[:oldestEntry.sessionLen],
			tag:     oldestEntry.key[oldestEntry.sessionLen+1:],
		})
	}
	return churned
}

// --- env field extraction ----------------------------------------------------

var (
	envCwdRe      = regexp.MustCompile(`(?i)(?:^|\n)\s*Working directory:\s*(.+?)\s*(?:\n|$)`)
	envGitRepoRe  = regexp.MustCompile(`(?i)(?:^|\n)\s*Is directory a git repo:\s*(Yes|No)\b`)
	envPlatformRe = regexp.MustCompile(`(?i)(?:^|\n)\s*Platform:\s*(.+?)\s*(?:\n|$)`)
	envOSVerRe    = regexp.MustCompile(`(?i)(?:^|\n)\s*OS Version:\s*(.+?)\s*(?:\n|$)`)
	envTodayRe    = regexp.MustCompile(`(?i)(?:^|\n)\s*Today'?s date:\s*(.+?)\s*(?:\n|$)`)
	branchRe1     = regexp.MustCompile(`(?i)(?:^|\n)\s*(?:On branch|Branch:)\s*([^\s\n]+)`)
	branchRe2     = regexp.MustCompile(`(?i)(?:^|\n)\s*Current branch:\s*([^\s\n]+)`)
)

func firstTagBody(text, tag string) (string, bool) {
	open := "<" + tag + ">"
	close_ := "</" + tag + ">"
	haystack := strings.ToLower(text)
	start := strings.Index(haystack, open)
	if start < 0 {
		return "", false
	}
	end := strings.Index(haystack[start+len(open):], close_)
	if end < 0 {
		return "", false
	}
	return text[start+len(open) : start+len(open)+end], true
}

func extractEnvFields(dynamicText string) EnvFields {
	var out EnvFields
	if dynamicText == "" {
		return out
	}
	if body, ok := firstTagBody(dynamicText, "env"); ok {
		if m := envCwdRe.FindStringSubmatch(body); m != nil {
			out.Cwd = strings.TrimSpace(m[1])
		}
		if m := envGitRepoRe.FindStringSubmatch(body); m != nil {
			v := strings.EqualFold(m[1], "yes")
			out.IsGitRepo = &v
		}
		if m := envPlatformRe.FindStringSubmatch(body); m != nil {
			out.Platform = strings.TrimSpace(m[1])
		}
		if m := envOSVerRe.FindStringSubmatch(body); m != nil {
			out.OSVersion = strings.TrimSpace(m[1])
		}
		if m := envTodayRe.FindStringSubmatch(body); m != nil {
			out.Today = strings.TrimSpace(m[1])
		}
	}
	if m := branchRe1.FindStringSubmatch(dynamicText); m != nil {
		out.GitBranch = strings.TrimSpace(m[1])
	} else if m := branchRe2.FindStringSubmatch(dynamicText); m != nil {
		out.GitBranch = strings.TrimSpace(m[1])
	}
	return out
}

func (e EnvFields) isEmpty() bool {
	return e.Cwd == "" && e.IsGitRepo == nil && e.GitBranch == "" && e.Platform == "" && e.OSVersion == "" && e.Today == ""
}

// --- misc helpers -------------------------------------------------------------

func firstUserText(req map[string]any) string {
	messages, _ := asArr(req["messages"])
	for _, mv := range messages {
		m, ok := asMap(mv)
		if !ok {
			continue
		}
		if role, _ := getStr(m, "role"); role != "user" {
			continue
		}
		if s, ok := m["content"].(string); ok {
			return u16Slice(s, 0, 4096)
		}
		if arr, ok := asArr(m["content"]); ok {
			for _, bv := range arr {
				if bm, ok := asMap(bv); ok && blockType(bv) == "text" {
					if t, ok := getStr(bm, "text"); ok {
						return u16Slice(t, 0, 4096)
					}
				}
			}
		}
		return ""
	}
	return ""
}

func firstMessageHasSystemReminder(messages []any) bool {
	if len(messages) == 0 {
		return false
	}
	first, ok := asMap(messages[0])
	if !ok {
		return false
	}
	if s, ok := first["content"].(string); ok && strings.Contains(s, "<system-reminder>") {
		return true
	}
	if arr, ok := asArr(first["content"]); ok {
		for _, bv := range arr {
			if bm, ok := asMap(bv); ok && blockType(bv) == "text" {
				if t, ok := getStr(bm, "text"); ok && strings.Contains(t, "<system-reminder>") {
					return true
				}
			}
		}
	}
	return false
}

func renderToolDoc(t map[string]any) string {
	name, _ := getStr(t, "name")
	if name == "" {
		name = "?"
	}
	parts := []string{"## Tool: " + name}
	if desc, ok := getStr(t, "description"); ok && desc != "" {
		parts = append(parts, desc)
	}
	if schema, has := t["input_schema"]; has {
		parts = append(parts, "```json\n"+jsStringifyString(schema)+"\n```")
	}
	return strings.Join(parts, "\n")
}

func approxBlockBytes(blk map[string]any) int {
	src, _ := asMap(blk["source"])
	if png, ok := src["data"].(pngBase64); ok {
		return len(png)
	}
	b64, _ := getStr(src, "data")
	pad := 0
	if strings.HasSuffix(b64, "==") {
		pad = 2
	} else if strings.HasSuffix(b64, "=") {
		pad = 1
	}
	return (len(b64)*3)/4 - pad
}

type renderedBlocks struct {
	blocks            []map[string]any
	pngs              [][]byte
	dims              []imageDim
	droppedChars      int
	droppedCodepoints map[rune]int
	pixels            int
}

func textToImageBlocks(text string, cols int, shrinkWidth bool, style render.RenderStyle, maxHeightPx int) (*renderedBlocks, error) {
	effectiveCols := cols
	if shrinkWidth {
		effectiveCols = render.ShrinkColsToContent(text, cols, 1, style.Font)
	}
	imgs, err := render.RenderTextToPngsWithCharLimit(text, effectiveCols, render.DenseContentCharsPerImage, style, maxHeightPx, nil)
	if err != nil {
		return nil, err
	}
	out := &renderedBlocks{droppedCodepoints: map[rune]int{}}
	for _, img := range imgs {
		out.blocks = append(out.blocks, makeImageBlock(img.PNG))
		out.pngs = append(out.pngs, img.PNG)
		out.dims = append(out.dims, imageDim{img.Width, img.Height})
		out.droppedChars += img.DroppedChars
		out.pixels += img.Width * img.Height
		for cp, n := range img.DroppedCodepoints {
			out.droppedCodepoints[cp] += n
		}
	}
	return out, nil
}

func recordRecoverable(info *TransformInfo, emit bool, kind, toolUseID, text string, imageCount int) {
	if !emit {
		return
	}
	id := "rec_" + sha8(kind+"\x00"+toolUseID+"\x00"+text)
	info.Recoverable = append(info.Recoverable, RecoverableBlock{
		ID: id, Kind: kind, ToolUseID: toolUseID, Text: text, ImageCount: imageCount,
	})
}

func historyImageSha8(messages []any) string {
	if len(messages) == 0 {
		return ""
	}
	synthetic, ok := asMap(messages[0])
	if !ok {
		return ""
	}
	arr, ok := asArr(synthetic["content"])
	if !ok {
		return ""
	}
	h := sha256.New()
	var scratch [16 << 10]byte
	hasData := false
	for _, bv := range arr {
		if blockType(bv) == "image" {
			if bm, ok := asMap(bv); ok {
				if src, ok := asMap(bm["source"]); ok {
					switch data := src["data"].(type) {
					case string:
						if len(data) > 0 {
							hasData = true
							_, _ = h.Write(unsafe.Slice(unsafe.StringData(data), len(data)))
						}
					case pngBase64:
						if len(data) > 0 {
							hasData = true
							for len(data) > 0 {
								n := minInt(len(data), 12<<10)
								encoded := base64.StdEncoding.EncodedLen(n)
								base64.StdEncoding.Encode(scratch[:encoded], data[:n])
								_, _ = h.Write(scratch[:encoded])
								data = data[n:]
							}
						}
					}
				}
			}
		}
	}
	if !hasData {
		return ""
	}
	var sum [sha256.Size]byte
	return hex.EncodeToString(h.Sum(sum[:0])[:4])
}

func relocateAnchorToHistoryImage(messages []any, anchorOrdinal int, hasOrdinal bool) {
	var historyImg map[string]any
	for _, mv := range messages {
		m, ok := asMap(mv)
		if !ok {
			continue
		}
		arr, ok := asArr(m["content"])
		if !ok || len(arr) == 0 {
			continue
		}
		first, ok := asMap(arr[0])
		if !ok || blockType(arr[0]) != "text" {
			continue
		}
		if t, _ := getStr(first, "text"); t != HistorySyntheticIntro {
			continue
		}
		var imgsInMsg []map[string]any
		for _, bv := range arr {
			if blockType(bv) == "image" {
				if bm, ok := asMap(bv); ok {
					imgsInMsg = append(imgsInMsg, bm)
				}
			}
		}
		if len(imgsInMsg) == 0 {
			break
		}
		if hasOrdinal && anchorOrdinal >= 0 && anchorOrdinal < len(imgsInMsg) {
			historyImg = imgsInMsg[anchorOrdinal]
		} else {
			historyImg = imgsInMsg[len(imgsInMsg)-1]
		}
		break
	}
	if historyImg == nil {
		return
	}
	var slabAnchor map[string]any
	for _, mv := range messages {
		m, ok := asMap(mv)
		if !ok {
			continue
		}
		arr, ok := asArr(m["content"])
		if !ok {
			continue
		}
		hasBoundary := false
		for _, bv := range arr {
			if bm, ok := asMap(bv); ok && blockType(bv) == "text" {
				if t, _ := getStr(bm, "text"); t == endOfRenderedContext {
					hasBoundary = true
					break
				}
			}
		}
		if !hasBoundary {
			continue
		}
		for _, bv := range arr {
			if bm, ok := asMap(bv); ok {
				if blockType(bv) == "text" {
					if t, _ := getStr(bm, "text"); t == endOfRenderedContext {
						break
					}
				}
				if blockType(bv) == "image" {
					if _, hasCC := bm["cache_control"]; hasCC {
						slabAnchor = bm
					}
				}
			}
		}
		break
	}
	if slabAnchor == nil {
		return
	}
	historyImg["cache_control"] = demoteRelocatedCacheControl(slabAnchor["cache_control"])
	delete(slabAnchor, "cache_control")
}

const maxCachePrefixScratchBytes = 1 << 20

var cachePrefixScratchCache = make(chan []byte, runtime.GOMAXPROCS(0))

func getCachePrefixScratch() []byte {
	select {
	case scratch := <-cachePrefixScratchCache:
		return scratch[:0]
	default:
		return make([]byte, 0, 4096)
	}
}

func putCachePrefixScratch(scratch []byte) {
	if cap(scratch) > maxCachePrefixScratchBytes {
		return
	}
	clear(scratch)
	select {
	case cachePrefixScratchCache <- scratch[:0]:
	default:
	}
}

func cachePrefixDigest(req map[string]any) (string, int, bool) {
	msgs, _ := asArr(req["messages"])
	boundary := -1
	for i, mv := range msgs {
		m, ok := asMap(mv)
		if !ok {
			continue
		}
		arr, ok := asArr(m["content"])
		if !ok {
			continue
		}
		isHistory := false
		if len(arr) > 0 && blockType(arr[0]) == "text" {
			if fm, ok := asMap(arr[0]); ok {
				if t, _ := getStr(fm, "text"); t == HistorySyntheticIntro {
					isHistory = true
				}
			}
		}
		hasSlab := false
		for _, bv := range arr {
			if bm, ok := asMap(bv); ok && blockType(bv) == "text" {
				if t, _ := getStr(bm, "text"); t == endOfRenderedContext {
					hasSlab = true
					break
				}
			}
		}
		if isHistory || hasSlab {
			boundary = i
		}
	}
	if boundary < 0 {
		return "", 0, false
	}
	h := sha256.New()
	var separator [1]byte
	scratch := getCachePrefixScratch()
	partCount := 0
	prefixUnits := 0
	appendString := func(s string) {
		if partCount > 0 {
			_, _ = h.Write(separator[:])
			prefixUnits++
		}
		_, _ = h.Write(unsafe.Slice(unsafe.StringData(s), len(s)))
		prefixUnits += u16len(s)
		partCount++
	}
	appendValue := func(v any) {
		scratch = appendJSValue(scratch[:0], v)
		appendString(unsafe.String(unsafe.SliceData(scratch), len(scratch)))
	}
	if tools, ok := asArr(req["tools"]); ok {
		for _, t := range tools {
			appendValue(t)
		}
	}
	if s, ok := req["system"].(string); ok {
		appendString(s)
	} else if arr, ok := asArr(req["system"]); ok {
		for _, b := range arr {
			appendValue(b)
		}
	}
	for i := 0; i <= boundary; i++ {
		m, ok := asMap(msgs[i])
		if !ok {
			continue
		}
		if s, ok := m["content"].(string); ok {
			appendString(s)
		} else if arr, ok := asArr(m["content"]); ok {
			for _, bv := range arr {
				if s, ok := bv.(string); ok {
					appendString(s)
				} else {
					appendValue(bv)
				}
			}
		}
	}
	var sum [sha256.Size]byte
	digest := hex.EncodeToString(h.Sum(sum[:0])[:4])
	putCachePrefixScratch(scratch)
	return digest, prefixUnits, true
}

func countOutgoingTextChars(req map[string]any) int {
	n := 0
	if s, ok := req["system"].(string); ok {
		n += u16len(s)
	} else if arr, ok := asArr(req["system"]); ok {
		for _, bv := range arr {
			if bm, ok := asMap(bv); ok && blockType(bv) == "text" {
				if t, ok := getStr(bm, "text"); ok {
					n += u16len(t)
				}
			}
		}
	}
	if tools, ok := asArr(req["tools"]); ok {
		for _, tv := range tools {
			tm, ok := asMap(tv)
			if !ok {
				continue
			}
			if name, ok := getStr(tm, "name"); ok {
				n += u16len(name)
			}
			if desc, ok := getStr(tm, "description"); ok {
				n += u16len(desc)
			}
			if schema, has := tm["input_schema"]; has {
				n += jsonStringifyLen(schema)
			}
		}
	}
	msgs, _ := asArr(req["messages"])
	for _, mv := range msgs {
		m, ok := asMap(mv)
		if !ok {
			continue
		}
		if s, ok := m["content"].(string); ok {
			n += u16len(s)
			continue
		}
		arr, ok := asArr(m["content"])
		if !ok {
			continue
		}
		for _, bv := range arr {
			bm, ok := asMap(bv)
			if !ok {
				continue
			}
			switch blockType(bv) {
			case "text":
				if t, ok := getStr(bm, "text"); ok {
					n += u16len(t)
				}
			case "tool_use":
				if name, ok := getStr(bm, "name"); ok {
					n += u16len(name)
				}
				if input, has := bm["input"]; has {
					n += jsonStringifyLen(input)
				}
			case "tool_result":
				if id, ok := getStr(bm, "tool_use_id"); ok {
					n += u16len(id)
				}
				if s, ok := bm["content"].(string); ok {
					n += u16len(s)
				} else if ia, ok := asArr(bm["content"]); ok {
					for _, sv := range ia {
						if sm, ok := asMap(sv); ok && blockType(sv) == "text" {
							if t, ok := getStr(sm, "text"); ok {
								n += u16len(t)
							}
						}
					}
				}
			case "thinking":
				if t, ok := getStr(bm, "thinking"); ok {
					n += u16len(t)
				}
			}
		}
	}
	return n
}

// --- history collapse wrapper -------------------------------------------------

func runHistoryCollapse(req map[string]any, info *TransformInfo, o *resolvedOptions, droppedCodepoints map[rune]int, protectedPrefix int, relocate bool) (bool, error) {
	msgs, ok := asArr(req["messages"])
	if !ok || len(msgs) == 0 {
		return false, nil
	}
	historyCpt := HistoryCharsPerToken
	if o.charsPerTokenSet {
		historyCpt = o.CharsPerToken
	}
	horizon := maxInt(1, o.HistoryAmortizationHorizon)
	geo := denseGateGeometry(o)
	profitable := func(text string, cols int) bool {
		return isCompressionProfitableAmortized(
			text, geo.cols, 0, historyCpt, horizon,
			o.PriorWarmTokens, o.PriorWarmImageTokens, true, geo.maxChars, geo,
		)
	}
	ho := historyDefaults()
	ho.cols = o.Cols
	ho.protectedPrefix = protectedPrefix
	ho.reflow = o.Reflow
	ho.style = geo.style
	ho.maxHeightPx = geo.maxHeightPx
	newMessages, histInfo, err := collapseHistory(msgs, profitable, ho)
	if err != nil {
		return false, err
	}
	if histInfo.collapsedTurns > 0 {
		req["messages"] = newMessages
		info.CollapsedTurns = histInfo.collapsedTurns
		info.CollapsedChars = histInfo.collapsedChars
		info.CollapsedImages = histInfo.collapsedImages
		info.ImageCount += histInfo.collapsedImages
		info.ImageBytes += histInfo.collapsedImageBytes
		info.ImagePixels += histInfo.collapsedImagePixels
		info.ImagePNGs = append(info.ImagePNGs, histInfo.collapsedPngs...)
		info.ImageDims = append(info.ImageDims, histInfo.collapsedImageDims...)
		info.DroppedChars += histInfo.droppedChars
		for cp, n := range histInfo.droppedCodepoints {
			droppedCodepoints[cp] += n
		}
		info.HistoryReason = "collapsed"
		info.HistoryTextChars = histInfo.collapsedChars
		info.HistoryImageSha = historyImageSha8(newMessages)
		bumpBucket(info, "history", histInfo.collapsedChars)
		if relocate && histInfo.hasCarryOver {
			relocateAnchorToHistoryImage(newMessages, histInfo.carryOverImageOrdinal, true)
		}
		return true, nil
	}
	if histInfo.reason != "" {
		info.HistoryReason = histInfo.reason
	}
	return false, nil
}

func applyPins(req map[string]any, info *TransformInfo, pins []pin) {
	msgs, ok := asArr(req["messages"])
	if len(pins) == 0 || !ok || len(msgs) == 0 {
		return
	}
	chars := appendPinBlock(msgs, pins)
	if chars > 0 {
		info.PinChars = chars
	}
}

func finalizeEarly(req map[string]any, bodyBytes int, info *TransformInfo, o *resolvedOptions, droppedCodepoints map[rune]int, pins []pin) ([]byte, bool, error) {
	collapsed := false
	if msgs, ok := asArr(req["messages"]); ok && len(msgs) > 0 {
		protectedPrefix := 0
		if firstMessageHasSystemReminder(msgs) {
			protectedPrefix = 1
		}
		var err error
		collapsed, err = runHistoryCollapse(req, info, o, droppedCodepoints, protectedPrefix, false)
		if err != nil {
			return nil, false, err
		}
	}
	applyPins(req, info, pins)
	info.OutgoingTextChars = countOutgoingTextChars(req)
	return jsStringifyCap(req, openAIJSONCapacity(bodyBytes, info.ImageBytes)), collapsed, nil
}

const imageInstructionHeaderBase = "=================== SESSION CONFIGURATION PAGES ===================\n" +
	"pxpipe (this user's local proxy) rendered this session's configuration" +
	" into the following images to reduce token cost. Read the pages carefully and follow them as" +
	" your operating instructions for this session." +
	" For exact identifiers, paths, hashes, version strings, and numbers, use the adjacent" +
	" exact-value factsheet; if a value was only visible in an image and is not in that factsheet," +
	" do not guess it — say it is not safe to quote from the image and re-read the source text."

const reflowNoteImg = " The glyph ↵ (U+21B5) marks an original hard line break in content — treat as a real newline."

const toolReferenceIntro = "=== TOOL REFERENCE ===\n" +
	"pxpipe (this user's local proxy) moved the full tool documentation for this" +
	" session here to reduce token cost. Each tool in the tools list carries a short" +
	" stub description pointing here; the entry under the matching" +
	" \"## Tool: <name>\" heading below is the complete description for that tool.\n\n"

// TransformRequest is the Go port of pxpipe's transformRequest.
func TransformRequest(body []byte, opts *TransformOptions) (outBody []byte, info *TransformInfo) {
	o := resolveOptions(opts)
	info = &TransformInfo{}
	droppedCodepoints := map[rune]int{}

	if !o.Compress {
		info.Reason = "compress=false"
		return body, info
	}

	req, err := parseOrderedJSON(body)
	if err != nil {
		info.Reason = "parse_error: " + err.Error()
		return body, info
	}

	out, err := transformParsed(req, body, o, info, droppedCodepoints)
	if err != nil {
		info.Reason = "transform_error: " + err.Error()
		info.Compressed = false
		return body, info
	}
	info.cacheControlMarkers = countCacheControlValue(req)
	info.cacheControlMarkersKnown = true
	return out, info
}

func transformParsed(req map[string]any, body []byte, o *resolvedOptions, info *TransformInfo, droppedCodepoints map[rune]int) ([]byte, error) {
	// Step 0: fold user pins and strip their commands from the outbound copy.
	var pins []pin
	pinsRewrote := false
	if msgs, ok := asArr(req["messages"]); ok && canAppendPinBlock(msgs) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					pins = nil
					pinsRewrote = false
					info.PinError = fmt.Sprint(r)
				}
			}()
			pins = foldPins(msgs, req["system"])
			stripped, stripChanged := stripPinCommands(msgs)
			strippedSystem, sysChanged := stripPinCommandsFromSystem(req["system"])
			pinsRewrote = len(pins) > 0 || sysChanged || stripChanged
			req["messages"] = stripped
			if sysChanged {
				req["system"] = strippedSystem
			}
		}()
	}

	// Step 1: split system text into billing line / dynamic blocks / static slab.
	systemStaticCC, hasSystemStaticCC := lastStaticSystemCacheControl(req["system"])
	rawSysText, rawSysRemainder := extractSystemText(req["system"])
	keptIdentity, sysRemainder := liftIdentityBlock(rawSysRemainder)
	billingLine, sysBody := stripBillingLine(rawSysText)
	envMarkdown, sysBodyNoEnv := stripMarkdownEnvSection(sysBody)
	splitSystem := splitStaticDynamic(sysBodyNoEnv)
	staticText := splitSystem.staticText
	dynamicText := splitSystem.dynamicText
	inlineIdentity := ""
	for _, id := range oauthIdentities {
		if strings.HasPrefix(staticText, id) {
			inlineIdentity = id
			break
		}
	}
	if inlineIdentity != "" {
		staticText = strings.TrimLeftFunc(staticText[len(inlineIdentity):], isJSSpace)
	}
	preservedIdentity := keptIdentity
	if preservedIdentity == "" {
		preservedIdentity = inlineIdentity
	}
	info.StaticChars = u16len(staticText)
	info.DynamicBlockCount = splitSystem.blockCount
	if len(splitSystem.unknownTags) > 0 {
		info.UnknownStaticTags = splitSystem.unknownTags
	}
	env := extractEnvFields(dynamicText)
	if !env.isEmpty() {
		e := env
		info.Env = &e
	}
	firstUser := firstUserText(req)
	if firstUser != "" {
		info.FirstUserSha8 = sha8(firstUser)
	}
	if len(splitSystem.staticTagContents) > 0 {
		sessionKey := info.FirstUserSha8
		if sessionKey == "" {
			sessionKey = "global"
		}
		churning := observeStaticTagChurn(sessionKey, splitSystem.staticTagOrder, splitSystem.staticTagContents)
		if len(churning) > 0 {
			info.ChurningStaticTags = churning
		}
	}
	info.DynamicChars = u16len(dynamicText)

	// Step 2: move tool docs into the imaged Tool Reference, stubbing originals.
	toolDocsText := ""
	var toolsRewritten []any
	if tools, ok := asArr(req["tools"]); o.CompressTools && ok && len(tools) > 0 {
		var docs []string
		toolsRewritten = make([]any, len(tools))
		for i, tv := range tools {
			tm, isMap := asMap(tv)
			if !isMap {
				toolsRewritten[i] = tv
				continue
			}
			if tType, hasType := getStr(tm, "type"); hasType && tType != "custom" {
				toolsRewritten[i] = tv
				continue
			}
			if deferLoading, ok := tm["defer_loading"].(bool); ok && deferLoading {
				toolsRewritten[i] = tv
				continue
			}
			docs = append(docs, renderToolDoc(tm))
			schema := tm["input_schema"]
			_, hasSchema := tm["input_schema"]
			if sm, ok := asMap(schema); ok {
				stripped := stripSchemaDescriptions(sm, 0)
				if strippedMap, ok := asMap(stripped); ok && schemaHasStructure(strippedMap) {
					schema = strippedMap
				}
			}
			name, _ := getStr(tm, "name")
			if name == "" {
				name = "?"
			}
			readFirstNote := ""
			if _, isReadFirst := readFirstTools[name]; isReadFirst {
				readFirstNote = " Requires a Read of the same file earlier in THIS session when the file" +
					" already exists — the call is rejected otherwise; file content recalled" +
					" from imaged or prior-session context does not satisfy this."
			}
			nt := cloneMap(tm)
			nt["description"] = "ⓘ Full docs: see \"## Tool: " + name + "\" in the Tool Reference section." + readFirstNote
			if hasSchema {
				nt["input_schema"] = schema
			}
			toolsRewritten[i] = nt
		}
		toolDocsText = strings.Join(docs, "\n\n")
		if toolDocsText != "" {
			info.ToolDocsChars = u16len(toolDocsText)
		}
	}

	toolReferenceText := ""
	if toolDocsText != "" {
		toolReferenceText = toolReferenceIntro + toolDocsText + "\n=== END TOOL REFERENCE ==="
	}
	var combinedParts []string
	for _, s := range []string{staticText, toolReferenceText} {
		if s != "" {
			combinedParts = append(combinedParts, s)
		}
	}
	combinedRaw := strings.Join(combinedParts, "\n\n")
	combined := maybeReflow(compactSlabWhitespace(combinedRaw), o.Reflow)
	info.OrigChars = u16len(combinedRaw)
	info.CompressedChars = 0
	if combined != "" {
		info.SystemSha8 = sha8(combined)
	}

	if u16len(combined) < o.MinCompressChars {
		info.Reason = "below_min_chars (" + strconv.Itoa(u16len(combined)) + " < " + strconv.Itoa(o.MinCompressChars) + ")"
		finalBody, collapsed, err := finalizeEarly(req, len(body), info, o, droppedCodepoints, pins)
		if err != nil {
			return nil, err
		}
		if collapsed {
			info.Compressed = true
			return finalBody, nil
		}
		if pinsRewrote {
			return finalBody, nil
		}
		return body, nil
	}

	denseGeo := denseGateGeometry(o)
	slabCpt := SlabCharsPerToken
	if o.charsPerTokenSet {
		slabCpt = o.CharsPerToken
	}
	header := imageInstructionHeaderBase
	if o.Reflow {
		header += reflowNoteImg
	}
	header += "\n====================== BEGIN RENDERED CONTEXT ======================\n"
	combinedWithHeader := header + combined
	slabCols := render.ShrinkColsToContent(combinedWithHeader, o.Cols, 1, denseGeo.style.Font)
	slabGate := evalCompressionProfitability(
		combinedWithHeader, slabCols, 0, slabCpt, o.PriorWarmTokens, o.PriorWarmImageTokens, false, denseGeo,
	)
	if slabGate != nil {
		info.GateEval = &slabGateEval{Site: "slab", gateEval: *slabGate}
	}
	if !isCompressionProfitable(
		combinedWithHeader, slabCols, 0, slabCpt, o.PriorWarmTokens, o.PriorWarmImageTokens,
		false, render.ReadableCharsPerImage, denseGeo,
	) {
		info.Reason = "not_profitable (slab=" + strconv.Itoa(u16len(combined)) + " chars)"
		bumpPassthrough(info, "not_profitable")
		finalBody, collapsed, err := finalizeEarly(req, len(body), info, o, droppedCodepoints, pins)
		if err != nil {
			return nil, err
		}
		if collapsed {
			info.Compressed = true
			return finalBody, nil
		}
		if pinsRewrote {
			return finalBody, nil
		}
		return body, nil
	}

	// Step 3: render the slab to PNG pages.
	images, err := render.RenderTextToPngs(combinedWithHeader, slabCols, denseGeo.style, denseGeo.maxHeightPx, nil)
	if err != nil {
		return nil, err
	}
	var imageBlocks []any
	for i, img := range images {
		info.ImageBytes += len(img.PNG)
		info.ImagePixels += img.Width * img.Height
		info.DroppedChars += img.DroppedChars
		for cp, n := range img.DroppedCodepoints {
			droppedCodepoints[cp] += n
		}
		block := makeImageBlock(img.PNG)
		if i == len(images)-1 && hasSystemStaticCC {
			block["cache_control"] = demoteRelocatedCacheControl(systemStaticCC)
		}
		imageBlocks = append(imageBlocks, block)
	}
	info.ImageCount = len(imageBlocks)
	info.CompressedChars += u16len(combinedRaw)
	bumpBucket(info, "static_slab", u16len(combinedRaw))
	if len(images) > 0 {
		info.FirstImagePNG = images[0].PNG
		info.FirstImageWidth = images[0].Width
		info.FirstImageHeight = images[0].Height
		for _, img := range images {
			info.ImagePNGs = append(info.ImagePNGs, img.PNG)
			info.ImageDims = append(info.ImageDims, imageDim{img.Width, img.Height})
		}
		info.ImageSourceText = u16Slice(combinedWithHeader, 0, 65_536)
	}

	// Step 4: splice images back; volatile system text stays in place.
	var sysTail []any
	if preservedIdentity != "" {
		sysTail = append(sysTail, textBlock(preservedIdentity))
	}
	if billingLine != "" {
		sysTail = append(sysTail, textBlock(billingLine))
	}
	if dynamicText != "" {
		sysTail = append(sysTail, textBlock(dynamicText))
	}
	if envMarkdown != "" {
		sysTail = append(sysTail, textBlock(envMarkdown))
	}
	if remArr, ok := asArr(sysRemainder); ok {
		sysTail = append(sysTail, remArr...)
	}
	if len(sysTail) > 0 {
		req["system"] = sysTail
	} else {
		delete(req, "system")
	}

	msgs, _ := asArr(req["messages"])
	firstUserIdx := -1
	for i, mv := range msgs {
		if m, ok := asMap(mv); ok {
			if role, _ := getStr(m, "role"); role == "user" {
				firstUserIdx = i
				break
			}
		}
	}
	if firstUserIdx >= 0 {
		m, _ := asMap(msgs[firstUserIdx])
		var existing []any
		if s, ok := m["content"].(string); ok {
			existing = []any{textBlock(s)}
		} else if arr, ok := asArr(m["content"]); ok {
			existing = arr
		}
		newContent := append([]any{}, imageBlocks...)
		if slabFactSheet := FactSheetText(combinedRaw); slabFactSheet != "" {
			newContent = append(newContent, textBlock(slabFactSheet))
		}
		newContent = append(newContent, textBlock(endOfRenderedContext))
		newContent = append(newContent, existing...)
		m["content"] = newContent
	}

	// Step 5b: compress tool_result content across ALL user messages.
	if o.CompressToolResults {
		if err := compressToolResults(req, o, info, denseGeo, droppedCodepoints); err != nil {
			return nil, err
		}
	}

	if toolsRewritten != nil {
		req["tools"] = toolsRewritten
	}

	// Step 6: history collapse (protected prefix = slab-bearing first user message).
	if msgs, ok := asArr(req["messages"]); ok && len(msgs) > 0 {
		slabAnchorIdx := -1
		for i, mv := range msgs {
			if m, ok := asMap(mv); ok {
				if role, _ := getStr(m, "role"); role == "user" {
					slabAnchorIdx = i
					break
				}
			}
		}
		protectedPrefix := 0
		if slabAnchorIdx >= 0 {
			protectedPrefix = slabAnchorIdx + 1
		}
		if _, err := runHistoryCollapse(req, info, o, droppedCodepoints, protectedPrefix, true); err != nil {
			return nil, err
		}
	}

	info.Compressed = true
	if sha, bytes, ok := cachePrefixDigest(req); ok {
		info.CachePrefixSha8 = sha
		info.CachePrefixBytes = bytes
	}
	if len(droppedCodepoints) > 0 {
		type cpCount struct {
			cp rune
			n  int
		}
		var sorted []cpCount
		for cp, n := range droppedCodepoints {
			sorted = append(sorted, cpCount{cp, n})
		}
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].n != sorted[j].n {
				return sorted[i].n > sorted[j].n
			}
			return sorted[i].cp < sorted[j].cp
		})
		if len(sorted) > 20 {
			sorted = sorted[:20]
		}
		top := map[string]int{}
		for _, e := range sorted {
			top[fmt.Sprintf("U+%04X", e.cp)] = e.n
		}
		info.DroppedCodepointsTop = top
	}
	applyPins(req, info, pins)
	info.OutgoingTextChars = countOutgoingTextChars(req)
	return jsStringifyCap(req, openAIJSONCapacity(len(body), info.ImageBytes)), nil
}

func compressToolResults(req map[string]any, o *resolvedOptions, info *TransformInfo, denseGeo gateGeometry, droppedCodepoints map[rune]int) error {
	msgs, _ := asArr(req["messages"])
	linesPerImage := maxInt(1, (denseGeo.maxHeightPx-2*render.PadY)/render.RenderCellHeight(denseGeo.style))
	for _, mv := range msgs {
		m, ok := asMap(mv)
		if !ok {
			continue
		}
		if role, _ := getStr(m, "role"); role != "user" {
			continue
		}
		arr, ok := asArr(m["content"])
		if !ok {
			continue
		}
		var rewritten []any
		changed := false
		for _, bv := range arr {
			if blockType(bv) != "tool_result" {
				rewritten = append(rewritten, bv)
				continue
			}
			tr, _ := asMap(bv)
			if isErr, ok := tr["is_error"].(bool); ok && isErr {
				rewritten = append(rewritten, bv)
				continue
			}
			toolUseID, _ := getStr(tr, "tool_use_id")
			innerRaw := tr["content"]
			if innerStr, isStr := innerRaw.(string); isStr {
				if callerKeepsSharp(o.KeepSharp, KeepSharpBlock{Kind: "tool_result", Text: innerStr, ToolUseID: toolUseID}) {
					bumpPassthrough(info, "kept_sharp")
					info.KeptSharpBlocks++
					rewritten = append(rewritten, bv)
					continue
				}
				inner := compactSlabWhitespace(innerStr)
				innerR := maybeReflow(inner, o.Reflow)
				if u16len(innerR) < o.MinToolResultChars {
					bumpPassthrough(info, "below_threshold")
					rewritten = append(rewritten, bv)
					continue
				}
				if !isCompressionProfitable(innerR, denseGeo.cols, o.MaxImagesPerToolResult, o.CharsPerToken, 0, 0, true, denseGeo.maxChars, denseGeo) {
					bumpPassthrough(info, "not_profitable")
					rewritten = append(rewritten, bv)
					continue
				}
				paged := truncateForBudget(innerR, o.MaxImagesPerToolResult, denseGeo.cols, denseGeo.maxChars, linesPerImage)
				if paged.truncated {
					info.TruncatedToolResults++
					info.OmittedChars += paged.omittedChars
				}
				rb, err := textToImageBlocks(paged.text, o.Cols, true, denseGeo.style, denseGeo.maxHeightPx)
				if err != nil {
					return err
				}
				info.ImagePNGs = append(info.ImagePNGs, rb.pngs...)
				info.ImageDims = append(info.ImageDims, rb.dims...)
				for _, img := range rb.blocks {
					info.ImageBytes += approxBlockBytes(img)
				}
				info.ImagePixels += rb.pixels
				info.ToolResultImgs += len(rb.blocks)
				info.ImageCount += len(rb.blocks)
				recordRecoverable(info, o.EmitRecoverable, "tool_result", toolUseID, innerStr, len(rb.blocks))
				info.CompressedChars += u16len(innerStr)
				info.DroppedChars += rb.droppedChars
				for cp, n := range rb.droppedCodepoints {
					droppedCodepoints[cp] += n
				}
				newContent := make([]any, 0, len(rb.blocks)+1)
				for _, blk := range rb.blocks {
					newContent = append(newContent, blk)
				}
				if trFactSheet := FactSheetText(innerStr); trFactSheet != "" {
					newContent = append(newContent, textBlock(trFactSheet))
				}
				ntr := cloneMap(tr)
				ntr["content"] = newContent
				rewritten = append(rewritten, ntr)
				changed = true
				bumpBucket(info, toolResultBucket(classifyContent(inner)), u16len(innerStr))
				continue
			}
			if innerArr, isArr := asArr(innerRaw); isArr {
				var newInner []any
				innerChanged := false
				for _, iv := range innerArr {
					ib, isMap := asMap(iv)
					isTextBlock := isMap && blockType(iv) == "text"
					var innerTextRaw string
					if isTextBlock {
						innerTextRaw, isTextBlock = getStr(ib, "text")
					}
					if !isTextBlock {
						newInner = append(newInner, iv)
						continue
					}
					if callerKeepsSharp(o.KeepSharp, KeepSharpBlock{Kind: "tool_result_part", Text: innerTextRaw, ToolUseID: toolUseID}) {
						bumpPassthrough(info, "kept_sharp")
						info.KeptSharpBlocks++
						newInner = append(newInner, iv)
						continue
					}
					innerText := compactSlabWhitespace(innerTextRaw)
					innerTextR := maybeReflow(innerText, o.Reflow)
					if u16len(innerTextR) < o.MinToolResultChars {
						bumpPassthrough(info, "below_threshold")
						newInner = append(newInner, iv)
						continue
					}
					if !isCompressionProfitable(innerTextR, denseGeo.cols, o.MaxImagesPerToolResult, o.CharsPerToken, 0, 0, true, denseGeo.maxChars, denseGeo) {
						bumpPassthrough(info, "not_profitable")
						newInner = append(newInner, iv)
						continue
					}
					paged := truncateForBudget(innerTextR, o.MaxImagesPerToolResult, denseGeo.cols, denseGeo.maxChars, linesPerImage)
					if paged.truncated {
						info.TruncatedToolResults++
						info.OmittedChars += paged.omittedChars
					}
					rb, err := textToImageBlocks(paged.text, o.Cols, true, denseGeo.style, denseGeo.maxHeightPx)
					if err != nil {
						return err
					}
					info.ImagePNGs = append(info.ImagePNGs, rb.pngs...)
					info.ImageDims = append(info.ImageDims, rb.dims...)
					srcCC, hasSrcCC := ib["cache_control"]
					if hasSrcCC {
						srcCC = demoteRelocatedCacheControl(srcCC)
					}
					for k, img := range rb.blocks {
						blk := img
						if k == len(rb.blocks)-1 && hasSrcCC {
							blk = cloneMap(img)
							blk["cache_control"] = srcCC
						}
						newInner = append(newInner, blk)
						info.ImageBytes += approxBlockBytes(img)
					}
					if partFactSheet := FactSheetText(innerTextRaw); partFactSheet != "" {
						newInner = append(newInner, textBlock(partFactSheet))
					}
					info.ImagePixels += rb.pixels
					info.ToolResultImgs += len(rb.blocks)
					info.ImageCount += len(rb.blocks)
					recordRecoverable(info, o.EmitRecoverable, "tool_result_part", toolUseID, innerTextRaw, len(rb.blocks))
					info.CompressedChars += u16len(innerTextRaw)
					info.DroppedChars += rb.droppedChars
					for cp, n := range rb.droppedCodepoints {
						droppedCodepoints[cp] += n
					}
					bumpBucket(info, toolResultBucket(classifyContent(innerText)), u16len(innerTextRaw))
					innerChanged = true
				}
				if innerChanged {
					ntr := cloneMap(tr)
					ntr["content"] = newInner
					rewritten = append(rewritten, ntr)
					changed = true
				} else {
					rewritten = append(rewritten, bv)
				}
				continue
			}
			rewritten = append(rewritten, bv)
		}
		if changed {
			m["content"] = rewritten
		}
	}
	return nil
}
