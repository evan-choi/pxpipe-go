package pxpipe

import (
	"slices"
	"testing"
)

func TestConfiguredModelBasesIgnoreRuntimeOverride(t *testing.T) {
	t.Setenv("PXPIPE_MODELS", "claude-opus-5, gpt-5.6-sol")
	SetAllowedModelBases([]string{"gpt-5.4"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })

	if got, want := GetConfiguredModelBases(), []string{"claude-opus-5", "gpt-5.6-sol"}; !slices.Equal(got, want) {
		t.Fatalf("GetConfiguredModelBases() = %v, want %v", got, want)
	}
	if got, want := GetAllowedModelBases(), []string{"gpt-5.4"}; !slices.Equal(got, want) {
		t.Fatalf("GetAllowedModelBases() = %v, want %v", got, want)
	}
}

func TestSupportedModelRejectsMisresolvedSibling(t *testing.T) {
	SetAllowedModelBases([]string{"gemini-3.6-flash"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })

	if !IsSupportedModel("google/gemini-3.6-flash") {
		t.Fatal("measured qualified Gemini model must remain supported")
	}
	if IsSupportedModel("gemini-3.6-flash-preview") {
		t.Fatal("unmeasured Gemini sibling must fail closed")
	}
}

func TestShouldTransformAnthropicMessages(t *testing.T) {
	SetAllowedModelBases([]string{"claude-fable-5"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })
	empty := int64(0)

	tests := []struct {
		name string
		in   ApplicabilityInput
		want ApplicabilityResult
	}{
		{
			name: "eligible",
			in:   ApplicabilityInput{Model: "claude-fable-5-20260801", Method: "post", Path: "/v1/messages"},
			want: ApplicabilityResult{Eligible: true, Reason: ApplicabilityReasonEligible},
		},
		{
			name: "unsupported method",
			in:   ApplicabilityInput{Model: "claude-fable-5", Method: "GET"},
			want: ApplicabilityResult{Reason: ApplicabilityReasonUnsupportedMethod},
		},
		{
			name: "unsupported path",
			in:   ApplicabilityInput{Model: "claude-fable-5", Path: "/v1/messages/count_tokens"},
			want: ApplicabilityResult{Reason: ApplicabilityReasonUnsupportedPath},
		},
		{
			name: "empty body",
			in:   ApplicabilityInput{Model: "claude-fable-5", BodyBytes: &empty},
			want: ApplicabilityResult{Reason: ApplicabilityReasonEmptyBody},
		},
		{
			name: "unsupported model",
			in:   ApplicabilityInput{Model: "claude-opus-5"},
			want: ApplicabilityResult{Reason: ApplicabilityReasonUnsupportedModel},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldTransformAnthropicMessages(tt.in); got != tt.want {
				t.Fatalf("ShouldTransformAnthropicMessages() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
