package pxpipe

import "strings"

// Reason classifies a TransformAnthropicMessages outcome.
type Reason string

const (
	ReasonApplied          Reason = "applied"
	ReasonUnsupportedModel Reason = "unsupported_model"
	ReasonParseError       Reason = "parse_error"
	ReasonBelowMinChars    Reason = "below_min_chars"
	ReasonBelowMinTokens   Reason = "below_min_tokens"
	ReasonNotProfitable    Reason = "not_profitable"
	ReasonCompressDisabled Reason = "compress_disabled"
	ReasonImageLimit       Reason = "image_limit"
	ReasonTransformError   Reason = "transform_error"
	ReasonPassthrough      Reason = "passthrough"
)

// TransformInput is the library-level input for one Anthropic Messages body.
type TransformInput struct {
	Body    []byte
	Model   string
	Options *TransformOptions
}

// TransformResult is the library-level outcome, including cache_control
// ownership so hosts do not stack a second marker injector.
type TransformResult struct {
	Body    []byte
	Applied bool
	Reason  Reason
	Detail  string
	Info    *TransformInfo
	Cache   struct {
		OwnsCacheControl bool
		MarkerCount      int
	}
}

func classifyReason(info *TransformInfo) Reason {
	if info.Compressed {
		return ReasonApplied
	}
	r := info.Reason
	switch {
	case strings.HasPrefix(r, "parse_error"):
		return ReasonParseError
	case strings.HasPrefix(r, "compress=false"):
		return ReasonCompressDisabled
	case strings.HasPrefix(r, "below_min_chars"):
		return ReasonBelowMinChars
	case strings.HasPrefix(r, "below_min_tokens"):
		return ReasonBelowMinTokens
	case strings.HasPrefix(r, "not_profitable"):
		return ReasonNotProfitable
	case strings.HasPrefix(r, "transform_error"):
		return ReasonTransformError
	case strings.Contains(r, "image") && strings.Contains(r, "limit"):
		return ReasonImageLimit
	}
	return ReasonPassthrough
}

// CountCacheControlMarkers counts cache_control markers anywhere in a Messages
// body.
func CountCacheControlMarkers(body []byte) int {
	var v any
	if err := jsonUnmarshal(body, &v); err != nil {
		return 0
	}
	return countCacheControlValue(v)
}

func countCacheControlValue(v any) int {
	n := 0
	switch tv := v.(type) {
	case map[string]any:
		if cc, has := tv["cache_control"]; has && cc != nil {
			n++
		}
		for _, item := range tv {
			n += countCacheControlValue(item)
		}
	case []any:
		for _, item := range tv {
			n += countCacheControlValue(item)
		}
	}
	return n
}

// TransformAnthropicMessages is the model-gated library entry mirroring the TS
// transformAnthropicMessages wrapper.
func TransformAnthropicMessages(input TransformInput) *TransformResult {
	res := &TransformResult{}
	if !IsSupportedModel(input.Model) {
		res.Body = input.Body
		res.Reason = ReasonUnsupportedModel
		res.Detail = input.Model
		res.Info = &TransformInfo{Reason: "unsupported_model"}
		res.Cache.MarkerCount = CountCacheControlMarkers(input.Body)
		return res
	}
	opts := input.Options
	if opts == nil {
		opts = &TransformOptions{}
	}
	optsCopy := *opts
	optsCopy.Model = input.Model
	body, info := TransformRequest(input.Body, &optsCopy)
	res.Body = body
	res.Applied = info.Compressed
	res.Reason = classifyReason(info)
	res.Detail = info.Reason
	res.Info = info
	markerCount := CountCacheControlMarkers(body)
	res.Cache.MarkerCount = markerCount
	res.Cache.OwnsCacheControl = info.Compressed && markerCount > 0
	return res
}
