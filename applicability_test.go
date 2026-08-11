package pxpipe

import (
	"slices"
	"sync"
	"testing"
)

func TestConfiguredModelBasesIgnoreRuntimeOverride(t *testing.T) {
	t.Setenv("PXPIPE_MODELS", "claude-opus-5, gpt-5.6-sol")
	SetAllowedModelBases([]string{"gpt-5.4"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })

	configured := GetConfiguredModelBases()
	if want := []string{"claude-opus-5", "gpt-5.6-sol"}; !slices.Equal(configured, want) {
		t.Fatalf("GetConfiguredModelBases() = %v, want %v", configured, want)
	}
	configured[0] = "mutated"
	if got, want := GetConfiguredModelBases(), []string{"claude-opus-5", "gpt-5.6-sol"}; !slices.Equal(got, want) {
		t.Fatalf("GetConfiguredModelBases() = %v, want %v", got, want)
	}
	allowed := GetAllowedModelBases()
	if want := []string{"gpt-5.4"}; !slices.Equal(allowed, want) {
		t.Fatalf("GetAllowedModelBases() = %v, want %v", allowed, want)
	}
	allowed[0] = "mutated"
	if got, want := GetAllowedModelBases(), []string{"gpt-5.4"}; !slices.Equal(got, want) {
		t.Fatalf("GetAllowedModelBases() = %v, want %v", got, want)
	}

	t.Setenv("PXPIPE_MODELS", "gemini-3.6-flash")
	if got, want := GetConfiguredModelBases(), []string{"gemini-3.6-flash"}; !slices.Equal(got, want) {
		t.Fatalf("refreshed GetConfiguredModelBases() = %v, want %v", got, want)
	}
	SetAllowedModelBases(nil)
	if got, want := GetAllowedModelBases(), []string{"gemini-3.6-flash"}; !slices.Equal(got, want) {
		t.Fatalf("refreshed GetAllowedModelBases() = %v, want %v", got, want)
	}
}

func TestAllowedModelBasesConcurrentAccess(t *testing.T) {
	t.Setenv("PXPIPE_MODELS", "gpt-5.4")
	SetAllowedModelBases(nil)
	t.Cleanup(func() { SetAllowedModelBases(nil) })

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 1_000 {
				if !IsSupportedGptModel("gpt-5.4") {
					t.Error("gpt-5.4 must remain allowed")
				}
			}
		})
	}
	wg.Go(func() {
		for range 1_000 {
			SetAllowedModelBases([]string{"gpt-5.4"})
			SetAllowedModelBases(nil)
		}
	})
	wg.Wait()
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

func TestSupportedModelWildcardAllowsAnthropicAndOpenAI(t *testing.T) {
	SetAllowedModelBases([]string{"*"})
	t.Cleanup(func() { SetAllowedModelBases(nil) })

	if !IsSupportedModel("claude-opus-5") || !IsSupportedGptModel("gpt-5.6-sol") {
		t.Fatal("wildcard must allow Anthropic and OpenAI models")
	}
	if IsSupportedModel("gemini-3.6-flash-preview") {
		t.Fatal("wildcard must not admit a misresolved model")
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
