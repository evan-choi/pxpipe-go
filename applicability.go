package pxpipe

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// Model scope: which models pxpipe may transform. Resolution order per call —
// runtime override (SetAllowedModelBases) → PXPIPE_MODELS env CSV → built-in
// default. Anthropic and OpenAI surfaces share this scope.

var defaultModelBases = []string{"claude-fable-5", "gemini-3.6-flash"}

var (
	runtimeModelBases         atomic.Pointer[modelBasesSnapshot]
	configuredModelBasesMu    sync.Mutex
	configuredModelBasesCache atomic.Pointer[configuredModelBasesSnapshot]
)

type modelBasesSnapshot struct {
	bases []string
}

type configuredModelBasesSnapshot struct {
	raw     string
	present bool
	bases   []string
}

func falsey(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off", "none":
		return true
	}
	return false
}

func parseConfiguredModelBases(raw string, present bool) []string {
	if !present {
		return defaultModelBases
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultModelBases
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

func configuredModelBases() []string {
	raw, present := os.LookupEnv("PXPIPE_MODELS")
	if cached := configuredModelBasesCache.Load(); cached != nil && cached.raw == raw && cached.present == present {
		return cached.bases
	}
	configuredModelBasesMu.Lock()
	defer configuredModelBasesMu.Unlock()
	if cached := configuredModelBasesCache.Load(); cached != nil && cached.raw == raw && cached.present == present {
		return cached.bases
	}
	bases := parseConfiguredModelBases(raw, present)
	configuredModelBasesCache.Store(&configuredModelBasesSnapshot{raw: raw, present: present, bases: bases})
	return bases
}

func envOrDefaultBases() []string { return append([]string(nil), configuredModelBases()...) }

func allowedModelBasesView() []string {
	if snapshot := runtimeModelBases.Load(); snapshot != nil {
		return snapshot.bases
	}
	return configuredModelBases()
}

func allowedModelBases() []string { return append([]string(nil), allowedModelBasesView()...) }

// GetAllowedModelBases returns the current effective allowed-model scope.
func GetAllowedModelBases() []string { return allowedModelBases() }

// GetConfiguredModelBases returns the PXPIPE_MODELS/default scope, ignoring
// the runtime override.
func GetConfiguredModelBases() []string { return envOrDefaultBases() }

// SetAllowedModelBases sets a runtime override; nil clears it, an empty slice
// compresses nothing.
func SetAllowedModelBases(list []string) {
	if list == nil {
		runtimeModelBases.Store(nil)
		return
	}
	bases := make([]string, 0, len(list))
	for _, s := range list {
		if t := strings.TrimSpace(s); t != "" {
			bases = append(bases, t)
		}
	}
	runtimeModelBases.Store(&modelBasesSnapshot{bases: bases})
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
	base := stripBracketVariants(strings.ToLower(model))
	if IsMisresolvedModelId(base) {
		return false
	}
	unqualified, hasUnqualified := unqualifiedModelID(base)
	hit := func(id, target string) bool {
		return id == target || strings.HasPrefix(id, target+"-")
	}
	for _, b := range allowedModelBasesView() {
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

// ApplicabilityReason explains why an Anthropic Messages request is or is not
// eligible for transformation.
type ApplicabilityReason string

const (
	ApplicabilityReasonEligible          ApplicabilityReason = "eligible"
	ApplicabilityReasonUnsupportedModel  ApplicabilityReason = "unsupported_model"
	ApplicabilityReasonUnsupportedMethod ApplicabilityReason = "unsupported_method"
	ApplicabilityReasonUnsupportedPath   ApplicabilityReason = "unsupported_path"
	ApplicabilityReasonEmptyBody         ApplicabilityReason = "empty_body"
)

// ApplicabilityInput describes the request fields used by the public
// Anthropic Messages eligibility check. Empty Method or Path skips that check;
// nil BodyBytes means its size is unknown.
type ApplicabilityInput struct {
	Model     string
	Method    string
	Path      string
	BodyBytes *int64
}

// ApplicabilityResult is the outcome of ShouldTransformAnthropicMessages.
type ApplicabilityResult struct {
	Eligible bool
	Reason   ApplicabilityReason
}

// ShouldTransformAnthropicMessages reports whether an Anthropic Messages
// request is eligible for transformation.
func ShouldTransformAnthropicMessages(input ApplicabilityInput) ApplicabilityResult {
	if input.Method != "" && !strings.EqualFold(input.Method, "POST") {
		return ApplicabilityResult{Reason: ApplicabilityReasonUnsupportedMethod}
	}
	if input.Path != "" && !IsAnthropicMessagesPath(input.Path) {
		return ApplicabilityResult{Reason: ApplicabilityReasonUnsupportedPath}
	}
	if input.BodyBytes != nil && *input.BodyBytes <= 0 {
		return ApplicabilityResult{Reason: ApplicabilityReasonEmptyBody}
	}
	if !IsSupportedModel(input.Model) {
		return ApplicabilityResult{Reason: ApplicabilityReasonUnsupportedModel}
	}
	return ApplicabilityResult{Eligible: true, Reason: ApplicabilityReasonEligible}
}
