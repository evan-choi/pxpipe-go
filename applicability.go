package pxpipe

import (
	"os"
	"regexp"
	"strings"
	"sync"
)

// Model scope: which models pxpipe may transform. Resolution order per call —
// runtime override (SetAllowedModelBases) → PXPIPE_MODELS env CSV → built-in
// default. This Go port transforms the Anthropic Messages surface only, so
// only Claude-profile models render; the scope semantics still mirror TS.

var defaultModelBases = []string{"claude-fable-5", "gemini-3.6-flash"}

var (
	runtimeModelBasesMu sync.RWMutex
	runtimeModelBases   []string
	hasRuntimeOverride  bool
)

func falsey(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off", "none":
		return true
	}
	return false
}

func envOrDefaultBases() []string {
	raw, ok := os.LookupEnv("PXPIPE_MODELS")
	if !ok {
		return append([]string(nil), defaultModelBases...)
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return append([]string(nil), defaultModelBases...)
	}
	if falsey(trimmed) {
		return nil
	}
	var out []string
	for _, s := range strings.Split(trimmed, ",") {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func allowedModelBases() []string {
	runtimeModelBasesMu.RLock()
	defer runtimeModelBasesMu.RUnlock()
	if hasRuntimeOverride {
		return append([]string(nil), runtimeModelBases...)
	}
	return envOrDefaultBases()
}

// GetAllowedModelBases returns the current effective allowed-model scope.
func GetAllowedModelBases() []string { return allowedModelBases() }

// SetAllowedModelBases sets a runtime override; nil clears it, an empty slice
// compresses nothing.
func SetAllowedModelBases(list []string) {
	runtimeModelBasesMu.Lock()
	defer runtimeModelBasesMu.Unlock()
	if list == nil {
		hasRuntimeOverride = false
		runtimeModelBases = nil
		return
	}
	hasRuntimeOverride = true
	runtimeModelBases = runtimeModelBases[:0]
	for _, s := range list {
		if t := strings.TrimSpace(s); t != "" {
			runtimeModelBases = append(runtimeModelBases, t)
		}
	}
}

func unqualifiedModelID(base string) (string, bool) {
	slash := strings.LastIndexByte(base, '/')
	if slash < 0 {
		return "", false
	}
	return base[slash+1:], true
}

func isAllowed(model string) bool {
	if model == "" {
		return false
	}
	base := strings.ToLower(variantTagRe.ReplaceAllString(model, ""))
	unqualified, hasUnqualified := unqualifiedModelID(base)
	hit := func(id, target string) bool {
		return id == target || strings.HasPrefix(id, target+"-")
	}
	for _, b := range allowedModelBases() {
		target := strings.ToLower(b)
		if hit(base, target) || (hasUnqualified && hit(unqualified, target)) {
			return true
		}
	}
	return false
}

// IsSupportedModel reports whether pxpipe may transform this Anthropic model.
func IsSupportedModel(model string) bool { return isAllowed(model) }

// IsSupportedGptModel reports whether pxpipe may transform this model on the
// OpenAI Chat Completions / Responses surface (same allowlist scope).
func IsSupportedGptModel(model string) bool { return isAllowed(model) }

var providerSegmentChatRe = regexp.MustCompile(`^(?:/[a-z0-9][a-z0-9._-]*)?(?:/v1)?/chat/completions$`)
var providerSegmentResponsesRe = regexp.MustCompile(`^(?:/[a-z0-9][a-z0-9._-]*)?(?:/v1)?/responses$`)

// IsOpenAIChatPath matches OpenAI Chat Completions wire paths, including one
// optional gateway/provider segment (mirrors OPENAI_CHAT_PATH in proxy.ts).
func IsOpenAIChatPath(pathname string) bool {
	return providerSegmentChatRe.MatchString(pathname)
}

// IsOpenAIResponsesPath matches OpenAI Responses wire paths (mirrors
// OPENAI_RESPONSES_PATH in proxy.ts).
func IsOpenAIResponsesPath(pathname string) bool {
	return providerSegmentResponsesRe.MatchString(pathname)
}

// IsAnthropicMessagesPath matches exactly the Messages routes pxpipe
// transforms (count_tokens excluded).
func IsAnthropicMessagesPath(pathname string) bool {
	return pathname == "/v1/messages" ||
		pathname == "/anthropic/v1/messages" ||
		pathname == "/anthropic/messages"
}
